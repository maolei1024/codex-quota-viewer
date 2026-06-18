package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	defaultListenAddr      = ":8080"
	defaultDataDir         = "/data"
	defaultStaleAfter      = 30 * time.Minute
	defaultRefreshInterval = 5 * time.Minute
)

type config struct {
	ListenAddr string
	DataDir    string
	StaleAfter time.Duration
	Refresh    time.Duration
}

type rawAccount struct {
	Email          string          `json:"email"`
	AuthMode       string          `json:"auth_mode"`
	PlanType       string          `json:"plan_type"`
	Quota          *rawQuota       `json:"quota"`
	QuotaError     *rawQuotaError  `json:"quota_error"`
	UsageUpdatedAt *int64          `json:"usage_updated_at"`
	Raw            json.RawMessage `json:"-"`
}

type rawQuota struct {
	HourlyPercentage    int    `json:"hourly_percentage"`
	HourlyResetTime     *int64 `json:"hourly_reset_time"`
	HourlyWindowMinutes *int64 `json:"hourly_window_minutes"`
	HourlyWindowPresent *bool  `json:"hourly_window_present"`
	WeeklyPercentage    int    `json:"weekly_percentage"`
	WeeklyResetTime     *int64 `json:"weekly_reset_time"`
	WeeklyWindowMinutes *int64 `json:"weekly_window_minutes"`
	WeeklyWindowPresent *bool  `json:"weekly_window_present"`
}

type rawQuotaError struct {
	Code      *string `json:"code"`
	Message   string  `json:"message"`
	Timestamp int64   `json:"timestamp"`
}

type accountView struct {
	Email             string      `json:"email"`
	AuthMode          string      `json:"authMode"`
	PlanType          string      `json:"planType"`
	Hourly            quotaWindow `json:"hourly"`
	Weekly            quotaWindow `json:"weekly"`
	UsageUpdatedAt    *int64      `json:"usageUpdatedAt,omitempty"`
	UsageUpdatedLabel string      `json:"usageUpdatedLabel"`
	Stale             bool        `json:"stale"`
	Error             string      `json:"error,omitempty"`
}

type quotaWindow struct {
	Present    bool   `json:"present"`
	Remaining  int    `json:"remaining"`
	ResetAt    *int64 `json:"resetAt,omitempty"`
	ResetLabel string `json:"resetLabel"`
	Window     string `json:"window"`
	Class      string `json:"class"`
}

type usageTotals struct {
	RequestCount                          int64   `json:"requestCount"`
	SuccessCount                          int64   `json:"successCount"`
	FailureCount                          int64   `json:"failureCount"`
	ClientCanceledCount                   int64   `json:"clientCanceledCount"`
	UpstreamResponseFailedCount           int64   `json:"upstreamResponseFailedCount"`
	StreamIncompleteCount                 int64   `json:"streamIncompleteCount"`
	TotalLatencyMs                        int64   `json:"totalLatencyMs"`
	TextRequestCount                      int64   `json:"textRequestCount"`
	ImageRequestCount                     int64   `json:"imageRequestCount"`
	ImageGenerationRequestCount           int64   `json:"imageGenerationRequestCount"`
	ImageEditRequestCount                 int64   `json:"imageEditRequestCount"`
	ImageGenerationCapabilityFailureCount int64   `json:"imageGenerationCapabilityFailureCount"`
	InputTokens                           int64   `json:"inputTokens"`
	OutputTokens                          int64   `json:"outputTokens"`
	TotalTokens                           int64   `json:"totalTokens"`
	CachedTokens                          int64   `json:"cachedTokens"`
	ReasoningTokens                       int64   `json:"reasoningTokens"`
	EstimatedCostUSD                      float64 `json:"estimatedCostUsd"`
}

type modelUsage struct {
	ModelID  string         `json:"modelId"`
	Usage    usageTotals    `json:"usage"`
	Accounts []accountUsage `json:"accounts,omitempty"`
}

type accountUsage struct {
	Account string      `json:"account"`
	Usage   usageTotals `json:"usage"`
}

type usageView struct {
	Available      bool         `json:"available"`
	Source         string       `json:"source"`
	Since          int64        `json:"since,omitempty"`
	SinceLabel     string       `json:"sinceLabel,omitempty"`
	UpdatedAt      int64        `json:"updatedAt,omitempty"`
	UpdatedLabel   string       `json:"updatedLabel,omitempty"`
	Daily          usageTotals  `json:"daily"`
	Weekly         usageTotals  `json:"weekly"`
	Monthly        usageTotals  `json:"monthly"`
	Models         []modelUsage `json:"models"`
	Error          string       `json:"error,omitempty"`
	BreakdownSince int64        `json:"-"`
}

type summaryView struct {
	GeneratedAt      int64         `json:"generatedAt"`
	GeneratedLabel   string        `json:"generatedLabel"`
	AccountCount     int           `json:"accountCount"`
	StaleCount       int           `json:"staleCount"`
	ErrorCount       int           `json:"errorCount"`
	LowestHourly     *int          `json:"lowestHourly,omitempty"`
	LowestWeekly     *int          `json:"lowestWeekly,omitempty"`
	RefreshSeconds   int           `json:"refreshSeconds"`
	RefreshLabel     string        `json:"refreshLabel"`
	MaxModelRequests int64         `json:"maxModelRequests"`
	Accounts         []accountView `json:"accounts"`
	LocalAccessUsage usageView     `json:"localAccessUsage"`
}

type rawStatsFile struct {
	Since     int64          `json:"since"`
	UpdatedAt int64          `json:"updatedAt"`
	Daily     rawStatsWindow `json:"daily"`
	Weekly    rawStatsWindow `json:"weekly"`
	Monthly   rawStatsWindow `json:"monthly"`
}

type rawStatsWindow struct {
	Since     int64           `json:"since"`
	UpdatedAt int64           `json:"updatedAt"`
	Totals    usageTotals     `json:"totals"`
	Models    []rawModelStats `json:"models"`
}

type rawModelStats struct {
	ModelID string      `json:"modelId"`
	Usage   usageTotals `json:"usage"`
}

func main() {
	cfg := loadConfig()
	app := &server{cfg: cfg, tmpl: template.Must(template.New("dashboard").Funcs(dashboardFuncs()).Parse(dashboardHTML))}

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.handleDashboard)
	mux.HandleFunc("/api/summary", app.handleSummary)
	mux.HandleFunc("/api/accounts", app.handleAccounts)
	mux.HandleFunc("/api/local-access/usage", app.handleUsage)
	mux.HandleFunc("/healthz", app.handleHealthz)

	log.Printf("codex-quota-viewer listening on %s, data dir %s", cfg.ListenAddr, cfg.DataDir)
	if err := http.ListenAndServe(cfg.ListenAddr, securityHeaders(mux)); err != nil {
		log.Fatal(err)
	}
}

func dashboardFuncs() template.FuncMap {
	return template.FuncMap{
		"percent":        percentLabel,
		"tokens":         compactInt,
		"cost":           costLabel,
		"statusClass":    statusClass,
		"successRate":    successRateLabel,
		"failureRate":    failureRateLabel,
		"avgLatency":     avgLatencyLabel,
		"barWidth":       barWidth,
		"failurePercent": failurePercent,
	}
}

type server struct {
	cfg  config
	tmpl *template.Template
}

func loadConfig() config {
	listenAddr := strings.TrimSpace(os.Getenv("LISTEN_ADDR"))
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}
	dataDir := strings.TrimSpace(os.Getenv("DATA_DIR"))
	if dataDir == "" {
		dataDir = defaultDataDir
	}
	staleAfter := defaultStaleAfter
	if raw := strings.TrimSpace(os.Getenv("STALE_AFTER_MINUTES")); raw != "" {
		if minutes, err := strconv.Atoi(raw); err == nil && minutes > 0 {
			staleAfter = time.Duration(minutes) * time.Minute
		}
	}
	refresh := defaultRefreshInterval
	if raw := strings.TrimSpace(os.Getenv("REFRESH_INTERVAL_SECONDS")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
			refresh = time.Duration(seconds) * time.Second
		}
	}
	return config{ListenAddr: listenAddr, DataDir: filepath.Clean(dataDir), StaleAfter: staleAfter, Refresh: refresh}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline' 'self'; script-src 'unsafe-inline' 'self'")
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data := s.buildSummary()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, data); err != nil {
		log.Printf("render dashboard: %v", err)
	}
}

func (s *server) handleSummary(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.buildSummary())
}

func (s *server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := loadAccounts(s.cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, accounts)
}

func (s *server) handleUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, loadUsage(s.cfg.DataDir))
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func (s *server) buildSummary() summaryView {
	now := time.Now()
	accounts, err := loadAccounts(s.cfg)
	if err != nil {
		accounts = []accountView{{
			Email:             "load-error",
			PlanType:          "-",
			UsageUpdatedLabel: "-",
			Error:             err.Error(),
		}}
	}
	usage := loadUsage(s.cfg.DataDir)
	summary := summaryView{
		GeneratedAt:      now.Unix(),
		GeneratedLabel:   formatTime(now.Unix()),
		AccountCount:     len(accounts),
		RefreshSeconds:   int(s.cfg.Refresh.Seconds()),
		RefreshLabel:     durationLabel(s.cfg.Refresh),
		Accounts:         accounts,
		LocalAccessUsage: usage,
	}
	for _, account := range accounts {
		if account.Stale {
			summary.StaleCount++
		}
		if account.Error != "" {
			summary.ErrorCount++
		}
		if account.Hourly.Present {
			value := account.Hourly.Remaining
			if summary.LowestHourly == nil || value < *summary.LowestHourly {
				summary.LowestHourly = &value
			}
		}
		if account.Weekly.Present {
			value := account.Weekly.Remaining
			if summary.LowestWeekly == nil || value < *summary.LowestWeekly {
				summary.LowestWeekly = &value
			}
		}
	}
	for _, model := range usage.Models {
		if model.Usage.RequestCount > summary.MaxModelRequests {
			summary.MaxModelRequests = model.Usage.RequestCount
		}
	}
	return summary
}

func loadAccounts(cfg config) ([]accountView, error) {
	dir := filepath.Join(cfg.DataDir, "codex_accounts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read accounts dir: %w", err)
	}

	accounts := make([]accountView, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			log.Printf("read account %s: %v", entry.Name(), err)
			continue
		}
		var account rawAccount
		if err := json.Unmarshal(raw, &account); err != nil {
			log.Printf("parse account %s: %v", entry.Name(), err)
			continue
		}
		accounts = append(accounts, account.toView(cfg.StaleAfter))
	}
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Stale != accounts[j].Stale {
			return accounts[i].Stale
		}
		return accounts[i].Email < accounts[j].Email
	})
	return accounts, nil
}

func (a rawAccount) toView(staleAfter time.Duration) accountView {
	now := time.Now()
	view := accountView{
		Email:             maskIdentity(a.Email),
		AuthMode:          dash(a.AuthMode),
		PlanType:          dash(a.PlanType),
		UsageUpdatedAt:    a.UsageUpdatedAt,
		UsageUpdatedLabel: "-",
	}
	if a.UsageUpdatedAt != nil {
		view.UsageUpdatedLabel = formatTime(*a.UsageUpdatedAt)
		view.Stale = now.Sub(time.Unix(*a.UsageUpdatedAt, 0)) > staleAfter
	}
	if a.Quota == nil {
		view.Hourly = missingWindow()
		view.Weekly = missingWindow()
	} else {
		view.Hourly = buildWindow(a.Quota.HourlyWindowPresent, a.Quota.HourlyPercentage, a.Quota.HourlyResetTime, a.Quota.HourlyWindowMinutes)
		view.Weekly = buildWindow(a.Quota.WeeklyWindowPresent, a.Quota.WeeklyPercentage, a.Quota.WeeklyResetTime, a.Quota.WeeklyWindowMinutes)
	}
	if a.QuotaError != nil {
		if a.QuotaError.Code != nil && *a.QuotaError.Code != "" {
			view.Error = *a.QuotaError.Code + ": " + a.QuotaError.Message
		} else {
			view.Error = a.QuotaError.Message
		}
	}
	return view
}

func buildWindow(present *bool, remaining int, resetAt *int64, windowMinutes *int64) quotaWindow {
	isPresent := true
	if present != nil {
		isPresent = *present
	}
	if !isPresent {
		return missingWindow()
	}
	remaining = clamp(remaining, 0, 100)
	window := "-"
	if windowMinutes != nil && *windowMinutes > 0 {
		window = fmt.Sprintf("%dm", *windowMinutes)
		if *windowMinutes%60 == 0 {
			window = fmt.Sprintf("%dh", *windowMinutes/60)
		}
	}
	return quotaWindow{
		Present:    true,
		Remaining:  remaining,
		ResetAt:    resetAt,
		ResetLabel: formatOptionalTime(resetAt),
		Window:     window,
		Class:      quotaClass(remaining),
	}
}

func missingWindow() quotaWindow {
	return quotaWindow{Present: false, Remaining: 0, ResetLabel: "-", Window: "-", Class: "unknown"}
}

func loadUsage(dataDir string) usageView {
	statsPath := filepath.Join(dataDir, "codex_local_access_stats.json")
	if usage, ok := loadUsageFromStatsFile(statsPath); ok {
		if usage.Available {
			sqlitePath := filepath.Join(dataDir, "codex_local_access_logs.sqlite")
			usage.Models = attachModelAccountUsageFromSQLite(sqlitePath, usage.BreakdownSince, usage.Models)
		}
		return usage
	}
	return loadUsageFromSQLite(filepath.Join(dataDir, "codex_local_access_logs.sqlite"))
}

func loadUsageFromStatsFile(path string) (usageView, bool) {
	raw, err := os.ReadFile(path)
	if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
		return usageView{}, false
	}
	var stats rawStatsFile
	if err := json.Unmarshal(raw, &stats); err != nil {
		return usageView{Available: false, Source: "stats-json", Error: "stats json parse failed"}, true
	}
	models := make([]modelUsage, 0, len(stats.Monthly.Models))
	for _, model := range stats.Monthly.Models {
		models = append(models, modelUsage{ModelID: model.ModelID, Usage: model.Usage})
	}
	sortModels(models)
	breakdownSince := stats.Monthly.Since
	if breakdownSince <= 0 {
		breakdownSince = time.Now().Add(-30 * 24 * time.Hour).Unix()
	}
	return usageView{
		Available:      true,
		Source:         "stats-json",
		Since:          stats.Since,
		SinceLabel:     formatOptionalUnix(stats.Since),
		UpdatedAt:      stats.UpdatedAt,
		UpdatedLabel:   formatOptionalUnix(stats.UpdatedAt),
		Daily:          stats.Daily.Totals,
		Weekly:         stats.Weekly.Totals,
		Monthly:        stats.Monthly.Totals,
		Models:         models,
		BreakdownSince: breakdownSince,
	}, true
}

func loadUsageFromSQLite(path string) usageView {
	if _, err := os.Stat(path); err != nil {
		return usageView{Available: false, Source: "sqlite", Error: "no local access usage data"}
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return usageView{Available: false, Source: "sqlite", Error: "open sqlite failed"}
	}
	defer db.Close()

	now := time.Now()
	dailySince := now.Add(-24 * time.Hour).Unix()
	weeklySince := now.Add(-7 * 24 * time.Hour).Unix()
	monthlySince := now.Add(-30 * 24 * time.Hour).Unix()
	daily := queryUsageTotals(db, dailySince)
	weekly := queryUsageTotals(db, weeklySince)
	monthly := queryUsageTotals(db, monthlySince)
	models := queryModelUsage(db, monthlySince)
	return usageView{
		Available:      true,
		Source:         "sqlite",
		UpdatedAt:      now.Unix(),
		UpdatedLabel:   formatTime(now.Unix()),
		Daily:          daily,
		Weekly:         weekly,
		Monthly:        monthly,
		Models:         models,
		BreakdownSince: monthlySince,
	}
}

func queryUsageTotals(db *sql.DB, since int64) usageTotals {
	var totals usageTotals
	row := db.QueryRow(`
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(cached_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0),
			COALESCE(SUM(estimated_cost_usd), 0)
		FROM request_logs
		WHERE timestamp >= ?`, since)
	if err := row.Scan(
		&totals.RequestCount,
		&totals.SuccessCount,
		&totals.FailureCount,
		&totals.InputTokens,
		&totals.OutputTokens,
		&totals.TotalTokens,
		&totals.CachedTokens,
		&totals.ReasoningTokens,
		&totals.EstimatedCostUSD,
	); err != nil {
		log.Printf("query usage totals: %v", err)
	}
	return totals
}

func queryModelUsage(db *sql.DB, since int64) []modelUsage {
	includeAccounts := requestLogsHaveColumns(db, "account_id", "email")
	rows, err := db.Query(`
		SELECT
			COALESCE(NULLIF(model_id, ''), 'unknown') AS model_id,
			COUNT(*),
			COALESCE(SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(cached_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0),
			COALESCE(SUM(estimated_cost_usd), 0)
		FROM request_logs
		WHERE timestamp >= ?
		GROUP BY model_id
		ORDER BY COUNT(*) DESC
		LIMIT 12`, since)
	if err != nil {
		log.Printf("query model usage: %v", err)
		return nil
	}
	defer rows.Close()

	var models []modelUsage
	for rows.Next() {
		var model modelUsage
		if err := rows.Scan(
			&model.ModelID,
			&model.Usage.RequestCount,
			&model.Usage.SuccessCount,
			&model.Usage.FailureCount,
			&model.Usage.InputTokens,
			&model.Usage.OutputTokens,
			&model.Usage.TotalTokens,
			&model.Usage.CachedTokens,
			&model.Usage.ReasoningTokens,
			&model.Usage.EstimatedCostUSD,
		); err != nil {
			log.Printf("scan model usage: %v", err)
			continue
		}
		if includeAccounts {
			model.Accounts = queryModelAccountUsage(db, since, model.ModelID)
		}
		models = append(models, model)
	}
	return models
}

func attachModelAccountUsageFromSQLite(path string, since int64, models []modelUsage) []modelUsage {
	if len(models) == 0 {
		return models
	}
	if _, err := os.Stat(path); err != nil {
		return models
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		log.Printf("open sqlite for model account usage: %v", err)
		return models
	}
	defer db.Close()
	if !requestLogsHaveColumns(db, "account_id", "email") {
		return models
	}
	if since <= 0 {
		since = time.Now().Add(-30 * 24 * time.Hour).Unix()
	}
	for i := range models {
		models[i].Accounts = queryModelAccountUsage(db, since, models[i].ModelID)
	}
	return models
}

func queryModelAccountUsage(db *sql.DB, since int64, modelID string) []accountUsage {
	rows, err := db.Query(`
		SELECT
			COALESCE(MAX(NULLIF(email, '')), MAX(NULLIF(account_id, '')), 'unknown') AS account,
			COUNT(*),
			COALESCE(SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(cached_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0),
			COALESCE(SUM(estimated_cost_usd), 0)
		FROM request_logs
		WHERE timestamp >= ?
			AND COALESCE(NULLIF(model_id, ''), 'unknown') = ?
		GROUP BY COALESCE(NULLIF(account_id, ''), NULLIF(email, ''), 'unknown')
		ORDER BY COUNT(*) DESC, COALESCE(SUM(estimated_cost_usd), 0) DESC
		LIMIT 12`, since, modelID)
	if err != nil {
		log.Printf("query model account usage: %v", err)
		return nil
	}
	defer rows.Close()

	var accounts []accountUsage
	for rows.Next() {
		var account string
		var usage usageTotals
		if err := rows.Scan(
			&account,
			&usage.RequestCount,
			&usage.SuccessCount,
			&usage.FailureCount,
			&usage.InputTokens,
			&usage.OutputTokens,
			&usage.TotalTokens,
			&usage.CachedTokens,
			&usage.ReasoningTokens,
			&usage.EstimatedCostUSD,
		); err != nil {
			log.Printf("scan model account usage: %v", err)
			continue
		}
		accounts = append(accounts, accountUsage{Account: maskUsageAccount(account), Usage: usage})
	}
	return accounts
}

func requestLogsHaveColumns(db *sql.DB, columns ...string) bool {
	if len(columns) == 0 {
		return true
	}
	rows, err := db.Query("PRAGMA table_info(request_logs)")
	if err != nil {
		log.Printf("inspect request_logs columns: %v", err)
		return false
	}
	defer rows.Close()

	available := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			log.Printf("scan request_logs column: %v", err)
			return false
		}
		available[name] = true
	}
	for _, column := range columns {
		if !available[column] {
			return false
		}
	}
	return true
}

func sortModels(models []modelUsage) {
	sort.Slice(models, func(i, j int) bool {
		return models[i].Usage.RequestCount > models[j].Usage.RequestCount
	})
	if len(models) > 12 {
		models = models[:12]
	}
}

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(data); err != nil {
		log.Printf("write json: %v", err)
	}
}

func maskIdentity(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	if strings.HasPrefix(value, "api-key-") {
		return "api-key-****"
	}
	at := strings.Index(value, "@")
	if at < 0 {
		if len(value) <= 1 {
			return "*"
		}
		return string([]rune(value)[0]) + "***"
	}
	local := []rune(value[:at])
	first := "*"
	if len(local) > 0 {
		first = string(local[0])
	}
	domain := value[at+1:]
	suffix := ""
	parts := strings.Split(domain, ".")
	if len(parts) > 0 {
		last := parts[len(parts)-1]
		if last != "" {
			suffix = "." + last
		}
	}
	return first + "***@**" + suffix
}

func maskUsageAccount(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "unknown" {
		return "unknown"
	}
	return maskIdentity(value)
}

func dash(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	return value
}

func clamp(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func quotaClass(value int) string {
	switch {
	case value <= 20:
		return "danger"
	case value <= 50:
		return "warn"
	default:
		return "ok"
	}
}

func statusClass(summary summaryView) string {
	if summary.ErrorCount > 0 {
		return "danger"
	}
	if summary.StaleCount > 0 {
		return "warn"
	}
	return "ok"
}

func percentLabel(value *int) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprintf("%d%%", *value)
}

func compactInt(value int64) string {
	abs := math.Abs(float64(value))
	switch {
	case abs >= 1_000_000:
		return fmt.Sprintf("%.1fm", float64(value)/1_000_000)
	case abs >= 1_000:
		return fmt.Sprintf("%.1fk", float64(value)/1_000)
	default:
		return strconv.FormatInt(value, 10)
	}
}

func costLabel(value float64) string {
	if value <= 0 {
		return "$0.00"
	}
	if value < 0.01 {
		return fmt.Sprintf("$%.6f", value)
	}
	return fmt.Sprintf("$%.2f", value)
}

func successRateLabel(totals usageTotals) string {
	if totals.RequestCount <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", float64(totals.SuccessCount)*100/float64(totals.RequestCount))
}

func failureRateLabel(totals usageTotals) string {
	if totals.RequestCount <= 0 {
		return "-"
	}
	failed := totals.FailureCount + totals.ClientCanceledCount + totals.UpstreamResponseFailedCount + totals.StreamIncompleteCount
	return fmt.Sprintf("%.1f%%", float64(failed)*100/float64(totals.RequestCount))
}

func failurePercent(totals usageTotals) int {
	if totals.RequestCount <= 0 {
		return 0
	}
	failed := totals.FailureCount + totals.ClientCanceledCount + totals.UpstreamResponseFailedCount + totals.StreamIncompleteCount
	return clamp(int(math.Round(float64(failed)*100/float64(totals.RequestCount))), 0, 100)
}

func avgLatencyLabel(totals usageTotals) string {
	if totals.RequestCount <= 0 || totals.TotalLatencyMs <= 0 {
		return "-"
	}
	avg := float64(totals.TotalLatencyMs) / float64(totals.RequestCount)
	if avg >= 1000 {
		return fmt.Sprintf("%.1fs", avg/1000)
	}
	return fmt.Sprintf("%.0fms", avg)
}

func barWidth(value, max int64) int {
	if value <= 0 || max <= 0 {
		return 0
	}
	return clamp(int(math.Round(float64(value)*100/float64(max))), 2, 100)
}

func durationLabel(value time.Duration) string {
	if value <= 0 {
		return "关闭"
	}
	if value%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(value/time.Hour))
	}
	if value%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(value/time.Minute))
	}
	return fmt.Sprintf("%ds", int(value/time.Second))
}

func formatOptionalTime(ts *int64) string {
	if ts == nil || *ts <= 0 {
		return "-"
	}
	return formatTime(*ts)
}

func formatOptionalUnix(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return formatTime(ts)
}

func formatTime(ts int64) string {
	return time.Unix(ts, 0).Local().Format("2006-01-02 15:04:05")
}

var dashboardHTML = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Codex Quota Viewer</title>
  <style>
    :root { color-scheme: light; --bg:#f5f6f8; --panel:#fff; --text:#17202a; --muted:#667085; --line:#d8dde6; --soft:#f0f3f7; --ok:#15803d; --ok-bg:#dcfce7; --warn:#b45309; --warn-bg:#fef3c7; --danger:#b91c1c; --danger-bg:#fee2e2; --accent:#2563eb; }
    * { box-sizing: border-box; }
    body { margin: 0; font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: var(--bg); color: var(--text); }
    header { padding: 22px 28px 16px; border-bottom: 1px solid var(--line); background: var(--panel); }
    h1 { margin: 0 0 8px; font-size: 24px; font-weight: 700; }
    .muted { color: var(--muted); }
    main { padding: 20px 28px 32px; max-width: 1280px; margin: 0 auto; }
    .status-line { display: flex; flex-wrap: wrap; gap: 8px 14px; align-items: center; color: var(--muted); font-size: 13px; }
    .status-line span { display: inline-flex; align-items: center; gap: 6px; }
    .dot { width: 7px; height: 7px; border-radius: 999px; background: var(--ok); }
    .metric, section { background: var(--panel); border: 1px solid var(--line); border-radius: 8px; }
    .metric { padding: 14px 16px; }
    .metric span { display: block; font-size: 13px; color: var(--muted); margin-bottom: 6px; }
    .metric strong { font-size: 22px; }
    section { margin-top: 18px; overflow: hidden; }
    section h2 { margin: 0; padding: 14px 16px; font-size: 16px; border-bottom: 1px solid var(--line); background: #fbfcfd; }
    table { width: 100%; border-collapse: collapse; }
    th, td { padding: 11px 12px; border-bottom: 1px solid var(--line); text-align: left; font-size: 14px; vertical-align: middle; }
    th { font-size: 12px; color: var(--muted); font-weight: 600; background: #fbfcfd; }
    tr:last-child td { border-bottom: 0; }
    .pill { display: inline-flex; align-items: center; min-width: 56px; justify-content: center; padding: 3px 8px; border-radius: 999px; font-weight: 700; font-size: 12px; }
    .ok { color: var(--ok); background: var(--ok-bg); }
    .warn { color: var(--warn); background: var(--warn-bg); }
    .danger { color: var(--danger); background: var(--danger-bg); }
    .unknown { color: var(--muted); background: #eef2f7; }
    .bar-cell { min-width: 150px; }
    .quota { display: grid; grid-template-columns: 44px minmax(88px, 1fr); gap: 8px; align-items: center; }
    .track { height: 8px; overflow: hidden; border-radius: 999px; background: var(--soft); }
    .fill { height: 100%; border-radius: inherit; background: var(--ok); }
    .fill.warn { background: var(--warn); }
    .fill.danger { background: var(--danger); }
    .fill.unknown { background: #9aa4b2; }
    .grid3 { display: grid; grid-template-columns: repeat(3, minmax(220px, 1fr)); gap: 12px; padding: 16px; }
    .usage-card { border: 1px solid var(--line); border-radius: 8px; padding: 14px; background: #fff; }
    .usage-card h3 { margin: 0 0 10px; font-size: 14px; }
    .usage-card dl { display: grid; grid-template-columns: 1fr auto; gap: 7px 12px; margin: 0; font-size: 13px; }
    .usage-card dt { color: var(--muted); }
    .usage-card dd { margin: 0; font-weight: 700; }
    .stack { display: flex; height: 8px; overflow: hidden; border-radius: 999px; background: var(--soft); margin: 12px 0 2px; }
    .stack .success { background: var(--ok); }
    .stack .failure { background: var(--danger); }
    .chart { padding: 8px 16px 12px; }
    .chart-item { border-bottom: 1px solid var(--line); padding: 8px 0; }
    .chart-item:last-child { border-bottom: 0; }
    .chart-row { display: grid; grid-template-columns: minmax(140px, 240px) minmax(120px, 1fr) 84px 84px; gap: 12px; align-items: center; font-size: 14px; }
    .chart-label { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .chart-bar { height: 10px; overflow: hidden; border-radius: 999px; background: var(--soft); }
    .chart-bar span { display: block; height: 100%; border-radius: inherit; background: var(--accent); }
    .account-breakdown { margin: 8px 0 0; padding-left: 18px; display: grid; gap: 5px; color: var(--muted); font-size: 12px; }
    .account-row { display: grid; grid-template-columns: minmax(120px, 220px) 70px 78px; gap: 10px; align-items: center; }
    .account-name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--text); }
    .empty { padding: 18px 16px; color: var(--muted); }
    .error { color: var(--danger); max-width: 360px; overflow-wrap: anywhere; }
    @media (max-width: 860px) { main, header { padding-left: 14px; padding-right: 14px; } .grid3 { grid-template-columns: 1fr; } section { overflow-x: auto; } th, td { white-space: nowrap; } .chart-item { min-width: 520px; } .chart-row { grid-template-columns: 160px 180px 72px 72px; } }
  </style>
</head>
<body data-refresh-seconds="{{.RefreshSeconds}}">
  <header>
    <h1>Codex Quota Viewer</h1>
    <div class="status-line">
      <span><span class="dot"></span>生成 {{.GeneratedLabel}}</span>
      <span>用量更新 {{if .LocalAccessUsage.UpdatedLabel}}{{.LocalAccessUsage.UpdatedLabel}}{{else}}-{{end}}</span>
      <span>数据源 {{.LocalAccessUsage.Source}}</span>
      <span>自动刷新 <strong id="refresh-label">{{.RefreshLabel}}</strong><span id="refresh-countdown"></span></span>
    </div>
  </header>
  <main>
    <section>
      <h2>Codex 账号额度</h2>
      {{if .Accounts}}
      <table>
        <thead><tr><th>账号</th><th>Plan</th><th>认证</th><th>5h 剩余</th><th>5h 重置</th><th>周剩余</th><th>周重置</th><th>缓存更新</th><th>状态</th></tr></thead>
        <tbody>
        {{range .Accounts}}
          <tr>
            <td>{{.Email}}</td>
            <td>{{.PlanType}}</td>
            <td>{{.AuthMode}}</td>
            <td class="bar-cell">{{if .Hourly.Present}}<div class="quota"><strong>{{.Hourly.Remaining}}%</strong><div class="track"><div class="fill {{.Hourly.Class}}" style="width: {{.Hourly.Remaining}}%"></div></div></div>{{else}}-{{end}}</td>
            <td>{{.Hourly.ResetLabel}}</td>
            <td class="bar-cell">{{if .Weekly.Present}}<div class="quota"><strong>{{.Weekly.Remaining}}%</strong><div class="track"><div class="fill {{.Weekly.Class}}" style="width: {{.Weekly.Remaining}}%"></div></div></div>{{else}}-{{end}}</td>
            <td>{{.Weekly.ResetLabel}}</td>
            <td>{{.UsageUpdatedLabel}}</td>
            <td>{{if .Error}}<span class="error">{{.Error}}</span>{{else if .Stale}}<span class="pill warn">stale</span>{{else}}<span class="pill ok">ok</span>{{end}}</td>
          </tr>
        {{end}}
        </tbody>
      </table>
      {{else}}<div class="empty">未读取到 Codex 账号缓存。</div>{{end}}
    </section>

    <section>
      <h2>本地 API 服务用量</h2>
      {{if .LocalAccessUsage.Available}}
      <div class="grid3">
        <div class="usage-card">
          <h3>24h</h3>
          <dl><dt>请求</dt><dd>{{.LocalAccessUsage.Daily.RequestCount}}</dd><dt>成功率</dt><dd>{{successRate .LocalAccessUsage.Daily}}</dd><dt>失败率</dt><dd>{{failureRate .LocalAccessUsage.Daily}}</dd><dt>平均延迟</dt><dd>{{avgLatency .LocalAccessUsage.Daily}}</dd><dt>成本</dt><dd>{{cost .LocalAccessUsage.Daily.EstimatedCostUSD}}</dd></dl>
          <div class="stack"><span class="success" style="width: {{barWidth .LocalAccessUsage.Daily.SuccessCount .LocalAccessUsage.Daily.RequestCount}}%"></span><span class="failure" style="width: {{failurePercent .LocalAccessUsage.Daily}}%"></span></div>
        </div>
        <div class="usage-card">
          <h3>7d</h3>
          <dl><dt>请求</dt><dd>{{.LocalAccessUsage.Weekly.RequestCount}}</dd><dt>成功率</dt><dd>{{successRate .LocalAccessUsage.Weekly}}</dd><dt>失败率</dt><dd>{{failureRate .LocalAccessUsage.Weekly}}</dd><dt>平均延迟</dt><dd>{{avgLatency .LocalAccessUsage.Weekly}}</dd><dt>成本</dt><dd>{{cost .LocalAccessUsage.Weekly.EstimatedCostUSD}}</dd></dl>
          <div class="stack"><span class="success" style="width: {{barWidth .LocalAccessUsage.Weekly.SuccessCount .LocalAccessUsage.Weekly.RequestCount}}%"></span><span class="failure" style="width: {{failurePercent .LocalAccessUsage.Weekly}}%"></span></div>
        </div>
        <div class="usage-card">
          <h3>30d</h3>
          <dl><dt>请求</dt><dd>{{.LocalAccessUsage.Monthly.RequestCount}}</dd><dt>成功率</dt><dd>{{successRate .LocalAccessUsage.Monthly}}</dd><dt>失败率</dt><dd>{{failureRate .LocalAccessUsage.Monthly}}</dd><dt>平均延迟</dt><dd>{{avgLatency .LocalAccessUsage.Monthly}}</dd><dt>成本</dt><dd>{{cost .LocalAccessUsage.Monthly.EstimatedCostUSD}}</dd></dl>
          <div class="stack"><span class="success" style="width: {{barWidth .LocalAccessUsage.Monthly.SuccessCount .LocalAccessUsage.Monthly.RequestCount}}%"></span><span class="failure" style="width: {{failurePercent .LocalAccessUsage.Monthly}}%"></span></div>
        </div>
      </div>
      {{else}}<div class="empty">未读取到本地 API 服务用量数据。{{.LocalAccessUsage.Error}}</div>{{end}}
    </section>

    <section>
      <h2>模型请求排行</h2>
      {{if .LocalAccessUsage.Models}}
      <div class="chart">
        {{range .LocalAccessUsage.Models}}
        <div class="chart-item">
          <div class="chart-row">
            <div class="chart-label">{{.ModelID}}</div>
            <div class="chart-bar"><span style="width: {{barWidth .Usage.RequestCount $.MaxModelRequests}}%"></span></div>
            <div>{{.Usage.RequestCount}} 次</div>
            <div>{{cost .Usage.EstimatedCostUSD}}</div>
          </div>
          {{if .Accounts}}
          <div class="account-breakdown">
            {{range .Accounts}}
            <div class="account-row">
              <span class="account-name">{{.Account}}</span>
              <span>{{.Usage.RequestCount}} 次</span>
              <span>{{cost .Usage.EstimatedCostUSD}}</span>
            </div>
            {{end}}
          </div>
          {{end}}
        </div>
        {{end}}
      </div>
      {{else}}<div class="empty">暂无模型维度用量。</div>{{end}}
    </section>

    {{if or .StaleCount .ErrorCount}}
    <section>
      <h2>异常</h2>
      <table>
        <thead><tr><th>类型</th><th>数量</th></tr></thead>
        <tbody>
          {{if .StaleCount}}<tr><td>额度缓存过期</td><td>{{.StaleCount}}</td></tr>{{end}}
          {{if .ErrorCount}}<tr><td>账号额度错误</td><td>{{.ErrorCount}}</td></tr>{{end}}
        </tbody>
      </table>
    </section>
    {{end}}
  </main>
  <script>
    (function () {
      var refreshSeconds = Number(document.body.dataset.refreshSeconds || "0");
      if (!Number.isFinite(refreshSeconds) || refreshSeconds <= 0) return;
      var countdown = document.getElementById("refresh-countdown");
      var nextRefreshAt = Date.now() + refreshSeconds * 1000;
      function renderCountdown() {
        var remaining = Math.max(0, Math.ceil((nextRefreshAt - Date.now()) / 1000));
        var minutes = Math.floor(remaining / 60);
        var seconds = String(remaining % 60).padStart(2, "0");
        if (countdown) countdown.textContent = " · " + minutes + ":" + seconds;
        if (remaining <= 0) window.location.reload();
      }
      setInterval(renderCountdown, 1000);
      document.addEventListener("visibilitychange", function () {
        if (document.visibilityState === "visible" && Date.now() >= nextRefreshAt) {
          window.location.reload();
        }
      });
      renderCountdown();
    })();
  </script>
</body>
</html>`

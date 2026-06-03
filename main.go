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
	defaultListenAddr = ":8080"
	defaultDataDir    = "/data"
	defaultStaleAfter = 30 * time.Minute
)

type config struct {
	ListenAddr string
	DataDir    string
	StaleAfter time.Duration
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
	RequestCount     int64   `json:"requestCount"`
	SuccessCount     int64   `json:"successCount"`
	FailureCount     int64   `json:"failureCount"`
	InputTokens      int64   `json:"inputTokens"`
	OutputTokens     int64   `json:"outputTokens"`
	TotalTokens      int64   `json:"totalTokens"`
	CachedTokens     int64   `json:"cachedTokens"`
	ReasoningTokens  int64   `json:"reasoningTokens"`
	EstimatedCostUSD float64 `json:"estimatedCostUsd"`
}

type modelUsage struct {
	ModelID string      `json:"modelId"`
	Usage   usageTotals `json:"usage"`
}

type usageView struct {
	Available bool         `json:"available"`
	Source    string       `json:"source"`
	Daily     usageTotals  `json:"daily"`
	Weekly    usageTotals  `json:"weekly"`
	Monthly   usageTotals  `json:"monthly"`
	Models    []modelUsage `json:"models"`
	Error     string       `json:"error,omitempty"`
}

type summaryView struct {
	GeneratedAt      int64         `json:"generatedAt"`
	GeneratedLabel   string        `json:"generatedLabel"`
	AccountCount     int           `json:"accountCount"`
	StaleCount       int           `json:"staleCount"`
	ErrorCount       int           `json:"errorCount"`
	LowestHourly     *int          `json:"lowestHourly,omitempty"`
	LowestWeekly     *int          `json:"lowestWeekly,omitempty"`
	Accounts         []accountView `json:"accounts"`
	LocalAccessUsage usageView     `json:"localAccessUsage"`
}

type rawStatsFile struct {
	Daily   rawStatsWindow `json:"daily"`
	Weekly  rawStatsWindow `json:"weekly"`
	Monthly rawStatsWindow `json:"monthly"`
}

type rawStatsWindow struct {
	Totals usageTotals     `json:"totals"`
	Models []rawModelStats `json:"models"`
}

type rawModelStats struct {
	ModelID string      `json:"modelId"`
	Usage   usageTotals `json:"usage"`
}

func main() {
	cfg := loadConfig()
	app := &server{cfg: cfg, tmpl: template.Must(template.New("dashboard").Funcs(template.FuncMap{
		"percent":     percentLabel,
		"tokens":      compactInt,
		"cost":        costLabel,
		"statusClass": statusClass,
	}).Parse(dashboardHTML))}

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
	return config{ListenAddr: listenAddr, DataDir: filepath.Clean(dataDir), StaleAfter: staleAfter}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline' 'self'")
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
	summary := summaryView{
		GeneratedAt:      now.Unix(),
		GeneratedLabel:   formatTime(now.Unix()),
		AccountCount:     len(accounts),
		Accounts:         accounts,
		LocalAccessUsage: loadUsage(s.cfg.DataDir),
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
	return usageView{
		Available: true,
		Source:    "stats-json",
		Daily:     stats.Daily.Totals,
		Weekly:    stats.Weekly.Totals,
		Monthly:   stats.Monthly.Totals,
		Models:    models,
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
	daily := queryUsageTotals(db, now.Add(-24*time.Hour).Unix())
	weekly := queryUsageTotals(db, now.Add(-7*24*time.Hour).Unix())
	monthly := queryUsageTotals(db, now.Add(-30*24*time.Hour).Unix())
	models := queryModelUsage(db, now.Add(-30*24*time.Hour).Unix())
	return usageView{
		Available: true,
		Source:    "sqlite",
		Daily:     daily,
		Weekly:    weekly,
		Monthly:   monthly,
		Models:    models,
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
		models = append(models, model)
	}
	return models
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

func formatOptionalTime(ts *int64) string {
	if ts == nil || *ts <= 0 {
		return "-"
	}
	return formatTime(*ts)
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
    :root { color-scheme: light; --bg:#f6f7f9; --panel:#fff; --text:#17202a; --muted:#667085; --line:#d8dde6; --ok:#15803d; --warn:#b45309; --danger:#b91c1c; }
    * { box-sizing: border-box; }
    body { margin: 0; font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: var(--bg); color: var(--text); }
    header { padding: 24px 28px 18px; border-bottom: 1px solid var(--line); background: var(--panel); }
    h1 { margin: 0 0 8px; font-size: 24px; font-weight: 700; }
    .muted { color: var(--muted); }
    main { padding: 20px 28px 32px; max-width: 1280px; margin: 0 auto; }
    .summary { display: grid; grid-template-columns: repeat(5, minmax(140px, 1fr)); gap: 12px; margin-bottom: 20px; }
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
    .ok { color: var(--ok); background: #dcfce7; }
    .warn { color: var(--warn); background: #fef3c7; }
    .danger { color: var(--danger); background: #fee2e2; }
    .unknown { color: var(--muted); background: #eef2f7; }
    .grid3 { display: grid; grid-template-columns: repeat(3, minmax(180px, 1fr)); gap: 12px; padding: 16px; }
    .empty { padding: 18px 16px; color: var(--muted); }
    .error { color: var(--danger); max-width: 360px; overflow-wrap: anywhere; }
    @media (max-width: 860px) { main, header { padding-left: 14px; padding-right: 14px; } .summary, .grid3 { grid-template-columns: 1fr; } section { overflow-x: auto; } th, td { white-space: nowrap; } }
  </style>
</head>
<body>
  <header>
    <h1>Codex Quota Viewer</h1>
    <div class="muted">生成时间 {{.GeneratedLabel}}，数据来自只读 Cockpit Tools 本地缓存。</div>
  </header>
  <main>
    <div class="summary">
      <div class="metric"><span>账号数</span><strong>{{.AccountCount}}</strong></div>
      <div class="metric"><span>最低 5h 额度</span><strong>{{percent .LowestHourly}}</strong></div>
      <div class="metric"><span>最低周额度</span><strong>{{percent .LowestWeekly}}</strong></div>
      <div class="metric"><span>过期缓存</span><strong>{{.StaleCount}}</strong></div>
      <div class="metric"><span>错误账号</span><strong class="{{statusClass .}}">{{.ErrorCount}}</strong></div>
    </div>

    <section>
      <h2>Codex 账号额度</h2>
      {{if .Accounts}}
      <table>
        <thead><tr><th>账号</th><th>Plan</th><th>认证</th><th>5h 剩余</th><th>5h 重置</th><th>周剩余</th><th>周重置</th><th>上次刷新</th><th>状态</th></tr></thead>
        <tbody>
        {{range .Accounts}}
          <tr>
            <td>{{.Email}}</td>
            <td>{{.PlanType}}</td>
            <td>{{.AuthMode}}</td>
            <td>{{if .Hourly.Present}}<span class="pill {{.Hourly.Class}}">{{.Hourly.Remaining}}%</span>{{else}}-{{end}}</td>
            <td>{{.Hourly.ResetLabel}}</td>
            <td>{{if .Weekly.Present}}<span class="pill {{.Weekly.Class}}">{{.Weekly.Remaining}}%</span>{{else}}-{{end}}</td>
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
        <div class="metric"><span>24h 请求 / tokens</span><strong>{{.LocalAccessUsage.Daily.RequestCount}} / {{tokens .LocalAccessUsage.Daily.TotalTokens}}</strong></div>
        <div class="metric"><span>7d 请求 / tokens</span><strong>{{.LocalAccessUsage.Weekly.RequestCount}} / {{tokens .LocalAccessUsage.Weekly.TotalTokens}}</strong></div>
        <div class="metric"><span>30d 请求 / 成本</span><strong>{{.LocalAccessUsage.Monthly.RequestCount}} / {{cost .LocalAccessUsage.Monthly.EstimatedCostUSD}}</strong></div>
      </div>
      {{if .LocalAccessUsage.Models}}
      <table>
        <thead><tr><th>模型</th><th>请求</th><th>成功</th><th>失败</th><th>输入 tokens</th><th>输出 tokens</th><th>总 tokens</th><th>估算成本</th></tr></thead>
        <tbody>
        {{range .LocalAccessUsage.Models}}
          <tr>
            <td>{{.ModelID}}</td>
            <td>{{.Usage.RequestCount}}</td>
            <td>{{.Usage.SuccessCount}}</td>
            <td>{{.Usage.FailureCount}}</td>
            <td>{{tokens .Usage.InputTokens}}</td>
            <td>{{tokens .Usage.OutputTokens}}</td>
            <td>{{tokens .Usage.TotalTokens}}</td>
            <td>{{cost .Usage.EstimatedCostUSD}}</td>
          </tr>
        {{end}}
        </tbody>
      </table>
      {{end}}
      {{else}}<div class="empty">未读取到本地 API 服务用量数据。{{.LocalAccessUsage.Error}}</div>{{end}}
    </section>
  </main>
</body>
</html>`

package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestMaskIdentity(t *testing.T) {
	tests := map[string]string{
		"mike@gmail.com":      "m***@**.com",
		"alice@company.co.uk": "a***@**.uk",
		"api-key-50ccfbb0":    "api-key-****",
		"plainaccount":        "p***",
		"":                    "-",
	}

	for input, want := range tests {
		if got := maskIdentity(input); got != want {
			t.Fatalf("maskIdentity(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLoadAccountsReturnsOnlySanitizedView(t *testing.T) {
	dir := t.TempDir()
	accountsDir := filepath.Join(dir, "codex_accounts")
	if err := os.MkdirAll(accountsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	updatedAt := time.Now().Add(-5 * time.Minute).Unix()
	content := `{
		"id": "internal-account-id",
		"email": "mike@gmail.com",
		"auth_mode": "oauth",
		"plan_type": "Plus",
		"tokens": {
			"id_token": "secret-id-token",
			"access_token": "secret-access-token",
			"refresh_token": "secret-refresh-token"
		},
		"quota": {
			"hourly_percentage": 80,
			"hourly_reset_time": 1780000000,
			"hourly_window_minutes": 300,
			"hourly_window_present": true,
			"weekly_percentage": 60,
			"weekly_reset_time": 1780100000,
			"weekly_window_minutes": 10080,
			"weekly_window_present": true,
			"raw_data": {"secret": "do-not-expose"}
		},
		"usage_updated_at": ` + strconv.FormatInt(updatedAt, 10) + `
	}`
	if err := os.WriteFile(filepath.Join(accountsDir, "account.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	accounts, err := loadAccounts(config{DataDir: dir, StaleAfter: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1", len(accounts))
	}
	account := accounts[0]
	if account.Email != "m***@**.com" {
		t.Fatalf("email = %q", account.Email)
	}
	if account.Hourly.Remaining != 80 || account.Weekly.Remaining != 60 {
		t.Fatalf("unexpected quota: %+v %+v", account.Hourly, account.Weekly)
	}
	if account.Stale {
		t.Fatal("account should not be stale")
	}
}

func TestLoadUsageFromSQLite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex_local_access_logs.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE request_logs (
			timestamp INTEGER NOT NULL,
			model_id TEXT NOT NULL,
			success INTEGER NOT NULL,
			input_tokens INTEGER NOT NULL,
			output_tokens INTEGER NOT NULL,
			total_tokens INTEGER NOT NULL,
			cached_tokens INTEGER NOT NULL,
			reasoning_tokens INTEGER NOT NULL,
			estimated_cost_usd REAL NOT NULL
		);
		INSERT INTO request_logs VALUES
			(?, 'gpt-5-codex', 1, 10, 5, 15, 3, 1, 0.001),
			(?, 'gpt-5-codex', 0, 2, 0, 2, 0, 0, 0.0002);
	`, time.Now().Unix(), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	usage := loadUsageFromSQLite(path)
	if !usage.Available {
		t.Fatalf("usage should be available: %+v", usage)
	}
	if usage.Daily.RequestCount != 2 {
		t.Fatalf("request count = %d", usage.Daily.RequestCount)
	}
	if usage.Daily.TotalTokens != 17 {
		t.Fatalf("total tokens = %d", usage.Daily.TotalTokens)
	}
	if len(usage.Models) != 1 || usage.Models[0].ModelID != "gpt-5-codex" {
		t.Fatalf("models = %+v", usage.Models)
	}
}

func TestSQLiteReadOnlyDSNUsesImmutableMode(t *testing.T) {
	dsn := sqliteReadOnlyDSN("/data/codex_local_access_logs.sqlite")
	if !strings.HasPrefix(dsn, "file:") {
		t.Fatalf("dsn = %q, want file URI", dsn)
	}
	for _, want := range []string{"mode=ro", "immutable=1"} {
		if !strings.Contains(dsn, want) {
			t.Fatalf("dsn = %q, missing %q", dsn, want)
		}
	}
}

func TestLoadUsageFromSQLiteIncludesModelAccountBreakdown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex_local_access_logs.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE request_logs (
			timestamp INTEGER NOT NULL,
			account_id TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '',
			model_id TEXT NOT NULL,
			success INTEGER NOT NULL,
			input_tokens INTEGER NOT NULL,
			output_tokens INTEGER NOT NULL,
			total_tokens INTEGER NOT NULL,
			cached_tokens INTEGER NOT NULL,
			reasoning_tokens INTEGER NOT NULL,
			estimated_cost_usd REAL NOT NULL
		);
		INSERT INTO request_logs VALUES
			(?, 'account-a', 'alice@example.com', 'gpt-5.5', 1, 10, 5, 15, 3, 1, 1.25),
			(?, 'account-b', 'bob@example.com', 'gpt-5.5', 1, 20, 8, 28, 4, 2, 2.50),
			(?, 'account-b', 'bob@example.com', 'gpt-5.5', 0, 1, 0, 1, 0, 0, 0.50),
			(?, 'account-a', 'alice@example.com', 'gpt-5-codex', 1, 4, 2, 6, 1, 0, 0.25);
	`, time.Now().Unix(), time.Now().Unix(), time.Now().Unix(), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	usage := loadUsageFromSQLite(path)
	model := findModel(t, usage.Models, "gpt-5.5")
	if model.Usage.RequestCount != 3 {
		t.Fatalf("model request count = %d, want 3", model.Usage.RequestCount)
	}
	if len(model.Accounts) != 2 {
		t.Fatalf("account breakdown len = %d, want 2: %+v", len(model.Accounts), model.Accounts)
	}
	if model.Accounts[0].Account != "b***@**.com" || model.Accounts[0].Usage.RequestCount != 2 {
		t.Fatalf("first account = %+v", model.Accounts[0])
	}
	if model.Accounts[1].Account != "a***@**.com" || model.Accounts[1].Usage.RequestCount != 1 {
		t.Fatalf("second account = %+v", model.Accounts[1])
	}
}

func TestLoadUsageAugmentsStatsJSONWithSQLiteModelAccounts(t *testing.T) {
	dir := t.TempDir()
	stats := `{
		"since": 1700000000,
		"updatedAt": 1700000300,
		"daily": {"totals": {}, "models": []},
		"weekly": {"totals": {}, "models": []},
		"monthly": {
			"since": 1700000000,
			"totals": {"requestCount": 3, "estimatedCostUsd": 4.25},
			"models": [
				{"modelId": "gpt-5.5", "usage": {"requestCount": 3, "estimatedCostUsd": 4.25}}
			]
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "codex_local_access_stats.json"), []byte(stats), 0o644); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "codex_local_access_logs.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE request_logs (
			timestamp INTEGER NOT NULL,
			account_id TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '',
			model_id TEXT NOT NULL,
			success INTEGER NOT NULL,
			input_tokens INTEGER NOT NULL,
			output_tokens INTEGER NOT NULL,
			total_tokens INTEGER NOT NULL,
			cached_tokens INTEGER NOT NULL,
			reasoning_tokens INTEGER NOT NULL,
			estimated_cost_usd REAL NOT NULL
		);
		INSERT INTO request_logs VALUES
			(1700000100, 'account-a', 'alice@example.com', 'gpt-5.5', 1, 10, 5, 15, 3, 1, 1.25),
			(1700000200, 'account-b', 'bob@example.com', 'gpt-5.5', 1, 20, 8, 28, 4, 2, 2.50),
			(1700000300, 'account-b', 'bob@example.com', 'gpt-5.5', 0, 1, 0, 1, 0, 0, 0.50);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	usage := loadUsage(dir)
	if usage.Source != "stats-json" {
		t.Fatalf("source = %q, want stats-json", usage.Source)
	}
	model := findModel(t, usage.Models, "gpt-5.5")
	if len(model.Accounts) != 2 {
		t.Fatalf("account breakdown len = %d, want 2: %+v", len(model.Accounts), model.Accounts)
	}
	if model.Accounts[0].Account != "b***@**.com" || model.Accounts[0].Usage.RequestCount != 2 {
		t.Fatalf("first account = %+v", model.Accounts[0])
	}
}

func TestRefreshIntervalConfig(t *testing.T) {
	t.Setenv("LISTEN_ADDR", ":9090")
	t.Setenv("DATA_DIR", "/tmp/codex-data")
	t.Setenv("REFRESH_INTERVAL_SECONDS", "120")
	t.Setenv("WEEKLY_RESET_NOTIFY_URL", "https://mlntfy.example/api/notifications/simple/send/mlNtfy")
	t.Setenv("WEEKLY_RESET_NOTIFY_STATE_DIR", "/tmp/codex-state")
	t.Setenv("WEEKLY_RESET_NOTIFY_TIMEOUT_SECONDS", "3")

	cfg := loadConfig()
	if cfg.Refresh != 2*time.Minute {
		t.Fatalf("refresh = %s, want 2m", cfg.Refresh)
	}
	if cfg.ListenAddr != ":9090" || cfg.DataDir != "/tmp/codex-data" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.WeeklyResetNotifyURL != "https://mlntfy.example/api/notifications/simple/send/mlNtfy" {
		t.Fatalf("notify url = %q", cfg.WeeklyResetNotifyURL)
	}
	if cfg.WeeklyResetNotifyStateDir != "/tmp/codex-state" {
		t.Fatalf("state dir = %q", cfg.WeeklyResetNotifyStateDir)
	}
	if cfg.WeeklyResetNotifyTimeout != 3*time.Second {
		t.Fatalf("notify timeout = %s", cfg.WeeklyResetNotifyTimeout)
	}
}

func TestWeeklyResetNotifierSendsWebhookWhenObservedResetPasses(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	now := time.Unix(1_780_000_000, 0)
	oldReset := now.Add(-time.Minute).Unix()
	nextReset := now.Add(time.Hour).Unix()
	writeTestAccount(t, dir, "account-a", "alice@example.com", 88, nextReset)
	if err := saveWeeklyResetReminderState(filepath.Join(stateDir, weeklyResetReminderStateFile), weeklyResetReminderState{
		Accounts: map[string]weeklyResetAccountState{
			"account-a": {ObservedResetAt: oldReset},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var gotPath string
	var gotPayload map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if contentType := r.Header.Get("Content-Type"); contentType != "application/json" {
			t.Fatalf("content type = %q", contentType)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer server.Close()

	cfg := config{
		DataDir:                   dir,
		StaleAfter:                30 * time.Minute,
		WeeklyResetNotifyURL:      server.URL + "/api/notifications/simple/send/mlNtfy",
		WeeklyResetNotifyStateDir: stateDir,
		WeeklyResetNotifyTimeout:  time.Second,
	}
	if err := checkWeeklyResetNotifications(cfg, now, server.Client()); err != nil {
		t.Fatal(err)
	}

	if gotPath != "/api/notifications/simple/send/mlNtfy" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotPayload["title"] != "Codex 周额度已重置" {
		t.Fatalf("title = %q", gotPayload["title"])
	}
	for _, want := range []string{"a***@**.com", "88%", formatTime(oldReset)} {
		if !strings.Contains(gotPayload["message"], want) {
			t.Fatalf("message missing %q: %q", want, gotPayload["message"])
		}
	}
	if gotPayload["priority"] != "high" || gotPayload["tags"] != "codex,quota,weekly-reset" {
		t.Fatalf("unexpected payload: %+v", gotPayload)
	}

	state, err := loadWeeklyResetReminderState(filepath.Join(stateDir, weeklyResetReminderStateFile))
	if err != nil {
		t.Fatal(err)
	}
	accountState := state.Accounts["account-a"]
	if accountState.NotifiedResetAt != oldReset {
		t.Fatalf("notified reset = %d, want %d", accountState.NotifiedResetAt, oldReset)
	}
	if accountState.ObservedResetAt != nextReset {
		t.Fatalf("observed reset = %d, want %d", accountState.ObservedResetAt, nextReset)
	}
}

func TestWeeklyResetNotifierSendsWebhookWhenFutureResetJumpsForward(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	now := time.Unix(1_780_000_000, 0)
	observedReset := now.Add(time.Hour).Unix()
	nextReset := now.Add(7 * 24 * time.Hour).Unix()
	writeTestAccount(t, dir, "account-a", "alice@example.com", 100, nextReset)
	if err := saveWeeklyResetReminderState(filepath.Join(stateDir, weeklyResetReminderStateFile), weeklyResetReminderState{
		Accounts: map[string]weeklyResetAccountState{
			"account-a": {ObservedResetAt: observedReset},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var gotPayload map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer server.Close()

	cfg := config{
		DataDir:                   dir,
		StaleAfter:                30 * time.Minute,
		WeeklyResetNotifyURL:      server.URL + "/api/notifications/simple/send/mlNtfy",
		WeeklyResetNotifyStateDir: stateDir,
		WeeklyResetNotifyTimeout:  time.Second,
	}
	if err := checkWeeklyResetNotifications(cfg, now, server.Client()); err != nil {
		t.Fatal(err)
	}
	if gotPayload["title"] != "Codex 周额度已重置" {
		t.Fatalf("title = %q", gotPayload["title"])
	}
	for _, want := range []string{"a***@**.com", "100%", formatTime(observedReset), formatTime(nextReset)} {
		if !strings.Contains(gotPayload["message"], want) {
			t.Fatalf("message missing %q: %q", want, gotPayload["message"])
		}
	}

	state, err := loadWeeklyResetReminderState(filepath.Join(stateDir, weeklyResetReminderStateFile))
	if err != nil {
		t.Fatal(err)
	}
	accountState := state.Accounts["account-a"]
	if accountState.NotifiedResetAt != observedReset {
		t.Fatalf("notified reset = %d, want %d", accountState.NotifiedResetAt, observedReset)
	}
	if accountState.ObservedResetAt != nextReset {
		t.Fatalf("observed reset = %d, want %d", accountState.ObservedResetAt, nextReset)
	}
}

func TestWeeklyResetNotifierDoesNotSendDuplicateWebhook(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	now := time.Unix(1_780_000_000, 0)
	resetAt := now.Add(-time.Minute).Unix()
	writeTestAccount(t, dir, "account-a", "alice@example.com", 88, resetAt)
	if err := saveWeeklyResetReminderState(filepath.Join(stateDir, weeklyResetReminderStateFile), weeklyResetReminderState{
		Accounts: map[string]weeklyResetAccountState{
			"account-a": {ObservedResetAt: resetAt, NotifiedResetAt: resetAt},
		},
	}); err != nil {
		t.Fatal(err)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		t.Fatalf("unexpected webhook request")
	}))
	defer server.Close()

	cfg := config{
		DataDir:                   dir,
		StaleAfter:                30 * time.Minute,
		WeeklyResetNotifyURL:      server.URL + "/api/notifications/simple/send/mlNtfy",
		WeeklyResetNotifyStateDir: stateDir,
		WeeklyResetNotifyTimeout:  time.Second,
	}
	if err := checkWeeklyResetNotifications(cfg, now, server.Client()); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func TestWeeklyResetNotifierIgnoresTinyFutureResetDrift(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	now := time.Unix(1_780_000_000, 0)
	observedReset := now.Add(7 * 24 * time.Hour).Unix()
	currentReset := observedReset + 1
	writeTestAccount(t, dir, "account-a", "alice@example.com", 97, currentReset)
	if err := saveWeeklyResetReminderState(filepath.Join(stateDir, weeklyResetReminderStateFile), weeklyResetReminderState{
		Accounts: map[string]weeklyResetAccountState{
			"account-a": {ObservedResetAt: observedReset},
		},
	}); err != nil {
		t.Fatal(err)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		t.Fatalf("unexpected webhook request")
	}))
	defer server.Close()

	cfg := config{
		DataDir:                   dir,
		StaleAfter:                30 * time.Minute,
		WeeklyResetNotifyURL:      server.URL + "/api/notifications/simple/send/mlNtfy",
		WeeklyResetNotifyStateDir: stateDir,
		WeeklyResetNotifyTimeout:  time.Second,
	}
	if err := checkWeeklyResetNotifications(cfg, now, server.Client()); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}

	state, err := loadWeeklyResetReminderState(filepath.Join(stateDir, weeklyResetReminderStateFile))
	if err != nil {
		t.Fatal(err)
	}
	accountState := state.Accounts["account-a"]
	if accountState.NotifiedResetAt != 0 {
		t.Fatalf("notified reset = %d, want 0", accountState.NotifiedResetAt)
	}
	if accountState.ObservedResetAt != currentReset {
		t.Fatalf("observed reset = %d, want %d", accountState.ObservedResetAt, currentReset)
	}
}

func TestUsageDisplayHelpers(t *testing.T) {
	totals := usageTotals{
		RequestCount:        10,
		SuccessCount:        8,
		FailureCount:        1,
		ClientCanceledCount: 1,
		TotalLatencyMs:      12_500,
		EstimatedCostUSD:    0.0123,
	}
	if got := successRateLabel(totals); got != "80.0%" {
		t.Fatalf("successRateLabel = %q", got)
	}
	if got := failureRateLabel(totals); got != "20.0%" {
		t.Fatalf("failureRateLabel = %q", got)
	}
	if got := failurePercent(totals); got != 20 {
		t.Fatalf("failurePercent = %d", got)
	}
	if got := avgLatencyLabel(totals); got != "1.2s" {
		t.Fatalf("avgLatencyLabel = %q", got)
	}
	if got := barWidth(5, 10); got != 50 {
		t.Fatalf("barWidth = %d", got)
	}
	if got := durationLabel(5 * time.Minute); got != "5m" {
		t.Fatalf("durationLabel = %q", got)
	}
}

func TestDashboardTemplateRendersNewLayout(t *testing.T) {
	tmpl := template.Must(template.New("dashboard").Funcs(dashboardFuncs()).Parse(dashboardHTML))
	summary := summaryView{
		GeneratedLabel:   "2026-06-04 13:30:00",
		RefreshSeconds:   300,
		RefreshLabel:     "5m",
		MaxModelRequests: 20,
		Accounts: []accountView{{
			Email:             "m***@**.com",
			AuthMode:          "oauth",
			PlanType:          "Plus",
			Hourly:            quotaWindow{Present: true, Remaining: 98, ResetLabel: "2026-06-04 18:00:00", Class: "ok"},
			Weekly:            quotaWindow{Present: true, Remaining: 94, ResetLabel: "2026-06-08 18:00:00", Class: "ok"},
			UsageUpdatedLabel: "2026-06-04 13:29:00",
		}},
		LocalAccessUsage: usageView{
			Available:    true,
			Source:       "stats-json",
			UpdatedLabel: "2026-06-04 13:29:00",
			Daily:        usageTotals{RequestCount: 10, SuccessCount: 9, FailureCount: 1, TotalLatencyMs: 5000},
			Weekly:       usageTotals{RequestCount: 20, SuccessCount: 18, FailureCount: 2, TotalLatencyMs: 12_000},
			Monthly:      usageTotals{RequestCount: 20, SuccessCount: 18, FailureCount: 2, TotalLatencyMs: 12_000, EstimatedCostUSD: 0.02},
			Models: []modelUsage{{
				ModelID: "gpt-5-codex",
				Usage:   usageTotals{RequestCount: 20, EstimatedCostUSD: 0.02},
				Accounts: []accountUsage{{
					Account: "m***@**.com",
					Usage:   usageTotals{RequestCount: 12, EstimatedCostUSD: 0.012},
				}},
			}},
		},
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, summary); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"data-refresh-seconds=\"300\"", "模型请求排行", "gpt-5-codex", "m***@**.com", "生成 2026-06-04 13:30:00"} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered html missing %q", want)
		}
	}
	if strings.Contains(html, "账号数") || strings.Contains(html, "最低 5h 额度") {
		t.Fatalf("rendered html still contains removed summary metrics")
	}
}

func findModel(t *testing.T, models []modelUsage, modelID string) modelUsage {
	t.Helper()
	for _, model := range models {
		if model.ModelID == modelID {
			return model
		}
	}
	t.Fatalf("model %q not found in %+v", modelID, models)
	return modelUsage{}
}

func writeTestAccount(t *testing.T, dir, id, email string, weeklyRemaining int, weeklyResetAt int64) {
	t.Helper()
	accountsDir := filepath.Join(dir, "codex_accounts")
	if err := os.MkdirAll(accountsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{
		"email": "` + email + `",
		"auth_mode": "oauth",
		"plan_type": "Plus",
		"quota": {
			"hourly_percentage": 80,
			"hourly_window_present": true,
			"weekly_percentage": ` + strconv.Itoa(weeklyRemaining) + `,
			"weekly_reset_time": ` + strconv.FormatInt(weeklyResetAt, 10) + `,
			"weekly_window_minutes": 10080,
			"weekly_window_present": true
		},
		"usage_updated_at": ` + strconv.FormatInt(time.Now().Unix(), 10) + `
	}`
	if err := os.WriteFile(filepath.Join(accountsDir, id+".json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

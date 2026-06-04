package main

import (
	"bytes"
	"database/sql"
	"html/template"
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

func TestRefreshIntervalConfig(t *testing.T) {
	t.Setenv("LISTEN_ADDR", ":9090")
	t.Setenv("DATA_DIR", "/tmp/codex-data")
	t.Setenv("REFRESH_INTERVAL_SECONDS", "120")

	cfg := loadConfig()
	if cfg.Refresh != 2*time.Minute {
		t.Fatalf("refresh = %s, want 2m", cfg.Refresh)
	}
	if cfg.ListenAddr != ":9090" || cfg.DataDir != "/tmp/codex-data" {
		t.Fatalf("unexpected config: %+v", cfg)
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
			Models:       []modelUsage{{ModelID: "gpt-5-codex", Usage: usageTotals{RequestCount: 20, EstimatedCostUSD: 0.02}}},
		},
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, summary); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"data-refresh-seconds=\"300\"", "模型请求排行", "gpt-5-codex", "生成 2026-06-04 13:30:00"} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered html missing %q", want)
		}
	}
	if strings.Contains(html, "账号数") || strings.Contains(html, "最低 5h 额度") {
		t.Fatalf("rendered html still contains removed summary metrics")
	}
}

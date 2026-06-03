package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
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

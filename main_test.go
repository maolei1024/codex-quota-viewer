package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"math"
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

func TestLoadAccountsDecryptsCockpitSecureEnvelope(t *testing.T) {
	dir := t.TempDir()
	accountsDir := filepath.Join(dir, "codex_accounts")
	if err := os.MkdirAll(accountsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	updatedAt := time.Now().Add(-5 * time.Minute).Unix()
	plaintext := []byte(`{
		"id": "encrypted-account-id",
		"email": "alice@example.com",
		"auth_mode": "oauth",
		"plan_type": "Plus",
		"quota": {
			"hourly_percentage": 75,
			"hourly_window_present": true,
			"weekly_percentage": 55,
			"weekly_window_present": true
		},
		"usage_updated_at": ` + strconv.FormatInt(updatedAt, 10) + `
	}`)
	key := bytes.Repeat([]byte{0x42}, 32)
	if err := os.WriteFile(
		filepath.Join(dir, secureAccountKeyFile),
		[]byte(base64.StdEncoding.EncodeToString(key)),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{0x24}, gcm.NonceSize())
	envelope := secureAccountEnvelope{
		Version:     secureAccountVersion,
		Kind:        "codex",
		Algorithm:   "AES-256-GCM",
		KeyID:       "local-secure-account-storage-v1",
		Nonce:       base64.StdEncoding.EncodeToString(nonce),
		Ciphertext:  base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, plaintext, nil)),
		EncryptedAt: time.Now().Unix(),
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(accountsDir, "account.json"), raw, 0o600); err != nil {
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
	if account.Email != "a***@**.com" || account.AuthMode != "oauth" || account.PlanType != "Plus" {
		t.Fatalf("unexpected decrypted account: %+v", account)
	}
	if account.Hourly.Remaining != 75 || account.Weekly.Remaining != 55 {
		t.Fatalf("unexpected decrypted quota: %+v %+v", account.Hourly, account.Weekly)
	}
}

func TestLoadAccountsClassifiesSingleSevenDayPrimaryWindowAsWeekly(t *testing.T) {
	dir := t.TempDir()
	accountsDir := filepath.Join(dir, "codex_accounts")
	if err := os.MkdirAll(accountsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	resetAt := time.Now().Add(7 * 24 * time.Hour).Unix()
	content := `{
		"email": "alice@example.com",
		"auth_mode": "oauth",
		"plan_type": "Plus",
		"quota": {
			"hourly_percentage": 67,
			"hourly_reset_time": ` + strconv.FormatInt(resetAt, 10) + `,
			"hourly_window_minutes": 10080,
			"hourly_window_present": true,
			"weekly_percentage": 100,
			"weekly_window_present": false
		}
	}`
	if err := os.WriteFile(filepath.Join(accountsDir, "account.json"), []byte(content), 0o600); err != nil {
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
	if account.Hourly.Present {
		t.Fatalf("short quota window should be absent: %+v", account.Hourly)
	}
	if !account.Weekly.Present || account.Weekly.Remaining != 67 || account.Weekly.Window != "168h" {
		t.Fatalf("seven-day primary window not classified as weekly: %+v", account.Weekly)
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

func TestLoadUsageFromSQLiteSupportsMillisecondTimestamps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex_local_access_logs.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
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
			(?, 'recent', 1, 10, 5, 15, 3, 1, 0.001),
			(?, 'older', 1, 2, 1, 3, 0, 0, 0.0002);
	`, now.Add(-2*time.Hour).UnixMilli(), now.Add(-48*time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	usage := loadUsageFromSQLite(path)
	if usage.Daily.RequestCount != 1 {
		t.Fatalf("daily request count = %d, want 1", usage.Daily.RequestCount)
	}
	if usage.Weekly.RequestCount != 2 {
		t.Fatalf("weekly request count = %d, want 2", usage.Weekly.RequestCount)
	}
}

func TestSQLiteReadOnlyDSNUsesLiveReadOnlyMode(t *testing.T) {
	dsn := sqliteReadOnlyDSN("/data/codex_local_access_logs.sqlite")
	if !strings.HasPrefix(dsn, "file:") {
		t.Fatalf("dsn = %q, want file URI", dsn)
	}
	if !strings.Contains(dsn, "mode=ro") {
		t.Fatalf("dsn = %q, missing mode=ro", dsn)
	}
	if strings.Contains(dsn, "immutable=") {
		t.Fatalf("dsn = %q, live WAL databases must not use immutable mode", dsn)
	}
}

func TestSQLiteReadOnlyDSNReadsLiveWALFromReadOnlyFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex_local_access_logs.sqlite")
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`
		PRAGMA journal_mode = WAL;
		PRAGMA wal_autocheckpoint = 0;
		CREATE TABLE request_logs (id INTEGER PRIMARY KEY);
		INSERT INTO request_logs VALUES (1);
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec("INSERT INTO request_logs VALUES (2)"); err != nil {
		t.Fatal(err)
	}

	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(file, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
		for _, file := range []string{path, path + "-wal", path + "-shm"} {
			_ = os.Chmod(file, 0o644)
		}
	})

	reader, err := sql.Open("sqlite", sqliteReadOnlyDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var count int
	if err := reader.QueryRow("SELECT COUNT(*) FROM request_logs").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("row count = %d, want live WAL row count 2", count)
	}
}

func TestServerUsageWaitStopsWhenRequestIsCanceled(t *testing.T) {
	app := &server{
		cfg:            config{DataDir: t.TempDir()},
		usageQueryGate: make(chan struct{}, 1),
	}
	app.usageQueryGate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	usage := app.loadUsage(ctx)
	if usage.Error != "request canceled" {
		t.Fatalf("usage error = %q, want request canceled", usage.Error)
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

func TestStatsJSONReconcilesAccountCostsToModelTotal(t *testing.T) {
	dir := t.TempDir()
	const since = int64(1_700_000_000_000)
	stats := `{
		"since": 1700000000000,
		"updatedAt": 1700000300000,
		"daily": {"totals": {}, "models": []},
		"weekly": {"totals": {}, "models": []},
		"monthly": {
			"since": 1700000000000,
			"totals": {"requestCount": 2, "estimatedCostUsd": 2},
			"models": [
				{"modelId": "gpt-5.5", "usage": {"requestCount": 2, "estimatedCostUsd": 2}}
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
			(?, 'account-a', 'alice@example.com', 'gpt-5.5', 1, 10, 5, 15, 3, 1, 1),
			(?, 'account-b', 'bob@example.com', 'gpt-5.5', 1, 20, 8, 28, 4, 2, 3);
	`, since+1000, since+2000)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	usage := loadUsage(dir)
	model := findModel(t, usage.Models, "gpt-5.5")
	if len(model.Accounts) != 2 {
		t.Fatalf("account breakdown len = %d, want 2", len(model.Accounts))
	}
	var accountCost float64
	for _, account := range model.Accounts {
		accountCost += account.Usage.EstimatedCostUSD
	}
	if math.Abs(accountCost-model.Usage.EstimatedCostUSD) > 1e-9 {
		t.Fatalf("account cost sum = %f, model cost = %f", accountCost, model.Usage.EstimatedCostUSD)
	}
	if usage.SinceLabel != formatTime(since) || usage.UpdatedLabel != formatTime(1_700_000_300_000) {
		t.Fatalf("millisecond labels not normalized: %+v", usage)
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
	writeTestAccountWithUsageUpdatedAt(t, dir, "account-a", "alice@example.com", 88, nextReset, now.Unix())
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
	writeTestAccountWithUsageUpdatedAt(t, dir, "account-a", "alice@example.com", 100, nextReset, now.Unix())
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
	rawAccountState := readWeeklyResetAccountStateRaw(t, stateDir, "account-a")
	if got := requireStateInt64(t, rawAccountState, "suppressFutureJumpsUntil"); got != nextReset {
		t.Fatalf("suppress future jumps until = %d, want %d", got, nextReset)
	}
}

func TestWeeklyResetNotifierSuppressesRepeatedFutureResetJumpUntilNextCycle(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	now := time.Unix(1_780_000_000, 0)
	firstObservedReset := now.Add(time.Hour).Unix()
	firstNextReset := now.Add(7 * 24 * time.Hour).Unix()
	extendedNextReset := firstNextReset + int64(30*time.Minute/time.Second)
	writeTestAccountWithUsageUpdatedAt(t, dir, "account-a", "alice@example.com", 96, extendedNextReset, now.Unix())
	writeWeeklyResetAccountStateRaw(t, stateDir, "account-a", map[string]any{
		"account":                  "a***@**.com",
		"observedResetAt":          firstNextReset,
		"notifiedResetAt":          firstObservedReset,
		"suppressFutureJumpsUntil": firstNextReset,
		"updatedAt":                now.Unix(),
	})

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
	if accountState.NotifiedResetAt != firstObservedReset {
		t.Fatalf("notified reset = %d, want %d", accountState.NotifiedResetAt, firstObservedReset)
	}
	if accountState.ObservedResetAt != extendedNextReset {
		t.Fatalf("observed reset = %d, want %d", accountState.ObservedResetAt, extendedNextReset)
	}
	rawAccountState := readWeeklyResetAccountStateRaw(t, stateDir, "account-a")
	if got := requireStateInt64(t, rawAccountState, "suppressFutureJumpsUntil"); got != extendedNextReset {
		t.Fatalf("suppress future jumps until = %d, want %d", got, extendedNextReset)
	}
}

func TestWeeklyResetNotifierAllowsFutureResetJumpAfterSuppressionExpires(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	now := time.Unix(1_780_000_000, 0)
	previousObservedReset := now.Add(time.Hour).Unix()
	expiredSuppressUntil := now.Add(-time.Minute).Unix()
	nextReset := now.Add(7 * 24 * time.Hour).Unix()
	writeTestAccountWithUsageUpdatedAt(t, dir, "account-a", "alice@example.com", 99, nextReset, now.Unix())
	writeWeeklyResetAccountStateRaw(t, stateDir, "account-a", map[string]any{
		"account":                  "a***@**.com",
		"observedResetAt":          previousObservedReset,
		"notifiedResetAt":          expiredSuppressUntil,
		"suppressFutureJumpsUntil": expiredSuppressUntil,
		"updatedAt":                now.Unix(),
	})

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

	state, err := loadWeeklyResetReminderState(filepath.Join(stateDir, weeklyResetReminderStateFile))
	if err != nil {
		t.Fatal(err)
	}
	accountState := state.Accounts["account-a"]
	if accountState.NotifiedResetAt != previousObservedReset {
		t.Fatalf("notified reset = %d, want %d", accountState.NotifiedResetAt, previousObservedReset)
	}
	if accountState.ObservedResetAt != nextReset {
		t.Fatalf("observed reset = %d, want %d", accountState.ObservedResetAt, nextReset)
	}
	rawAccountState := readWeeklyResetAccountStateRaw(t, stateDir, "account-a")
	if got := requireStateInt64(t, rawAccountState, "suppressFutureJumpsUntil"); got != nextReset {
		t.Fatalf("suppress future jumps until = %d, want %d", got, nextReset)
	}
}

func TestWeeklyResetNotifierDoesNotSendDuplicateWebhook(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	now := time.Unix(1_780_000_000, 0)
	resetAt := now.Add(-time.Minute).Unix()
	writeTestAccountWithUsageUpdatedAt(t, dir, "account-a", "alice@example.com", 88, resetAt, now.Unix())
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
	writeTestAccountWithUsageUpdatedAt(t, dir, "account-a", "alice@example.com", 97, currentReset, now.Unix())
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

func TestLowWeeklyNotifierSendsAtThresholdOnlyOncePerCycle(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	now := time.Unix(1_780_000_000, 0)
	resetAt := now.Add(24 * time.Hour).Unix()
	writeTestAccountWithUsageUpdatedAt(t, dir, "account-a", "alice@example.com", lowWeeklyQuotaThreshold, resetAt, now.Unix())

	var payloads []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
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
	if err := checkWeeklyResetNotifications(cfg, now.Add(time.Minute), server.Client()); err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 1 {
		t.Fatalf("notifications = %d, want 1", len(payloads))
	}
	payload := payloads[0]
	if payload["title"] != "Codex 周额度已达 3% 以下" {
		t.Fatalf("title = %q", payload["title"])
	}
	if payload["priority"] != "high" || payload["tags"] != "codex,quota,weekly-low" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	for _, want := range []string{"a***@**.com", "周剩余: 3%", "通知阈值: 3%", formatTime(resetAt)} {
		if !strings.Contains(payload["message"], want) {
			t.Fatalf("message missing %q: %q", want, payload["message"])
		}
	}

	state, err := loadWeeklyResetReminderState(filepath.Join(stateDir, weeklyResetReminderStateFile))
	if err != nil {
		t.Fatal(err)
	}
	accountState := state.Accounts["account-a"]
	if accountState.NotifiedLowWeeklyCycleResetAt != resetAt {
		t.Fatalf("notified low weekly cycle reset = %d, want %d", accountState.NotifiedLowWeeklyCycleResetAt, resetAt)
	}
	if accountState.NotifiedLowWeeklyAt != now.Unix() {
		t.Fatalf("notified low weekly at = %d, want %d", accountState.NotifiedLowWeeklyAt, now.Unix())
	}
}

func TestLowWeeklyNotifierDoesNotSendAboveThreshold(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	now := time.Unix(1_780_000_000, 0)
	resetAt := now.Add(24 * time.Hour).Unix()
	writeTestAccountWithUsageUpdatedAt(t, dir, "account-a", "alice@example.com", lowWeeklyQuotaThreshold+1, resetAt, now.Unix())

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
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
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func TestLowWeeklyNotifierSendsAgainAfterWeeklyCycleRollsForward(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	now := time.Unix(1_780_000_000, 0)
	previousResetAt := now.Add(-time.Minute).Unix()
	currentResetAt := now.Add(7 * 24 * time.Hour).Unix()
	writeTestAccountWithUsageUpdatedAt(t, dir, "account-a", "alice@example.com", 2, currentResetAt, now.Unix())
	if err := saveWeeklyResetReminderState(filepath.Join(stateDir, weeklyResetReminderStateFile), weeklyResetReminderState{
		Accounts: map[string]weeklyResetAccountState{
			"account-a": {
				ObservedResetAt:               previousResetAt,
				NotifiedResetAt:               previousResetAt,
				NotifiedLowWeeklyCycleResetAt: previousResetAt,
				NotifiedLowWeeklyAt:           now.Add(-time.Hour).Unix(),
			},
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
	if gotPayload["tags"] != "codex,quota,weekly-low" {
		t.Fatalf("unexpected payload: %+v", gotPayload)
	}

	state, err := loadWeeklyResetReminderState(filepath.Join(stateDir, weeklyResetReminderStateFile))
	if err != nil {
		t.Fatal(err)
	}
	accountState := state.Accounts["account-a"]
	if accountState.NotifiedLowWeeklyCycleResetAt != currentResetAt {
		t.Fatalf("notified low weekly cycle reset = %d, want %d", accountState.NotifiedLowWeeklyCycleResetAt, currentResetAt)
	}
}

func TestLowWeeklyNotifierIgnoresResetTimeDriftWithinCycle(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	now := time.Unix(1_780_000_000, 0)
	observedResetAt := now.Add(7 * 24 * time.Hour).Unix()
	currentResetAt := observedResetAt + 1
	writeTestAccountWithUsageUpdatedAt(t, dir, "account-a", "alice@example.com", 2, currentResetAt, now.Unix())
	if err := saveWeeklyResetReminderState(filepath.Join(stateDir, weeklyResetReminderStateFile), weeklyResetReminderState{
		Accounts: map[string]weeklyResetAccountState{
			"account-a": {
				ObservedResetAt:               observedResetAt,
				NotifiedLowWeeklyCycleResetAt: observedResetAt,
				NotifiedLowWeeklyAt:           now.Add(-time.Hour).Unix(),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
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
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func TestStaleNotifierSendsWebhookWhenAccountStopsRefreshing(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	now := time.Unix(1_780_000_000, 0)
	usageUpdatedAt := now.Add(-31 * time.Minute).Unix()
	hourlyResetAt := usageUpdatedAt + int64(5*time.Hour/time.Second)
	weeklyResetAt := now.Add(24 * time.Hour).Unix()
	writeTestAccountWithUsageUpdatedAt(t, dir, "account-a", "alice@example.com", 21, weeklyResetAt, usageUpdatedAt)

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

	if gotPayload["title"] != "Codex 额度缓存已过期" {
		t.Fatalf("title = %q", gotPayload["title"])
	}
	if gotPayload["priority"] != "high" || gotPayload["tags"] != "codex,quota,stale" {
		t.Fatalf("unexpected payload: %+v", gotPayload)
	}
	for _, want := range []string{
		"a***@**.com",
		"Plus",
		"oauth",
		"80%",
		"21%",
		formatTime(hourlyResetAt),
		formatTime(weeklyResetAt),
		formatTime(usageUpdatedAt),
		"30m",
		"stale",
	} {
		if !strings.Contains(gotPayload["message"], want) {
			t.Fatalf("message missing %q: %q", want, gotPayload["message"])
		}
	}

	state, err := loadWeeklyResetReminderState(filepath.Join(stateDir, weeklyResetReminderStateFile))
	if err != nil {
		t.Fatal(err)
	}
	accountState := state.Accounts["account-a"]
	if accountState.NotifiedStaleUsageUpdatedAt != usageUpdatedAt {
		t.Fatalf("notified stale usage update = %d, want %d", accountState.NotifiedStaleUsageUpdatedAt, usageUpdatedAt)
	}
	if accountState.NotifiedStaleAt != now.Unix() {
		t.Fatalf("notified stale at = %d, want %d", accountState.NotifiedStaleAt, now.Unix())
	}
	if accountState.ObservedResetAt != weeklyResetAt {
		t.Fatalf("observed reset = %d, want %d", accountState.ObservedResetAt, weeklyResetAt)
	}
}

func TestStaleNotifierDoesNotSendDuplicateWebhookForSameUsageUpdate(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	now := time.Unix(1_780_000_000, 0)
	usageUpdatedAt := now.Add(-31 * time.Minute).Unix()
	weeklyResetAt := now.Add(24 * time.Hour).Unix()
	writeTestAccountWithUsageUpdatedAt(t, dir, "account-a", "alice@example.com", 21, weeklyResetAt, usageUpdatedAt)
	if err := saveWeeklyResetReminderState(filepath.Join(stateDir, weeklyResetReminderStateFile), weeklyResetReminderState{
		Accounts: map[string]weeklyResetAccountState{
			"account-a": {
				ObservedResetAt:             weeklyResetAt,
				NotifiedStaleUsageUpdatedAt: usageUpdatedAt,
				NotifiedStaleAt:             now.Add(-time.Minute).Unix(),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
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
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func TestStaleNotifierSendsAgainAfterUsageRefreshBecomesStale(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	now := time.Unix(1_780_000_000, 0)
	oldUsageUpdatedAt := now.Add(-2 * time.Hour).Unix()
	newUsageUpdatedAt := now.Add(-31 * time.Minute).Unix()
	weeklyResetAt := now.Add(24 * time.Hour).Unix()
	writeTestAccountWithUsageUpdatedAt(t, dir, "account-a", "alice@example.com", 34, weeklyResetAt, newUsageUpdatedAt)
	if err := saveWeeklyResetReminderState(filepath.Join(stateDir, weeklyResetReminderStateFile), weeklyResetReminderState{
		Accounts: map[string]weeklyResetAccountState{
			"account-a": {
				ObservedResetAt:             weeklyResetAt,
				NotifiedStaleUsageUpdatedAt: oldUsageUpdatedAt,
				NotifiedStaleAt:             now.Add(-time.Hour).Unix(),
			},
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
	if gotPayload["title"] != "Codex 额度缓存已过期" {
		t.Fatalf("title = %q", gotPayload["title"])
	}
	if !strings.Contains(gotPayload["message"], formatTime(newUsageUpdatedAt)) {
		t.Fatalf("message missing refreshed usage update: %q", gotPayload["message"])
	}

	state, err := loadWeeklyResetReminderState(filepath.Join(stateDir, weeklyResetReminderStateFile))
	if err != nil {
		t.Fatal(err)
	}
	accountState := state.Accounts["account-a"]
	if accountState.NotifiedStaleUsageUpdatedAt != newUsageUpdatedAt {
		t.Fatalf("notified stale usage update = %d, want %d", accountState.NotifiedStaleUsageUpdatedAt, newUsageUpdatedAt)
	}
}

func TestStaleNotifierSendsWebhookWithoutWeeklyWindow(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	now := time.Unix(1_780_000_000, 0)
	usageUpdatedAt := now.Add(-31 * time.Minute).Unix()
	writeTestAccountWithoutWeeklyWindow(t, dir, "account-a", "alice@example.com", usageUpdatedAt)

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

	if gotPayload["title"] != "Codex 额度缓存已过期" {
		t.Fatalf("title = %q", gotPayload["title"])
	}
	for _, want := range []string{"周剩余: -", "周重置: -"} {
		if !strings.Contains(gotPayload["message"], want) {
			t.Fatalf("message missing %q: %q", want, gotPayload["message"])
		}
	}

	state, err := loadWeeklyResetReminderState(filepath.Join(stateDir, weeklyResetReminderStateFile))
	if err != nil {
		t.Fatal(err)
	}
	accountState := state.Accounts["account-a"]
	if accountState.NotifiedStaleUsageUpdatedAt != usageUpdatedAt {
		t.Fatalf("notified stale usage update = %d, want %d", accountState.NotifiedStaleUsageUpdatedAt, usageUpdatedAt)
	}
	if accountState.ObservedResetAt != 0 {
		t.Fatalf("observed reset = %d, want 0", accountState.ObservedResetAt)
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

func TestFormatTimeAcceptsSecondsAndMilliseconds(t *testing.T) {
	const seconds = int64(1_784_377_842)
	if got, want := formatTime(seconds*1000), formatTime(seconds); got != want {
		t.Fatalf("millisecond label = %q, second label = %q", got, want)
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
	for _, want := range []string{
		"data-refresh-seconds=\"300\"",
		"模型请求排行",
		"gpt-5-codex",
		"m***@**.com",
		"生成 2026-06-04 13:30:00",
		"var refreshStarted = false",
		"if (refreshStarted) return",
		"clearInterval(refreshTimer)",
	} {
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
	writeTestAccountWithUsageUpdatedAt(t, dir, id, email, weeklyRemaining, weeklyResetAt, time.Now().Unix())
}

func writeTestAccountWithUsageUpdatedAt(t *testing.T, dir, id, email string, weeklyRemaining int, weeklyResetAt, usageUpdatedAt int64) {
	t.Helper()
	accountsDir := filepath.Join(dir, "codex_accounts")
	if err := os.MkdirAll(accountsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hourlyResetAt := usageUpdatedAt + int64(5*time.Hour/time.Second)
	content := `{
		"email": "` + email + `",
		"auth_mode": "oauth",
		"plan_type": "Plus",
		"quota": {
			"hourly_percentage": 80,
			"hourly_reset_time": ` + strconv.FormatInt(hourlyResetAt, 10) + `,
			"hourly_window_minutes": 300,
			"hourly_window_present": true,
			"weekly_percentage": ` + strconv.Itoa(weeklyRemaining) + `,
			"weekly_reset_time": ` + strconv.FormatInt(weeklyResetAt, 10) + `,
			"weekly_window_minutes": 10080,
			"weekly_window_present": true
		},
		"usage_updated_at": ` + strconv.FormatInt(usageUpdatedAt, 10) + `
	}`
	if err := os.WriteFile(filepath.Join(accountsDir, id+".json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTestAccountWithoutWeeklyWindow(t *testing.T, dir, id, email string, usageUpdatedAt int64) {
	t.Helper()
	accountsDir := filepath.Join(dir, "codex_accounts")
	if err := os.MkdirAll(accountsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hourlyResetAt := usageUpdatedAt + int64(5*time.Hour/time.Second)
	content := `{
		"email": "` + email + `",
		"auth_mode": "oauth",
		"plan_type": "Plus",
		"quota": {
			"hourly_percentage": 80,
			"hourly_reset_time": ` + strconv.FormatInt(hourlyResetAt, 10) + `,
			"hourly_window_minutes": 300,
			"hourly_window_present": true,
			"weekly_window_present": false
		},
		"usage_updated_at": ` + strconv.FormatInt(usageUpdatedAt, 10) + `
	}`
	if err := os.WriteFile(filepath.Join(accountsDir, id+".json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeWeeklyResetAccountStateRaw(t *testing.T, stateDir, accountKey string, accountState map[string]any) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{
		"accounts": map[string]any{
			accountKey: accountState,
		},
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, weeklyResetReminderStateFile), append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readWeeklyResetAccountStateRaw(t *testing.T, stateDir, accountKey string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stateDir, weeklyResetReminderStateFile))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Accounts map[string]map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	accountState, ok := state.Accounts[accountKey]
	if !ok {
		t.Fatalf("state missing account %q: %+v", accountKey, state.Accounts)
	}
	return accountState
}

func requireStateInt64(t *testing.T, accountState map[string]any, field string) int64 {
	t.Helper()
	value, ok := accountState[field]
	if !ok {
		t.Fatalf("state missing %q: %+v", field, accountState)
	}
	number, ok := value.(float64)
	if !ok {
		t.Fatalf("state field %q = %T(%v), want number", field, value, value)
	}
	return int64(number)
}

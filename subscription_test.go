package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSubscriptionExpirationFormats(t *testing.T) {
	want := time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC).Unix()
	for name, raw := range map[string]string{
		"ISO UTC":      `"2026-11-01T08:30:00Z"`,
		"ISO offset":   `"2026-11-01T16:30:00+08:00"`,
		"ISO fraction": `"2026-11-01T08:30:00.123Z"`,
		"seconds":      `"` + strconv.FormatInt(want, 10) + `"`,
		"milliseconds": `"` + strconv.FormatInt(want*1000, 10) + `"`,
		"JSON number":  strconv.FormatInt(want, 10),
		"SQL date":     `"2026-11-01 08:30:00"`,
	} {
		t.Run(name, func(t *testing.T) {
			got := parseSubscriptionExpiresAt(json.RawMessage(raw))
			if got == nil || *got != want {
				t.Fatalf("expiration = %v, want Unix seconds %d", got, want)
			}
		})
	}

	dateOnly := parseSubscriptionExpiresAt(json.RawMessage(`"2026-11-01"`))
	if dateOnly == nil || *dateOnly != time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("date-only expiration = %v", dateOnly)
	}
}

func TestSubscriptionExpirationBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		remaining time.Duration
		days      int
		label     string
		class     string
		expired   bool
	}{
		{"30 days", 30 * 24 * time.Hour, 30, "剩余 30 天", "ok", false},
		{"over seven days", 7*24*time.Hour + time.Second, 8, "剩余 8 天", "ok", false},
		{"seven days", 7 * 24 * time.Hour, 7, "剩余 7 天", "warn", false},
		{"one day", 24 * time.Hour, 1, "剩余 1 天", "warn", false},
		{"under one day", 5 * time.Hour, 1, "不足 1 天", "warn", false},
		{"last second", time.Second, 1, "不足 1 天", "warn", false},
		{"expires now", 0, 0, "已到期", "danger", true},
		{"expired", -time.Hour, 0, "已到期", "danger", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expires := now.Add(tc.remaining)
			raw, _ := json.Marshal(expires.Format(time.RFC3339))
			view := buildSubscription(raw, now)
			if !view.Known || view.ExpiresAt == nil || *view.ExpiresAt != expires.Unix() {
				t.Fatalf("missing expiration: %+v", view)
			}
			if view.DaysRemaining != tc.days || view.RemainingLabel != tc.label || view.Class != tc.class || view.Expired != tc.expired {
				t.Fatalf("unexpected presentation: %+v", view)
			}
			if view.ExpiresLabel != formatTime(expires.Unix()) {
				t.Fatalf("absolute date = %q", view.ExpiresLabel)
			}
		})
	}
}

func TestSubscriptionUnknownDoesNotInferTokenOrQuotaExpiration(t *testing.T) {
	now := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	for _, raw := range []string{
		`null`, `""`, `"not-a-date"`, `0`, `-1`, `"0"`, `true`, `{}`, `[]`,
		`"999999999999999999999999999"`, `9223372036854775807`,
	} {
		t.Run(raw, func(t *testing.T) {
			content := `{"email":"alice@example.com","subscription_active_until":` + raw + `,
				"expires_at":1793521800,"tokens":{"expires_at":1793521800},
				"quota":{"hourly_percentage":85,"hourly_reset_time":1793521800}}`
			var account rawAccount
			if err := json.Unmarshal([]byte(content), &account); err != nil {
				t.Fatalf("invalid optional expiration must not hide account: %v", err)
			}
			view := account.toViewAt(time.Hour, now)
			if view.Subscription.Known || view.Subscription.ExpiresAt != nil || view.Subscription.Expired || view.Subscription.Class != "unknown" {
				t.Fatalf("must remain unknown: %+v", view.Subscription)
			}
			if view.Hourly.Remaining != 85 {
				t.Fatalf("valid quota discarded: %+v", view.Hourly)
			}
		})
	}
	var absent rawAccount
	if view := absent.toViewAt(time.Hour, now); view.Subscription.Known || view.Subscription.ExpiresAt != nil {
		t.Fatalf("missing subscription must be unknown: %+v", view.Subscription)
	}
}

func TestLoadAccountsSubscriptionPlaintextAndEncrypted(t *testing.T) {
	now := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	plaintext := []byte(`{
		"id":"private-account-id","email":"alice@example.com","plan_type":"Plus",
		"subscription_active_until":"2026-11-01T08:00:00Z",
		"tokens":{"access_token":"private-access-token"},
		"quota":{"hourly_percentage":75,"hourly_window_present":true}
	}`)
	for _, encrypted := range []bool{false, true} {
		t.Run(strconv.FormatBool(encrypted), func(t *testing.T) {
			dir := t.TempDir()
			accountsDir := filepath.Join(dir, "codex_accounts")
			if err := os.MkdirAll(accountsDir, 0o700); err != nil {
				t.Fatal(err)
			}
			content := plaintext
			if encrypted {
				key := bytes.Repeat([]byte{0x42}, 32)
				if err := os.WriteFile(filepath.Join(dir, secureAccountKeyFile), []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
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
				content, err = json.Marshal(secureAccountEnvelope{
					Version: secureAccountVersion, Kind: "codex", Algorithm: "AES-256-GCM",
					Nonce:      base64.StdEncoding.EncodeToString(nonce),
					Ciphertext: base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, plaintext, nil)),
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(accountsDir, "private-account-id.json"), content, 0o600); err != nil {
				t.Fatal(err)
			}
			accounts, err := loadAccountsAt(config{DataDir: dir, StaleAfter: time.Hour}, now)
			if err != nil || len(accounts) != 1 {
				t.Fatalf("load accounts: count=%d, error=%v", len(accounts), err)
			}
			account := accounts[0]
			if !account.Subscription.Known || account.Subscription.DaysRemaining != 30 || account.Email != "a***@**.com" {
				t.Fatalf("unexpected subscription view: %+v", account)
			}
			encoded, err := json.Marshal(account)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"alice@example.com", "private-account-id", "private-access-token", "tokens", "subscription_active_until"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("sanitized view leaked %q", secret)
				}
			}
		})
	}
}

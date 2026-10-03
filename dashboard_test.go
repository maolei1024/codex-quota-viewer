package main

import (
	"bytes"
	"encoding/json"
	"html/template"
	"strings"
	"testing"
	"time"
)

func TestDashboardRendersQuotaAndExpiryWithoutJavaScript(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	reset := now.Add(5 * time.Hour).UnixMilli()
	weeklyPresent := false
	account := rawAccount{
		Email:                   "alice@example.com",
		PlanType:                "Plus",
		SubscriptionActiveUntil: json.RawMessage(`"2026-11-01T12:00:00Z"`),
		Quota: &rawQuota{
			HourlyPercentage:    72,
			HourlyResetTime:     &reset,
			WeeklyWindowPresent: &weeklyPresent,
		},
	}.toViewAt(defaultStaleAfter, now)
	account.AccountKey = "private-account-key"
	account.Error = "<script>alert('private')</script>"
	summary := summaryView{GeneratedAt: now.Unix(), Accounts: []accountView{account}}
	tmpl := template.Must(template.New("dashboard").Funcs(dashboardFuncs()).Parse(dashboardHTML))
	var out bytes.Buffer
	if err := tmpl.Execute(&out, summary); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	// Core account information must be server-rendered even when scripts fail or
	// are disabled, while secondary usage stays collapsed until requested.
	for _, want := range []string{"72<small>%</small>", "5小时后重置", "剩余 30 天", account.Subscription.ExpiresLabel, `<details class="secondary" id="usage-details">`} {
		if !strings.Contains(html, want) {
			t.Errorf("server-rendered page missing %q", want)
		}
	}
	if strings.Contains(html, "周额度") || strings.Contains(html, "暂无此额度窗口") {
		t.Error("page should omit quota windows that are not present")
	}
	for _, private := range []string{"alice@example.com", "private-account-key", "<script>alert("} {
		if strings.Contains(html, private) {
			t.Errorf("page exposes unescaped or private value %q", private)
		}
	}
}

func TestResetCountdownHandlesTimestampUnitsAndBoundaries(t *testing.T) {
	const generatedAt = int64(1790906400)
	tests := []struct {
		name        string
		resetAt     int64
		generatedAt int64
		want        string
	}{
		{"zero", 0, generatedAt, "重置时间未知"},
		{"negative", -1, generatedAt, "重置时间未知"},
		{"past", generatedAt - 1, generatedAt, "已到重置时间，等待同步"},
		{"due now", generatedAt, generatedAt, "已到重置时间，等待同步"},
		{"seconds", generatedAt + 59, generatedAt, "不到 1 分钟后重置"},
		{"one minute", generatedAt + 60, generatedAt, "1分钟后重置"},
		{"minutes", generatedAt + 3599, generatedAt, "59分钟后重置"},
		{"exact hours", generatedAt + 5*3600, generatedAt, "5小时后重置"},
		{"hours and minutes", generatedAt + 5*3600 + 23*60, generatedAt, "5小时 23分钟后重置"},
		{"exact days", generatedAt + 7*86400, generatedAt, "7天后重置"},
		{"days and hours", generatedAt + 2*86400 + 3*3600, generatedAt, "2天 3小时后重置"},
		{"milliseconds", (generatedAt+300)*1000 + 999, generatedAt, "5分钟后重置"},
		{"millisecond reference", (generatedAt + 300) * 1000, generatedAt * 1000, "5分钟后重置"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resetCountdown(&tt.resetAt, tt.generatedAt); got != tt.want {
				t.Fatalf("countdown = %q, want %q", got, tt.want)
			}
		})
	}
	if got := resetCountdown(nil, generatedAt); got != "重置时间未知" {
		t.Fatalf("missing reset countdown = %q", got)
	}
}

func TestTimestampNormalizesBrowserDates(t *testing.T) {
	for _, raw := range []int64{0, -1} {
		if got := timestamp(&raw); got != 0 {
			t.Fatalf("timestamp(%d) = %d, want 0", raw, got)
		}
	}
	if got := timestamp(nil); got != 0 {
		t.Fatalf("timestamp(nil) = %d, want 0", got)
	}
	for _, raw := range []int64{1790906400, 1790906400999} {
		if got := timestamp(&raw); got != 1790906400 {
			t.Fatalf("timestamp(%d) = %d", raw, got)
		}
	}
}

func TestWindowLabelUsesActualQuotaDuration(t *testing.T) {
	tests := []struct {
		window quotaWindow
		want   string
	}{
		{quotaWindow{Minutes: 300}, "5 小时额度"},
		{quotaWindow{Minutes: 10080}, "7 天额度"},
		{quotaWindow{Minutes: 90}, "90 分钟额度"},
		{quotaWindow{Minutes: 60, Window: "168h"}, "1 小时额度"},
		{quotaWindow{Window: "168h"}, "7 天额度"},
		{quotaWindow{Window: "300m"}, "5 小时额度"},
		{quotaWindow{Window: "-"}, "短期额度"},
		{quotaWindow{Window: "30s"}, "短期额度"},
		{quotaWindow{}, "短期额度"},
	}
	for _, tt := range tests {
		if got := windowLabel(tt.window, "短期额度"); got != tt.want {
			t.Errorf("windowLabel(%+v) = %q, want %q", tt.window, got, tt.want)
		}
	}
}

func TestAccountAttentionAndStatePrecedence(t *testing.T) {
	tests := []struct {
		name      string
		account   accountView
		attention bool
		state     string
		class     string
		quota     int
	}{
		{"missing windows", accountView{}, false, "暂无额度", "muted", 101},
		{"healthy single window", accountView{Weekly: quotaWindow{Present: true, Remaining: 80}}, false, "正常", "ok", 80},
		{"low primary", accountView{Hourly: quotaWindow{Present: true, Remaining: 20}, Weekly: quotaWindow{Present: true, Remaining: 80}}, true, "额度不足", "danger", 20},
		{"low secondary", accountView{Hourly: quotaWindow{Present: true, Remaining: 80}, Weekly: quotaWindow{Present: true, Remaining: 0}}, true, "额度不足", "danger", 0},
		{"stale before low", accountView{Stale: true, Hourly: quotaWindow{Present: true, Remaining: 0}}, true, "缓存过期", "warn", 0},
		{"error before stale", accountView{Error: "unavailable", Stale: true}, true, "读取异常", "danger", 101},
		{"expired before low", accountView{Subscription: subscriptionView{Known: true, Expired: true}, Hourly: quotaWindow{Present: true, Remaining: 0}}, true, "套餐已到期", "danger", 0},
		{"seven days remaining", accountView{Subscription: subscriptionView{Known: true, DaysRemaining: 7}}, true, "套餐即将到期", "warn", 101},
		{"eight days remaining", accountView{Subscription: subscriptionView{Known: true, DaysRemaining: 8}, Weekly: quotaWindow{Present: true, Remaining: 80}}, false, "正常", "ok", 80},
		{"unknown expiry is not expired", accountView{Subscription: subscriptionView{DaysRemaining: 0}, Weekly: quotaWindow{Present: true, Remaining: 80}}, false, "正常", "ok", 80},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := accountNeedsAttention(tt.account); got != tt.attention {
				t.Errorf("attention = %v, want %v", got, tt.attention)
			}
			if got := accountState(tt.account); got != tt.state {
				t.Errorf("state = %q, want %q", got, tt.state)
			}
			if got := accountStateClass(tt.account); got != tt.class {
				t.Errorf("class = %q, want %q", got, tt.class)
			}
			if got := quotaSort(tt.account); got != tt.quota {
				t.Errorf("quota = %d, want %d", got, tt.quota)
			}
		})
	}
}

func TestAccountAliasDistinguishesIdenticallyMaskedAccounts(t *testing.T) {
	first := accountView{AccountKey: "private-account-a", Email: "a***@**.com"}
	second := accountView{AccountKey: "private-account-b", Email: "a***@**.com"}
	alias := accountAlias(first)
	if len(alias) != 6 || alias != strings.ToUpper(alias) || alias == accountAlias(second) {
		t.Fatalf("unexpected aliases: %q, %q", alias, accountAlias(second))
	}
	first.Email = "changed@masked.invalid"
	if got := accountAlias(first); got != alias {
		t.Fatalf("alias changed with email: %q != %q", got, alias)
	}
	if got := accountAlias(accountView{Email: "private@example.com"}); got != "" {
		t.Fatalf("missing key should not produce an email-derived alias: %q", got)
	}
}

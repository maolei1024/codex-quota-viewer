package main

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
)

// timestamp gives browser-facing dates one consistent unit without inventing a
// date when the source did not provide one.
func timestamp(value *int64) int64 {
	if value == nil || *value <= 0 {
		return 0
	}
	return unixTime(*value).Unix()
}

func resetCountdown(resetAt *int64, generatedAt int64) string {
	reset := timestamp(resetAt)
	if reset == 0 {
		return "重置时间未知"
	}
	remaining := reset - timestamp(&generatedAt)
	if remaining <= 0 {
		return "已到重置时间，等待同步"
	}
	if remaining < 60 {
		return "不到 1 分钟后重置"
	}
	minutes := remaining / 60
	if minutes >= 24*60 {
		days, hours := minutes/(24*60), (minutes/60)%24
		if hours == 0 {
			return fmt.Sprintf("%d天后重置", days)
		}
		return fmt.Sprintf("%d天 %d小时后重置", days, hours)
	}
	if minutes >= 60 {
		hours, minutes := minutes/60, minutes%60
		if minutes == 0 {
			return fmt.Sprintf("%d小时后重置", hours)
		}
		return fmt.Sprintf("%d小时 %d分钟后重置", hours, minutes)
	}
	return fmt.Sprintf("%d分钟后重置", minutes)
}

func windowLabel(window quotaWindow, fallback string) string {
	minutes := window.Minutes
	if minutes <= 0 {
		if duration, err := time.ParseDuration(window.Window); err == nil && duration > 0 && duration%time.Minute == 0 {
			minutes = int64(duration / time.Minute)
		}
	}
	switch {
	case minutes <= 0:
		return fallback
	case minutes%(24*60) == 0:
		return fmt.Sprintf("%d 天额度", minutes/(24*60))
	case minutes%60 == 0:
		return fmt.Sprintf("%d 小时额度", minutes/60)
	default:
		return fmt.Sprintf("%d 分钟额度", minutes)
	}
}

func accountNeedsAttention(account accountView) bool {
	return account.Error != "" || account.Stale || quotaSort(account) <= 20 ||
		(account.Subscription.Known && (account.Subscription.Expired || account.Subscription.DaysRemaining <= 7))
}

func accountState(account accountView) string {
	switch {
	case account.Error != "":
		return "读取异常"
	case account.Stale:
		return "缓存过期"
	case account.Subscription.Known && account.Subscription.Expired:
		return "套餐已到期"
	case quotaSort(account) <= 20:
		return "额度不足"
	case account.Subscription.Known && account.Subscription.DaysRemaining <= 7:
		return "套餐即将到期"
	case !account.Hourly.Present && !account.Weekly.Present:
		return "暂无额度"
	default:
		return "正常"
	}
}

func accountStateClass(account accountView) string {
	switch accountState(account) {
	case "读取异常", "套餐已到期", "额度不足":
		return "danger"
	case "缓存过期", "套餐即将到期":
		return "warn"
	case "暂无额度":
		return "muted"
	default:
		return "ok"
	}
}

// Missing windows must not be mistaken for an exhausted allowance.
func quotaSort(account accountView) int {
	remaining := 101
	for _, window := range []quotaWindow{account.Hourly, account.Weekly} {
		if window.Present {
			remaining = min(remaining, clamp(window.Remaining, 0, 100))
		}
	}
	return remaining
}

func accountAlias(account accountView) string {
	if account.AccountKey == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(account.AccountKey))
	return strings.ToUpper(fmt.Sprintf("%x", digest[:3]))
}

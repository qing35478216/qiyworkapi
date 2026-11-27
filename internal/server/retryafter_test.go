package server

import (
	"testing"
	"time"

	"github.com/qing35478216/qiyworkapi/internal/auth"
	"github.com/qing35478216/qiyworkapi/internal/pool"
	"github.com/qing35478216/qiyworkapi/internal/upstream"
)

// TestRetryAfterSeconds 覆盖 429 / 模型封锁时 Retry-After 的取值优先级：
// 上游明示 > 池内最早解冻 > 兜底 60s；auth_dead 账号不参与（等它没用，要重新登录）。
func TestRetryAfterSeconds(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	// 没有任何观测 → 60s 兜底（不是 0：不带头会让客户端 1s 猛重试）。
	if got := h.retryAfterSeconds(nil, pool.ModelBlockStatus{}); got != 60 {
		t.Errorf("no signal = %d want 60", got)
	}

	// 上游明示 30s → 31（向上取整到秒 + 1s 缓冲）。
	if got := h.retryAfterSeconds(&upstream.Error{RetryAfter: 30 * time.Second}, pool.ModelBlockStatus{}); got != 30 {
		t.Errorf("upstream retry-after = %d want 30", got)
	}

	// 池内账号软冷却 600s → 约 600s。
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit")
	got := h.retryAfterSeconds(nil, pool.ModelBlockStatus{})
	if got < 590 || got > 601 {
		t.Errorf("pool cooldown = %d want ~600", got)
	}

	// 模型封锁 120s < 池冷却 600s → 取更早的 120s（客户端能更早换模型重试）。
	mb := pool.ModelBlockStatus{Blocked: true, Until: time.Now().Add(120 * time.Second)}
	got = h.retryAfterSeconds(nil, mb)
	if got < 118 || got > 121 {
		t.Errorf("model block = %d want ~120", got)
	}

	// 上游明示 5s 最小 → 压过池冷却与模型封锁。
	if got := h.retryAfterSeconds(&upstream.Error{RetryAfter: 5 * time.Second}, mb); got != 5 {
		t.Errorf("upstream 5s = %d want 5", got)
	}
}

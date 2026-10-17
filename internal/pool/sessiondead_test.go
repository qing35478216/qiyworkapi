package pool

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/qing35478216/qiyworkapi/internal/auth"
)

// TestNoteSessionDeadThresholdNotReached 前 2 次连续 12153 不 Disable（误判防护）。
// 但第 1 次即打 auth_dead 软暂停：选号跳过（不让每个请求挨个撞死号），
// 与 disabled 是两回事——30 分钟到期自动复探，刷新成功即清除。
func TestNoteSessionDeadThresholdNotReached(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if p.NoteSessionDead("u1") {
		t.Fatal("第 1 次 12153 不应禁用")
	}
	if p.NoteSessionDead("u1") {
		t.Fatal("第 2 次 12153 不应禁用")
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.Disabled {
		t.Fatalf("连续 2 次 12153 不应禁用: %+v", st)
	}
	// 新语义：第 1 次即软暂停选号（auth_dead），但不是禁用。
	if !st.AuthDead {
		t.Fatalf("第 1 次 12153 后应打 auth_dead 软暂停: %+v", st)
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("auth_dead 软暂停期间账号不应可选, got %+v", got)
	}
}

// TestNoteSessionDeadDisablesAtThird 连续第 3 次 12153 → 禁用并清计数。
func TestNoteSessionDeadDisablesAtThird(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	if !p.NoteSessionDead("u1") {
		t.Fatal("第 3 次 12153 应禁用")
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("第 3 次后应 disabled: %+v", st)
	}
	if st.Reason != "12153 session dead" {
		t.Errorf("reason=%q want 12153 session dead", st.Reason)
	}
	// 禁用后不再可选。
	if p.Pick() != nil {
		t.Fatal("禁用账号不可被选中")
	}
}

// TestClearSessionDeadResetsCount 中间成功（refresh 成功）清计数，后续从 1 重新计。
func TestClearSessionDeadResetsCount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.ClearSessionDead("u1") // 模拟 refresh 成功：清计数
	if p.NoteSessionDead("u1") {
		t.Fatal("清计数后第 1 次不应禁用")
	}
	if p.NoteSessionDead("u1") {
		t.Fatal("清计数后第 2 次不应禁用")
	}
	if !p.NoteSessionDead("u1") {
		t.Fatal("清计数后第 3 次应禁用（从 1 重新计够 3 次）")
	}
}

// TestNoteSuccessClearsSessionDeadCount 任意成功（chat 成功）也是 session 未死的强证据，
// 同样清计数——与 refresh 成功口径一致。
func TestNoteSuccessClearsSessionDeadCount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.NoteSuccess("u1")
	if p.NoteSessionDead("u1") {
		t.Fatal("成功清计数后第 1 次不应禁用")
	}
}

// TestReviveDisabled 复活入口：清 disabled + reason + 误判计数，账号回到池子。
func TestReviveDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1") // 触发禁用
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("precondition: 应已禁用")
	}
	p.ReviveDisabled("u1")
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.Disabled {
		t.Fatalf("revive 应清 disabled: %+v", st)
	}
	if st.Reason != "" {
		t.Errorf("reason=%q want 空（revive 清 reason）", st.Reason)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("复活后账号应回到池子, got %+v", got)
	}
	// 误判计数一并清零：复活后重新计满 3 次才禁用。
	p.NoteSessionDead("u1")
	if p.NoteSessionDead("u1") {
		t.Fatal("复活后第 2 次不应禁用（应从新计数）")
	}
	if !p.NoteSessionDead("u1") {
		t.Fatal("复活后第 3 次应禁用（从新计数够 3 次）")
	}
}

// TestReviveDisabledPersists 复活清 disabled + reason 落盘持久化（重启后不回退）。
func TestReviveDisabledPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.ReviveDisabled("u1")
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Disabled || st.Reason != "" {
		t.Fatalf("revive 应持久化（disabled=%v reason=%q）ok=%v", st.Disabled, st.Reason, ok)
	}
}

// TestStatusDisabledReasonDisabled when disabled, Status 透出 disabled_reason。
func TestStatusDisabledReasonDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	st, _ := p.Status("u1")
	if !st.Disabled || st.DisabledReason != "12153 session dead" {
		t.Errorf("disabled_reason=%q want 12153 session dead (disabled=%v)", st.DisabledReason, st.Disabled)
	}
}

// TestStatusDisabledReasonClearedByRevive 复活后 disabled_reason 归空。
func TestStatusDisabledReasonClearedByRevive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.ReviveDisabled("u1")
	st, _ := p.Status("u1")
	if st.DisabledReason != "" {
		t.Errorf("revive 后 disabled_reason=%q want 空", st.DisabledReason)
	}
	if st.Reason != "" {
		t.Errorf("revive 后 reason=%q want 空", st.Reason)
	}
}

// TestMarkAuthDeadPausesAndExpires 软暂停：MarkAuthDead 后选号跳过，到期自动复探。
func TestMarkAuthDeadPausesAndExpires(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.MarkAuthDead("u1")
	st, _ := p.Status("u1")
	if !st.AuthDead || st.AuthDeadRemaining <= 0 {
		t.Fatalf("MarkAuthDead 后应处于软暂停: %+v", st)
	}
	if p.Pick() != nil {
		t.Fatal("软暂停期间不应被选中")
	}
	// 到期（把截止拨到过去）→ 自动回池。
	p.mu.Lock()
	p.byUID["u1"].authDeadUntil = time.Now().Add(-time.Second)
	p.mu.Unlock()
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("到期后应回池, got %+v", got)
	}
	if st, _ := p.Status("u1"); st.AuthDead {
		t.Fatalf("到期后 AuthDead 应为 false: %+v", st)
	}
}

// TestAuthDeadClearedByRefreshSuccess 刷新成功（保活路径）清除软暂停。
func TestAuthDeadClearedByRefreshSuccess(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.MarkAuthDead("u1")
	if p.Pick() != nil {
		t.Fatal("precondition: 软暂停中应不可选")
	}
	p.ClearSessionDead("u1") // RunKeepaliveNow 刷新成功走这里
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("刷新成功后应回池, got %+v", got)
	}
	if st, _ := p.Status("u1"); st.AuthDead {
		t.Fatal("刷新成功后 AuthDead 应清除")
	}
}

// TestAuthDeadClearedByChatSuccess 聊天成功（NoteSuccess）同样清除软暂停。
func TestAuthDeadClearedByChatSuccess(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	if p.Pick() != nil {
		t.Fatal("precondition: 软暂停中应不可选")
	}
	p.NoteSuccess("u1")
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("聊天成功后应回池, got %+v", got)
	}
}

// TestAuthDeadClearedByRelogin 重新登录（令牌变更经 upsert）清除软暂停与计数——
// 用户重新授权后无需等 30 分钟或人工解冻。
func TestAuthDeadClearedByRelogin(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", RefreshToken: "rt-old"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	if p.Pick() != nil {
		t.Fatal("precondition: 软暂停中应不可选")
	}
	// 重新登录：同 uid、新 refreshToken（面板添加账号 / login.sh 的落盘路径）。
	p.Add(&auth.Auth{UID: "u1", RefreshToken: "rt-new"})
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("重新登录后应回池, got %+v", got)
	}
	st, _ := p.Status("u1")
	if st.AuthDead {
		t.Fatal("重新登录后 AuthDead 应清除")
	}
	// 计数也清零：重新登录后要重新计满 3 次才禁用。
	if p.NoteSessionDead("u1") {
		t.Fatal("重新登录后第 1 次不应禁用")
	}
}

// TestAuthDeadSameTokenNoReset 令牌不变的 SyncToDir 不误触恢复（否则暂停白打）。
func TestAuthDeadSameTokenNoReset(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", RefreshToken: "rt-same"})
	p.MarkAuthDead("u1")
	// 周期性重扫：同 uid 同 token（模拟启动 SyncToDir / 目录重扫）。
	p.Add(&auth.Auth{UID: "u1", RefreshToken: "rt-same"})
	if got := p.Pick(); got != nil {
		t.Fatalf("令牌未变时软暂停不应被清除, got %+v", got)
	}
}

// TestAuthDeadPersists 软暂停落盘：重启后死号不放回轮换（过期才恢复）。
func TestAuthDeadPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.MarkAuthDead("u1")
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || !st.AuthDead {
		t.Fatalf("软暂停应持久化（重启后仍暂停）: ok=%v st=%+v", ok, st)
	}
	if p2.Pick() != nil {
		t.Fatal("重启后软暂停中的账号不应回池")
	}
}

// TestCountAuthDead 汇总计数与 cooling 口径分开。
func TestCountAuthDead(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	p.MarkAuthDead("u1")
	p.NoteSessionDead("u2") // 走 NoteSessionDead 也打软暂停
	if got := p.CountAuthDead(); got != 2 {
		t.Fatalf("CountAuthDead=%d want 2", got)
	}
	p.ClearSessionDead("u1")
	if got := p.CountAuthDead(); got != 1 {
		t.Fatalf("清除一个后 CountAuthDead=%d want 1", got)
	}
}

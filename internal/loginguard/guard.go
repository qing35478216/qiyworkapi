// Package loginguard 给账号登录加「错峰闸门」。
//
// 背景：上游对短时间内的批量登录很敏感。2026-11-27 的事故里，14 个 CN 账号在
// 11-26 12:20 / 22:00 两个时间点集中登录，不到 24 小时就被上游整批吊销
// refresh token（idsvc 12153 invalid_grant: refresh token revoked），
// access token 虽然名义上还没到期，但网关已直接 401。
//
// 本包不碰 token，只回答两个问题：
//  1. 现在能不能再登录一个账号？（Check）
//  2. 刚刚登录成功了一个账号，记下来。（Record）
//
// 历史落在 data/login-history.json，面板（进程内）与 cmd/login（CLI）共用同一份，
// 因此从哪个入口登录都受同样的节奏约束。写入用「临时文件 + rename」保证原子性，
// 跨进程用 O_EXCL 锁文件串行化，避免面板和 CLI 同时记录时互相覆盖。
package loginguard

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// historyKeep 历史保留窗口：只用于节流与排障，30 天足够，且不会让文件无限增长。
	historyKeep = 30 * 24 * time.Hour
	// historyMax 条目上限兜底（正常一天最多几十条，500 条足够）。
	historyMax = 500
	// lockStale 锁文件超过该时长视为残留（进程崩溃留下的），自动回收。
	lockStale = 15 * time.Second
	// lockWait 抢锁最长时间：只覆盖一次读改写，正常在毫秒级完成。
	lockWait = 3 * time.Second
)

// Entry 一次成功登录的记录。
type Entry struct {
	At    time.Time `json:"at"`
	UID   string    `json:"uid,omitempty"`
	Realm string    `json:"realm,omitempty"`
}

// Decision 闸门判定结果。Allowed=false 时 Wait 表示还需等待多久，Reason 是可读原因。
type Decision struct {
	Allowed bool
	Wait    time.Duration
	NextAt  time.Time
	Reason  string
}

// Guard 登录错峰闸门。零值不可用；Path 为空时视为「未启用」（放行且不记录），
// 便于测试或未配置历史路径的嵌入场景。
type Guard struct {
	Path string

	// MinInterval 两次成功登录之间的最小间隔；<=0 表示不限制。
	MinInterval time.Duration
	// MaxPerHour 任意滚动 1 小时内的登录次数上限；<=0 表示不限制。
	MaxPerHour int
	// MaxPerDay 任意滚动 24 小时内的登录次数上限；<=0 表示不限制。
	MaxPerDay int

	// Disabled 应急开关：true 时完全放行（不读不写历史）。
	Disabled bool

	// Now 便于测试注入时钟；nil 用 time.Now。
	Now func() time.Time
}

func (g *Guard) now() time.Time {
	if g != nil && g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Check 判断此刻能否再发起一次登录。
//
// 读历史失败时选择放行而不是阻断：闸门是「防批量」的辅助手段，不能因为一个
// 损坏的 JSON 把用户锁死在门外；调用方会照常记录日志。
func (g *Guard) Check() Decision {
	if g == nil || g.Disabled || g.Path == "" {
		return Decision{Allowed: true}
	}
	entries, err := g.readEntries()
	if err != nil || len(entries) == 0 {
		return Decision{Allowed: true}
	}
	now := g.now()

	var wait time.Duration
	var reasons []string

	if g.MinInterval > 0 {
		last := entries[len(entries)-1].At
		if elapsed := now.Sub(last); elapsed >= 0 && elapsed < g.MinInterval {
			w := g.MinInterval - elapsed
			wait = maxDuration(wait, w)
			if elapsed < time.Second {
				reasons = append(reasons,
					fmt.Sprintf("刚刚登录过（要求间隔≥%s）", HumanDuration(g.MinInterval)))
			} else {
				reasons = append(reasons,
					fmt.Sprintf("距上次登录 %s（要求间隔≥%s）", HumanDuration(elapsed), HumanDuration(g.MinInterval)))
			}
		}
	}

	if g.MaxPerHour > 0 {
		if w, hit := windowWait(entries, now, time.Hour, g.MaxPerHour); hit {
			wait = maxDuration(wait, w)
			reasons = append(reasons, fmt.Sprintf("最近 1 小时已登录 %d 个（上限 %d 个）", g.MaxPerHour, g.MaxPerHour))
		}
	}

	if g.MaxPerDay > 0 {
		if w, hit := windowWait(entries, now, 24*time.Hour, g.MaxPerDay); hit {
			wait = maxDuration(wait, w)
			reasons = append(reasons, fmt.Sprintf("最近 24 小时已登录 %d 个（上限 %d 个）", g.MaxPerDay, g.MaxPerDay))
		}
	}

	if wait <= 0 {
		return Decision{Allowed: true}
	}
	return Decision{
		Allowed: false,
		Wait:    wait,
		NextAt:  now.Add(wait),
		Reason:  strings.Join(reasons, "；"),
	}
}

// windowWait 检查滚动窗口内的登录数是否达到上限；达到则返回「最早那条滑出窗口」
// 所需的时间。limit<=0 时永不命中。
func windowWait(entries []Entry, now time.Time, window time.Duration, limit int) (time.Duration, bool) {
	if limit <= 0 {
		return 0, false
	}
	cut := now.Add(-window)
	in := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.At.After(cut) {
			in = append(in, e)
		}
	}
	if len(in) < limit {
		return 0, false
	}
	// 需要让 len-limit+1 条滑出窗口；最后滑出的那条索引为 len-limit。
	idx := len(in) - limit
	if idx < 0 {
		idx = 0
	}
	w := in[idx].At.Add(window).Sub(now)
	if w < 0 {
		w = 0
	}
	return w, true
}

// Record 记录一次成功登录。失败只返回错误，不 panic；调用方应记日志但不要因为
// 记录失败而丢弃已经拿到手的凭证。
func (g *Guard) Record(uid, realm string) error {
	if g == nil || g.Disabled || g.Path == "" {
		return nil
	}
	release, err := g.acquireLock()
	if err != nil {
		return err
	}
	defer release()

	entries, err := g.readEntries()
	if err != nil {
		// 历史损坏时从空开始重建，不让旧文件永久阻断后续记录。
		entries = nil
	}
	now := g.now()
	entries = append(entries, Entry{At: now, UID: uid, Realm: realm})

	cut := now.Add(-historyKeep)
	kept := entries[:0]
	for _, e := range entries {
		if e.At.After(cut) {
			kept = append(kept, e)
		}
	}
	if len(kept) > historyMax {
		kept = kept[len(kept)-historyMax:]
	}
	return g.writeEntries(kept)
}

// History 返回按时间升序排列的历史（只读，供面板/排障展示）。
func (g *Guard) History() ([]Entry, error) {
	if g == nil || g.Path == "" {
		return nil, nil
	}
	return g.readEntries()
}

func (g *Guard) readEntries() ([]Entry, error) {
	raw, err := os.ReadFile(g.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var st struct {
		Logins []Entry `json:"logins"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	out := st.Logins[:0]
	for _, e := range st.Logins {
		if !e.At.IsZero() {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

func (g *Guard) writeEntries(entries []Entry) error {
	dir := filepath.Dir(g.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	payload, err := json.MarshalIndent(struct {
		Logins []Entry `json:"logins"`
	}{Logins: entries}, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	tmp, err := os.CreateTemp(dir, ".login-history-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后为 no-op
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, g.Path)
}

// acquireLock 跨进程互斥：O_EXCL 创建锁文件，等不到就返回错误（调用方记日志即可）。
func (g *Guard) acquireLock() (func(), error) {
	lockPath := g.Path + ".lock"
	deadline := time.Now().Add(lockWait)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if st, statErr := os.Stat(lockPath); statErr == nil && time.Since(st.ModTime()) > lockStale {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("登录记录被占用（%s），请稍后重试", lockPath)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// HumanDuration 把时长转成中文可读文案（用于面板提示与 CLI 输出）。
func HumanDuration(d time.Duration) string {
	if d <= 0 {
		return "0 秒"
	}
	if d < time.Second {
		return "不到 1 秒"
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	case d < time.Hour:
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		if s == 0 {
			return fmt.Sprintf("%d 分钟", m)
		}
		return fmt.Sprintf("%d 分 %d 秒", m, s)
	default:
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		if m == 0 {
			return fmt.Sprintf("%d 小时", h)
		}
		return fmt.Sprintf("%d 小时 %d 分", h, m)
	}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

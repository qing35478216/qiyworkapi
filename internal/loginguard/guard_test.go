package loginguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestGuard(t *testing.T) (*Guard, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	now := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)
	g := &Guard{
		Path:        filepath.Join(dir, "login-history.json"),
		MinInterval: 10 * time.Minute,
		MaxPerHour:  4,
		MaxPerDay:   0,
		Now:         func() time.Time { return now },
	}
	return g, &now
}

func TestCheckAllowsFirstLogin(t *testing.T) {
	g, _ := newTestGuard(t)
	if d := g.Check(); !d.Allowed {
		t.Fatalf("首次登录应放行，得到 %+v", d)
	}
}

func TestMinIntervalBlocks(t *testing.T) {
	g, now := newTestGuard(t)
	if err := g.Record("u1", "cn"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(3 * time.Minute)

	d := g.Check()
	if d.Allowed {
		t.Fatal("3 分钟后应被最小间隔拦住")
	}
	if d.Wait != 7*time.Minute {
		t.Fatalf("剩余等待 = %v，期望 7m", d.Wait)
	}
	if !strings.Contains(d.Reason, "距上次登录") {
		t.Fatalf("原因文案不对: %q", d.Reason)
	}
}

func TestMinIntervalExpires(t *testing.T) {
	g, now := newTestGuard(t)
	if err := g.Record("u1", "cn"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(10 * time.Minute)
	if d := g.Check(); !d.Allowed {
		t.Fatalf("满 10 分钟应放行，得到 %+v", d)
	}
}

func TestMaxPerHourBlocks(t *testing.T) {
	g, now := newTestGuard(t)
	g.MinInterval = 0 // 只验证小时上限
	base := *now
	for i := 0; i < 4; i++ {
		*now = base.Add(time.Duration(i) * 5 * time.Minute)
		if err := g.Record("u", "cn"); err != nil {
			t.Fatal(err)
		}
	}
	*now = base.Add(20 * time.Minute)

	d := g.Check()
	if d.Allowed {
		t.Fatal("1 小时内第 5 次应被拦住")
	}
	// 最早一条在 base 记录，窗口滑出需要到 base+1h，即还剩 40m。
	if d.Wait != 40*time.Minute {
		t.Fatalf("剩余等待 = %v，期望 40m", d.Wait)
	}
	if !strings.Contains(d.Reason, "最近 1 小时") {
		t.Fatalf("原因文案不对: %q", d.Reason)
	}
}

func TestMaxPerDayBlocks(t *testing.T) {
	g, now := newTestGuard(t)
	g.MinInterval = 0
	g.MaxPerHour = 0
	g.MaxPerDay = 2
	base := *now
	for i := 0; i < 2; i++ {
		*now = base.Add(time.Duration(i) * 2 * time.Hour)
		if err := g.Record("u", "cn"); err != nil {
			t.Fatal(err)
		}
	}
	*now = base.Add(5 * time.Hour)
	d := g.Check()
	if d.Allowed {
		t.Fatal("24 小时内第 3 次应被拦住")
	}
	if !strings.Contains(d.Reason, "最近 24 小时") {
		t.Fatalf("原因文案不对: %q", d.Reason)
	}
}

func TestDisabledAlwaysAllows(t *testing.T) {
	g, now := newTestGuard(t)
	g.Disabled = true
	if err := g.Record("u1", "cn"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	if d := g.Check(); !d.Allowed {
		t.Fatalf("Disabled 应放行，得到 %+v", d)
	}
}

func TestRecordPersistsHistory(t *testing.T) {
	g, now := newTestGuard(t)
	if err := g.Record("u1", "cn"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(30 * time.Minute)
	if err := g.Record("u2", "global"); err != nil {
		t.Fatal(err)
	}
	entries, err := g.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].UID != "u1" || entries[1].UID != "u2" {
		t.Fatalf("历史不符: %+v", entries)
	}
	if entries[1].Realm != "global" {
		t.Fatalf("realm 未落盘: %+v", entries[1])
	}
}

func TestCorruptHistoryFailsOpen(t *testing.T) {
	g, _ := newTestGuard(t)
	if err := os.MkdirAll(filepath.Dir(g.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(g.Path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d := g.Check(); !d.Allowed {
		t.Fatalf("历史损坏应放行而不是锁死，得到 %+v", d)
	}
	if err := g.Record("u1", "cn"); err != nil {
		t.Fatalf("历史损坏时应能重建: %v", err)
	}
	entries, err := g.History()
	if err != nil || len(entries) != 1 {
		t.Fatalf("重建后历史 = %+v, err=%v", entries, err)
	}
}

func TestEmptyPathIsNoop(t *testing.T) {
	g := &Guard{}
	if d := g.Check(); !d.Allowed {
		t.Fatal("空 Path 应放行")
	}
	if err := g.Record("u", "cn"); err != nil {
		t.Fatal("空 Path 记录应为 no-op: " + err.Error())
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                              "0 秒",
		500 * time.Millisecond:         "不到 1 秒",
		45 * time.Second:               "45 秒",
		7 * time.Minute:                "7 分钟",
		6*time.Minute + 48*time.Second: "6 分 48 秒",
		2 * time.Hour:                  "2 小时",
		90 * time.Minute:               "1 小时 30 分",
	}
	for in, want := range cases {
		if got := HumanDuration(in); got != want {
			t.Errorf("HumanDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

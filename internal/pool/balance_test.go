package pool

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/qing35478216/qiyworkapi/internal/auth"
)

// balanceDist 连续 pick n 次，返回每个 uid 被选中的次数（模拟"池内自然负载"）。
// 为贴近真实路径，每次 pick 都新建 tried（无请求级排除）。
//
// 注意：必须把 minPickGap 置 0。否则快速连打时"刚被选中的号 100ms 内不合格"
// 会自行制造均匀分布，把被测的权重效应完全掩盖（那是防撞号机制在起作用，
// 与均衡开关无关，低频真实请求下不生效）。pick.go 注释也明确该开关供
// "纯加权分布测试"使用。
func balanceDist(t *testing.T, p *Pool, n int) map[string]int {
	t.Helper()
	old := minPickGap
	minPickGap = 0
	defer func() { minPickGap = old }()

	dist := map[string]int{}
	for i := 0; i < n; i++ {
		a := p.Pick()
		if a == nil {
			t.Fatalf("pick %d 返回 nil", i)
		}
		dist[a.UID]++
	}
	return dist
}

// newBalancePool 构造一个 7 账号池，credits 从 100 到 2400 悬殊分布
// （模拟真实账号池：老号积分厚、新号积分薄），全部健康、无冷却。
func newBalancePool(t *testing.T, credits ...int64) *Pool {
	t.Helper()
	p := New("")
	for i, c := range credits {
		uid := fmt.Sprintf("acct-%d", i)
		p.Add(&auth.Auth{UID: uid, Nickname: uid})
		p.SetCredits(uid, c, c)
	}
	return p
}

// spread 计算分布的极差比 max/min（越接近 1 越均匀）。
func spread(dist map[string]int) (int, int, float64) {
	var mx, mn int
	first := true
	for _, v := range dist {
		if first {
			mx, mn, first = v, v, false
			continue
		}
		if v > mx {
			mx = v
		}
		if v < mn {
			mn = v
		}
	}
	if mn == 0 {
		return mx, mn, 0
	}
	return mx, mn, float64(mx) / float64(mn)
}

// TestBalanceDisabledKeepsLegacyDistribution：perPick=0（默认关闭）时分布应由
// credits 主导——高积分号显著多于低积分号（证明测试能观测到热点，而非恒均匀）。
func TestBalanceDisabledKeepsLegacyDistribution(t *testing.T) {
	p := newBalancePool(t, 2400, 2000, 1600, 1200, 800, 400, 100)
	// 默认 New 即关闭；显式再设一次 0 双保险。
	p.SetBalance(0, 0)

	dist := balanceDist(t, p, 700)
	mx, mn, ratio := spread(dist)
	t.Logf("关闭均衡：分布=%v max=%d min=%d 极差比=%.2f", dist, mx, mn, ratio)

	if ratio < 2.0 {
		t.Fatalf("关闭均衡后极差比仅 %.2f，预期高积分号被显著偏好（>=2），测试失去区分度", ratio)
	}
}

// TestBalanceEnabledFlattensDistribution：perPick=3 后分布应显著变均匀
// （极差比大幅下降），证明轮询均衡把请求打散了。
func TestBalanceEnabledFlattensDistribution(t *testing.T) {
	credits := []int64{2400, 2000, 1600, 1200, 800, 400, 100}
	const N = 700

	// 同一 seed 的随机源在两轮间保持一致太苛刻（选中序列不同），改为对比
	// 两个独立 Pool：credits 分布相同，唯一差别是均衡开关。
	off := newBalancePool(t, credits...)
	off.SetBalance(0, 0)
	distOff := balanceDist(t, off, N)
	_, _, ratioOff := spread(distOff)

	on := newBalancePool(t, credits...)
	on.SetBalance(4, 32)
	distOn := balanceDist(t, on, N)
	mxOn, mnOn, ratioOn := spread(distOn)

	t.Logf("关闭均衡：%v 极差比=%.2f", distOff, ratioOff)
	t.Logf("开启均衡：%v 极差比=%.2f", distOn, ratioOn)

	if ratioOn >= ratioOff {
		t.Fatalf("开启均衡后极差比未下降：on=%.2f off=%.2f", ratioOn, ratioOff)
	}
	// 每个账号都应被选中（无饿死），且极差比收敛到 2 以内（7 账号理想值 1.0，
	// 随机性 + 加权抽签下 2 以内即视为"已打散"）。
	for uid, c := range distOn {
		if c == 0 {
			t.Fatalf("账号 %s 一次都没被选中（饿死）", uid)
		}
	}
	if ratioOn > 2.0 {
		t.Fatalf("开启均衡后极差比仍 %.2f（max=%d min=%d），预期 <=2", ratioOn, mxOn, mnOn)
	}
}

// TestBalanceGrowsWithIdleGap：权重应随"落后轮数"单调增长，且封顶生效。
// lastUsed 置为 now（elapsed=0）以隔离闲置项，credits=0 隔离 credits 项，
// 使权重 = 1.0(基数) + 均衡项。
func TestBalanceGrowsWithIdleGap(t *testing.T) {
	p := New("")
	now := time.Now()
	e := &entry{}
	e.a = &auth.Auth{UID: "x"}
	e.lastUsed = now // elapsed=0 → 闲置项 0

	// perPick=1, max=10：gap 0/3/9/20 → 均衡项 0/3/9/10（封顶）。
	p.SetBalance(1, 10)
	p.pickSeq = 100

	e.usedSeq = 100 // gap=0
	if w := p.weightOf(e, 0, now); w != 1.0 {
		t.Fatalf("gap=0 权重=%v 预期 1.0（无 credits/闲置/均衡项）", w)
	}
	e.usedSeq = 97 // gap=3
	if w := p.weightOf(e, 0, now); w != 4.0 {
		t.Fatalf("gap=3 权重=%v 预期 4.0", w)
	}
	e.usedSeq = 91 // gap=9
	if w := p.weightOf(e, 0, now); w != 10.0 {
		t.Fatalf("gap=9 权重=%v 预期 10.0", w)
	}
	e.usedSeq = 80 // gap=20 → 封顶 10
	if w := p.weightOf(e, 0, now); w != 11.0 {
		t.Fatalf("gap=20 权重=%v 预期 11.0（均衡项封顶 10 + 基数 1）", w)
	}
}

// TestBalanceZeroKeepsLegacyWeight：perPick=0 时权重与旧版完全一致
// （回归保护：老用户不配该字段时行为零变化）。
func TestBalanceZeroKeepsLegacyWeight(t *testing.T) {
	p := New("")
	now := time.Now()
	e := &entry{}
	e.a = &auth.Auth{UID: "x"}
	e.lastUsed = now // elapsed=0 → 闲置项 0
	p.pickSeq = 500
	e.usedSeq = 10 // gap 极大，但开关关闭 → 不加成

	w := p.weightOf(e, 1000, now)
	want := 1.0 + 0.0 // credits 0、elapsed 0 → 仅基数
	if w != want {
		t.Fatalf("perPick=0 权重=%v 预期 %v（应等于旧版口径）", w, want)
	}
}

// TestBalanceConcurrentPicks 并发 pick 不 panic / 不饿死（持锁路径回归）。
func TestBalanceConcurrentPicks(t *testing.T) {
	p := newBalancePool(t, 2400, 2000, 1600, 1200, 800, 400, 100)
	p.SetBalance(3, 24)

	var wg sync.WaitGroup
	dist := map[string]int{}
	var mu sync.Mutex
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				a := p.Pick()
				mu.Lock()
				if a != nil {
					dist[a.UID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	total := 0
	for _, c := range dist {
		total += c
	}
	if total != 64*20 {
		t.Fatalf("并发 pick 总数=%d 预期 %d", total, 64*20)
	}
	if len(dist) < 5 {
		t.Fatalf("并发下仅 %d 个账号被选中，预期 >=5（均衡应打散）", len(dist))
	}
	t.Logf("并发分布：%v", dist)
}

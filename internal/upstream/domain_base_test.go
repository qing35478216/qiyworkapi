package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qing35478216/qiyworkapi/internal/auth"
	"github.com/qing35478216/qiyworkapi/internal/loginsite"
)

// globalAccount 构造国际号：domain 落盘即登录签发域，realm 由 BackfillRealm 推断。
func globalAccount(domain string) *auth.Auth {
	a := &auth.Auth{UID: "u-" + domain, Domain: domain}
	_, _ = a.BackfillRealm()
	return a
}

// TestGlobalBaseFollowsAuthDomain global 出站 base 必须按 auth 文件 domain 选，不按
// realm 恒定选：国际域有 codebuddy.ai（官方 CLI 国际站）与 workbuddy.ai（旧桌面端）
// 两套主机名，同一后端但签发域不同。
func TestGlobalBaseFollowsAuthDomain(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	c := New()
	cases := []struct {
		domain     string
		wantChat   string
		wantBill   string
		wantWeb    string
		realmCheck string
	}{
		{"www.codebuddy.ai", loginsite.BaseGlobalOfficial, loginsite.BaseGlobalOfficial, loginsite.BaseGlobalOfficial, "global"},
		{"codebuddy.ai", loginsite.BaseGlobalOfficial, loginsite.BaseGlobalOfficial, loginsite.BaseGlobalOfficial, "global"},
		{"www.workbuddy.ai", loginsite.BaseGlobalWorkBuddy, loginsite.BaseGlobalWorkBuddy, loginsite.BaseGlobalWorkBuddy, "global"},
		{"workbuddy.ai", loginsite.BaseGlobalWorkBuddy, loginsite.BaseGlobalWorkBuddy, loginsite.BaseGlobalWorkBuddy, "global"},
	}
	for _, tc := range cases {
		a := globalAccount(tc.domain)
		if got := a.Realm(); got != tc.realmCheck {
			t.Fatalf("Realm(%q) = %q want %q", tc.domain, got, tc.realmCheck)
		}
		if got := c.chatBase(a); got != tc.wantChat {
			t.Errorf("chatBase(domain=%q) = %q want %q", tc.domain, got, tc.wantChat)
		}
		if got := c.billingBase(a); got != tc.wantBill {
			t.Errorf("billingBase(domain=%q) = %q want %q", tc.domain, got, tc.wantBill)
		}
		if got := c.webBase(a); got != tc.wantWeb {
			t.Errorf("webBase(domain=%q) = %q want %q", tc.domain, got, tc.wantWeb)
		}
	}
}

// TestGlobalBaseFallbacks 回落链：config 覆盖 > domain 推导 > 默认；domain 缺失但显式
// realm=global 时用默认（不猜域）；CN 账号恒 CN base；逃生门关闭 global 后仍走 CN。
func TestGlobalBaseFallbacks(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	// domain 缺失 + 显式 global → 默认（本项目登录链路签发域）。
	bare := &auth.Auth{UID: "bare"}
	if _, err := auth.BackfillRealmFor(bare, "global"); err != nil {
		t.Fatal(err)
	}
	c := New()
	if got := c.chatBase(bare); got != defaultGlobalBase {
		t.Errorf("bare global chatBase = %q want %q", got, defaultGlobalBase)
	}

	// config 覆盖优先于 domain 推导。
	override := New()
	override.ChatBaseGlobal = "https://chat.override"
	override.BillingBaseGlobal = "https://bill.override"
	a := globalAccount("www.codebuddy.ai")
	if got := override.chatBase(a); got != "https://chat.override" {
		t.Errorf("override chatBase = %q", got)
	}
	if got := override.billingBase(a); got != "https://bill.override" {
		t.Errorf("override billingBase = %q", got)
	}

	// CN 账号：domain 是国内域，不得落到国际站。
	cn := &auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"}
	if got := cn.Realm(); got != "cn" {
		t.Fatalf("cn Realm = %q", got)
	}
	if got := c.chatBase(cn); got != c.ChatBaseCN {
		t.Errorf("cn chatBase = %q want %q", got, c.ChatBaseCN)
	}

	// 逃生门：global 账号在 GlobalEnabled=false 时仍走 CN base。
	auth.SetGlobalEnabled(false)
	if got := c.chatBase(a); got != c.ChatBaseCN {
		t.Errorf("escape-hatch chatBase = %q want %q", got, c.ChatBaseCN)
	}
	auth.SetGlobalEnabled(true)
}

// TestXDomainFollowsAuthDomain X-Domain 真机语义是「哪个前端在调用」：全球号必须声明
// 自己的签发域，不能硬编码 workbuddy.ai；带协议/尾斜杠的手写 domain 收敛成裸主机名。
func TestXDomainFollowsAuthDomain(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	c := New()

	cases := []struct{ domain, want string }{
		{"www.codebuddy.ai", "www.codebuddy.ai"},
		{"www.workbuddy.ai", "www.workbuddy.ai"},
	}
	for _, tc := range cases {
		a := globalAccount(tc.domain)
		req := httptest.NewRequest(http.MethodPost, "/v2/chat/completions", nil)
		c.ChatHeaders(req, a, "", ChatMeta{})
		if got := req.Header.Get("X-Domain"); got != tc.want {
			t.Errorf("chat X-Domain(domain=%q) = %q want %q", tc.domain, got, tc.want)
		}
	}

	// 手写带协议的 domain：头里必须是裸主机名（不是 https://...）。
	dirty := globalAccount("https://www.codebuddy.ai/")
	dreq := httptest.NewRequest(http.MethodPost, "/v2/chat/completions", nil)
	c.ChatHeaders(dreq, dirty, "", ChatMeta{})
	if got := dreq.Header.Get("X-Domain"); got != "www.codebuddy.ai" {
		t.Errorf("dirty domain X-Domain = %q want www.codebuddy.ai", got)
	}
	if got := c.xDomainFor(dirty, c.chatBase(dirty)); got != "www.codebuddy.ai" {
		t.Errorf("xDomainFor(dirty) = %q want www.codebuddy.ai", got)
	}

	// billing 域同样按 domain 声明。
	breq := httptest.NewRequest(http.MethodPost, "/billing/meter/get-user-resource", nil)
	c.BillingHeaders(breq, globalAccount("www.codebuddy.ai"))
	if got := breq.Header.Get("X-Domain"); got != "www.codebuddy.ai" {
		t.Errorf("billing X-Domain = %q want www.codebuddy.ai", got)
	}
}

// TestGlobalRegisterBaseFollowsDomain 注册激活链路（补地区/激活 trial）与 billing 同域规则。
func TestGlobalRegisterBaseFollowsDomain(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	c := New()

	if got := c.globalRegisterBase(globalAccount("www.codebuddy.ai")); got != loginsite.BaseGlobalOfficial {
		t.Errorf("register base(codebuddy.ai) = %q", got)
	}
	if got := c.globalRegisterBase(globalAccount("www.workbuddy.ai")); got != loginsite.BaseGlobalWorkBuddy {
		t.Errorf("register base(workbuddy.ai) = %q", got)
	}
	bare := &auth.Auth{UID: "bare2"}
	if _, err := auth.BackfillRealmFor(bare, "global"); err != nil {
		t.Fatal(err)
	}
	if got := c.globalRegisterBase(bare); got != defaultGlobalBase {
		t.Errorf("register base(bare) = %q want %q", got, defaultGlobalBase)
	}
}

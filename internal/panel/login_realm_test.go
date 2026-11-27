package panel

import (
	"testing"

	"github.com/qing35478216/qiyworkapi/internal/loginsite"
)

// TestLoginEndpoints realm→端点映射：global 三端点全在 workbuddy.ai 域，
// cn/空/非法值 → copilot.tencent.com（零回归兜底）。
// platform 必须是真机形态（CN=workbuddy / GL=workbuddy-ai），旧的 `platform=CLI`
// 是 CodeBuddy CLI 时代遗留，登录风控会据此识别非官方客户端。
func TestLoginEndpoints(t *testing.T) {
	cases := []struct {
		realm      string
		wantState  string
		wantOrigin string
	}{
		{"global", "https://www.workbuddy.ai/v2/plugin/auth/state?platform=workbuddy-ai", "https://www.workbuddy.ai"},
		{"cn", "https://copilot.tencent.com/v2/plugin/auth/state?platform=workbuddy", "https://www.workbuddy.cn"},
		{"", "https://copilot.tencent.com/v2/plugin/auth/state?platform=workbuddy", "https://www.workbuddy.cn"},
		{"weird", "https://copilot.tencent.com/v2/plugin/auth/state?platform=workbuddy", "https://www.workbuddy.cn"},
	}
	for _, c := range cases {
		t.Run(c.realm, func(t *testing.T) {
			st, tok, acct, origin := loginsite.For(c.realm).Endpoints()
			if st != c.wantState {
				t.Errorf("realm=%q state=%q want %q", c.realm, st, c.wantState)
			}
			if origin != c.wantOrigin {
				t.Errorf("realm=%q origin=%q want %q", c.realm, origin, c.wantOrigin)
			}
			// token/account 端点必与 state 同 base。
			if c.realm == "global" {
				if tok[:len("https://www.workbuddy.ai")] != "https://www.workbuddy.ai" ||
					acct[:len("https://www.workbuddy.ai")] != "https://www.workbuddy.ai" {
					t.Errorf("global token/account endpoints should be workbuddy.ai: %q %q", tok, acct)
				}
			} else {
				if tok[:len("https://copilot.tencent.com")] != "https://copilot.tencent.com" ||
					acct[:len("https://copilot.tencent.com")] != "https://copilot.tencent.com" {
					t.Errorf("cn token/account endpoints should be copilot.tencent.com: %q %q", tok, acct)
				}
			}
		})
	}
}

// TestLoginHeaders 设备码三端点出站头族的 realm 差异与端点差异：
// UA 用桌面端 RPC 族、X-Product=SaaS、X-Domain 声明前端域、哨兵头为字符串 "true"；
// auth/state 带 X-Custom-Channel-Id，auth/token **不带 X-Domain**；
// 且不发 Origin/Referer/X-Requested-With（那是浏览器语义，桌面端 node fetch 不发）。
func TestLoginHeaders(t *testing.T) {
	cn := loginsite.For("cn")
	if ua := cn.UserAgent; ua != "WorkBuddy/5.11.3 WorkBuddy/5.11.3 CLI/2.176.0" {
		t.Errorf("cn login UA = %q", ua)
	}
	h := cn.StateHeaders("")
	if h["X-Domain"] != "www.workbuddy.cn" {
		t.Errorf("cn X-Domain = %q want www.workbuddy.cn", h["X-Domain"])
	}
	if h["X-Custom-Channel-Id"] != "unknown" {
		t.Errorf("cn state X-Custom-Channel-Id = %q want unknown", h["X-Custom-Channel-Id"])
	}
	if _, ok := h["X-Device-Token"]; ok {
		t.Errorf("empty device token must not be sent (不伪造设备指纹)")
	}
	if got := cn.StateHeaders("v3:abc"); got["X-Device-Token"] != "v3:abc" {
		t.Errorf("device token not injected: %v", got)
	}
	if h["X-No-Authorization"] != "true" || h["X-No-User-Id"] != "true" {
		t.Errorf("cn sentinels = %v", h)
	}
	for _, banned := range []string{"Origin", "Referer", "X-Requested-With"} {
		if _, ok := h[banned]; ok {
			t.Errorf("cn state headers must not contain %s", banned)
		}
	}
	if h["Content-Type"] != "application/json" {
		t.Errorf("POST state must carry Content-Type")
	}
	tok := cn.TokenHeaders()
	if _, ok := tok["Content-Type"]; ok {
		t.Errorf("GET token must not carry Content-Type")
	}
	// 真机 auth/token 是唯一没有 X-Domain 的端点（state/account 都有）。
	if _, ok := tok["X-Domain"]; ok {
		t.Errorf("GET token must not carry X-Domain, got %q", tok["X-Domain"])
	}
	// auth/token 也不带 state 专属的 channel id / device token。
	if _, ok := tok["X-Custom-Channel-Id"]; ok {
		t.Errorf("GET token must not carry X-Custom-Channel-Id")
	}

	gl := loginsite.For("global")
	if ua := gl.UserAgent; ua != "workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0" {
		t.Errorf("global login UA = %q", ua)
	}
	if gl.StateHeaders("")["X-Domain"] != "www.workbuddy.ai" {
		t.Errorf("global X-Domain = %q", gl.StateHeaders("")["X-Domain"])
	}
	if got := gl.AccountHeaders("at"); got["Authorization"] != "Bearer at" {
		t.Errorf("account headers must carry bearer, got %v", got)
	} else if _, ok := got["X-No-Authorization"]; ok {
		t.Errorf("authorized account request must not send X-No-Authorization")
	}
	if got := gl.AccountHeaders("at"); got["X-Domain"] != "www.workbuddy.ai" {
		t.Errorf("account headers must carry X-Domain, got %v", got)
	}
}

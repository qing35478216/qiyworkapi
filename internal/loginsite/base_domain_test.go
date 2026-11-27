package loginsite

import "testing"

// TestBaseForDomain 国际域两套主机名按 auth 文件 domain 选，不按 realm 猜：
//
//	*.codebuddy.ai → 官方 CLI 国际站
//	*.workbuddy.ai → 旧桌面端国际版
//	其它/空        → ""（调用方回落配置/默认，不猜域）
//
// 特别校验 codebuddy.**cn** 不被误判成国际域（国内 billing 主机名与 .ai 无关）。
func TestBaseForDomain(t *testing.T) {
	cases := []struct {
		domain string
		want   string
	}{
		{"www.codebuddy.ai", BaseGlobalOfficial},
		{"codebuddy.ai", BaseGlobalOfficial},
		{"api.codebuddy.ai", BaseGlobalOfficial},
		{"  WWW.CODEBUDDY.AI  ", BaseGlobalOfficial},      // 大小写/空白归一
		{"https://www.codebuddy.ai/", BaseGlobalOfficial}, // 手写带协议尾斜杠
		{"www.workbuddy.ai", BaseGlobalWorkBuddy},
		{"workbuddy.ai", BaseGlobalWorkBuddy},
		{"sub.workbuddy.ai", BaseGlobalWorkBuddy},
		{"https://www.workbuddy.ai/", BaseGlobalWorkBuddy},
		// 非国际域：一律返回空，由调用方回落（不把 CN 账号猜成国际站）。
		{"www.codebuddy.cn", ""},
		{"www.workbuddy.cn", ""},
		{"copilot.tencent.com", ""},
		{"", ""},
		{"codebuddy.ai.evil.com", ""}, // 后缀匹配必须按点边界，不能是字符串包含
	}
	for _, c := range cases {
		if got := BaseForDomain(c.domain); got != c.want {
			t.Errorf("BaseForDomain(%q) = %q want %q", c.domain, got, c.want)
		}
	}
}

// TestSiteGlobalAPIUsesWorkBuddyHost 站点表的 global API 主机与 domain→base 映射保持
// 同一常量来源（改一处不会两边漂移）。
func TestSiteGlobalAPIUsesWorkBuddyHost(t *testing.T) {
	if siteGlobal.API != BaseGlobalWorkBuddy {
		t.Errorf("siteGlobal.API = %q want %q", siteGlobal.API, BaseGlobalWorkBuddy)
	}
	if For("global").API != BaseGlobalWorkBuddy {
		t.Errorf("For(global).API drift")
	}
}

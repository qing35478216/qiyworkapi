package upstream

import (
	"net/http"
	"testing"

	"github.com/qing35478216/qiyworkapi/internal/auth"
)

func realmAuth(uid, domain string) *auth.Auth {
	a := &auth.Auth{AccessToken: "at", UID: uid, Domain: domain}
	a.BackfillRealm()
	return a
}

// TestChatHeadersIdentityPerRealm 出站身份按 realm 分档（2026-11 官方分发包口径）：
//
//	CN  UA WorkBuddy/5.11.3 … CLI/2.176.0，Origin www.workbuddy.cn，
//	    带 X-Client-Version/Platform/Product 三头（5.11.3 桌面端新增）
//	GL  UA WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0，
//	    不带 X-Client-*（国际包停在 5.9.4，官方从不发这三头）
func TestChatHeadersIdentityPerRealm(t *testing.T) {
	cases := []struct {
		realm  string
		auth   *auth.Auth
		wantUA string
	}{
		{"cn", realmAuth("u-cn", "copilot.tencent.com"), "WorkBuddy/5.11.3 WorkBuddy/5.11.3 CLI/2.176.0"},
		{"global", realmAuth("u-gl", "www.workbuddy.ai"), "WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0"},
	}
	for _, tc := range cases {
		t.Run(tc.realm, func(t *testing.T) {
			var got http.Header
			c := &Client{
				ChatHTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
					got = r.Header.Clone()
					return jsonResp(200, `{"code":0}`), nil
				})},
				ChatBaseCN:     "https://chat.example",
				ChatBaseGlobal: "https://global.example",
			}
			if rc, status, _, err := c.ChatStream(tc.auth, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{}); status != 200 || err != nil {
				t.Fatalf("chat status=%d err=%v", status, err)
			} else if rc != nil {
				rc.Close()
			}
			if ua := got.Get("User-Agent"); ua != tc.wantUA {
				t.Errorf("UA = %q want %q", ua, tc.wantUA)
			}
			if tc.realm == "cn" {
				if o := got.Get("Origin"); o != "https://www.workbuddy.cn" {
					t.Errorf("CN Origin = %q want https://www.workbuddy.cn", o)
				}
				if got.Get("X-Client-Version") != "5.11.3" ||
					got.Get("X-Client-Platform") != "windows" ||
					got.Get("X-Client-Product") != "WorkBuddy" {
					t.Errorf("CN X-Client-* = %q/%q/%q",
						got.Get("X-Client-Version"), got.Get("X-Client-Platform"), got.Get("X-Client-Product"))
				}
				if v := got.Get("X-IDE-Version"); v != "5.11.3" {
					t.Errorf("CN X-IDE-Version = %q want 5.11.3", v)
				}
			} else {
				if o := got.Get("Origin"); o != "https://www.workbuddy.ai" {
					t.Errorf("GL Origin = %q", o)
				}
				if got.Get("X-Client-Version") != "" || got.Get("X-Client-Platform") != "" || got.Get("X-Client-Product") != "" {
					t.Errorf("global must not send X-Client-*: %v", got)
				}
				if v := got.Get("X-IDE-Version"); v != "5.9.4" {
					t.Errorf("GL X-IDE-Version = %q want 5.9.4", v)
				}
			}
		})
	}
}

// TestDesktopFingerprintPerRealm 桌面端 /v2/report 指纹三元组按 realm 分档：
// 国内号不能盖国际包的 commit/releaseDate（版本号与安装清单会自相矛盾）。
func TestDesktopFingerprintPerRealm(t *testing.T) {
	fp := desktopFingerprint(realmAuth("u-cn", "copilot.tencent.com"))
	if fp["ideVersion"] != "5.11.3" || fp["commit"] != desktopCommitCN || fp["releaseDate"] != desktopReleaseDateCN {
		t.Errorf("CN fingerprint = %v/%v/%v", fp["ideVersion"], fp["commit"], fp["releaseDate"])
	}
	fp = desktopFingerprint(realmAuth("u-gl", "www.workbuddy.ai"))
	if fp["ideVersion"] != "5.9.4" || fp["commit"] != desktopCommitGlobal || fp["releaseDate"] != desktopReleaseDateGlobal {
		t.Errorf("GL fingerprint = %v/%v/%v", fp["ideVersion"], fp["commit"], fp["releaseDate"])
	}
	if fp["extVersion"] != "5.9.4" {
		t.Errorf("extVersion = %v want 5.9.4", fp["extVersion"])
	}
}

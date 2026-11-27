package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qing35478216/qiyworkapi/internal/auth"
)

// refresh 是登录令牌链的高频端点（每号定期刷新），头族/请求体一旦和真机不同，
// 等于周期性自报非官方客户端。真机口径：
//
//	POST /v2/plugin/auth/token/refresh，body 恒为 2 字节 `{}`
//	Authorization（旧 AT）与 X-Refresh-Token 同时携带
//	Accept 宽松值 / X-Product: SaaS / X-Auth-Refresh-Source: plugin
//	X-Request-ID（32 hex）/ X-User-Id / X-Domain 齐全
func TestRefreshRequestIdentity(t *testing.T) {
	var (
		method, path string
		body         []byte
		hdr          http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		body, _ = io.ReadAll(r.Body)
		hdr = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"accessToken":"at2","refreshToken":"rt2","expiresIn":3600}}`)
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	c.HTTP = srv.Client()
	a := &auth.Auth{
		AccessToken:  "at1",
		RefreshToken: "rt1",
		UID:          "u1",
		Domain:       "www.codebuddy.cn",
	}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}

	if method != http.MethodPost || path != "/v2/plugin/auth/token/refresh" {
		t.Errorf("request = %s %s", method, path)
	}
	if string(body) != "{}" {
		t.Errorf("refresh body = %q, want {}（真机 Content-Length: 2）", body)
	}
	checks := map[string]string{
		"Authorization":         "Bearer at1",
		"X-Refresh-Token":       "rt1",
		"X-Product":             "SaaS",
		"X-Auth-Refresh-Source": "plugin",
		"X-User-Id":             "u1",
		"X-Domain":              "www.codebuddy.cn",
	}
	for k, want := range checks {
		if got := hdr.Get(k); got != want {
			t.Errorf("%s = %q want %q", k, got, want)
		}
	}
	if got := hdr.Get("Accept"); !strings.Contains(got, "text/plain") {
		t.Errorf("Accept = %q want 客户端宽松值", got)
	}
	if got := hdr.Get("X-Request-ID"); len(got) != 32 {
		t.Errorf("X-Request-ID = %q want 32 hex", got)
	}
	if a.AccessToken != "at2" || a.RefreshToken != "rt2" {
		t.Errorf("tokens not written back: at=%q rt=%q", a.AccessToken, a.RefreshToken)
	}
}

// 无 AT 时 refresh 发 X-No-Authorization 哨兵（真机未鉴权形态），不发空 Bearer。
func TestRefreshHeadersNoAccessToken(t *testing.T) {
	c := New()
	req, _ := http.NewRequest(http.MethodPost, "https://example.com", strings.NewReader("{}"))
	a := &auth.Auth{RefreshToken: "rt1", Domain: "www.codebuddy.cn"}
	c.RefreshHeaders(req, a)
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want empty", got)
	}
	if got := req.Header.Get("X-No-Authorization"); got != "true" {
		t.Errorf("X-No-Authorization = %q want true", got)
	}
}

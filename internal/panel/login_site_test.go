package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// roundTripFunc 把任意请求交给函数处理（用于替换 loginHTTP.Transport，
// 让设备授权流程在进程内跑完，不碰真实上游）。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fakeLoginUpstream(t *testing.T, authURL string, seen *[]*http.Request) {
	t.Helper()
	old := loginHTTP
	loginHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if seen != nil {
			*seen = append(*seen, r.Clone(r.Context()))
		}
		body, _ := json.Marshal(map[string]any{
			"code": 0,
			"msg":  "ok",
			"data": map[string]string{"state": "st-1", "authUrl": authURL},
		})
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(body))),
			Request:    r,
		}, nil
	})}
	t.Cleanup(func() { loginHTTP = old })
}

// 面板发起 CN 登录：platform=workbuddy、授权链接补 version+loginSessionId、
// state 头带 X-Custom-Channel-Id / X-Domain，且不带浏览器三头。
func TestLoginStartCNURLAndHeaders(t *testing.T) {
	var seen []*http.Request
	fakeLoginUpstream(t, "https://www.workbuddy.cn/login?platform=workbuddy&state=st-1", &seen)

	p := New(Config{APIKey: "k"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/login/start", strings.NewReader(`{"realm":"CN"}`))
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		URL   string `json:"url"`
		Realm string `json:"realm"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.URL, "version=5.11.3") || !strings.Contains(resp.URL, "loginSessionId=") {
		t.Errorf("cn auth url missing version/loginSessionId: %s", resp.URL)
	}
	if resp.Realm != "cn" {
		t.Errorf("realm=%q want cn（大小写应归一）", resp.Realm)
	}
	if len(seen) != 1 {
		t.Fatalf("upstream calls=%d want 1", len(seen))
	}
	r := seen[0]
	if !strings.Contains(r.URL.String(), "platform=workbuddy&") && !strings.HasSuffix(r.URL.String(), "platform=workbuddy") {
		t.Errorf("cn state endpoint = %s", r.URL)
	}
	if got := r.Header.Get("X-Domain"); got != "www.workbuddy.cn" {
		t.Errorf("X-Domain=%q", got)
	}
	if got := r.Header.Get("X-Custom-Channel-Id"); got != "unknown" {
		t.Errorf("X-Custom-Channel-Id=%q", got)
	}
	for _, banned := range []string{"Origin", "Referer", "X-Requested-With"} {
		if v := r.Header.Get(banned); v != "" {
			t.Errorf("login state must not send %s=%q", banned, v)
		}
	}
}

// 面板发起国际版登录：platform=workbuddy-ai、version=5.9.4，
// 且**不**追加 loginSessionId（真机国际链接没有该参数）。
func TestLoginStartGlobalURL(t *testing.T) {
	var seen []*http.Request
	fakeLoginUpstream(t, "https://www.workbuddy.ai/login/?platform=workbuddy-ai&state=st-1", &seen)

	p := New(Config{APIKey: "k"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/login/start", strings.NewReader(`{"realm":"global"}`))
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.URL, "version=5.9.4") {
		t.Errorf("global auth url missing version=5.9.4: %s", resp.URL)
	}
	if strings.Contains(resp.URL, "loginSessionId=") {
		t.Errorf("global auth url must not carry loginSessionId: %s", resp.URL)
	}
	if len(seen) != 1 || !strings.Contains(seen[0].URL.String(), "platform=workbuddy-ai") {
		t.Errorf("global state endpoint = %+v", seen)
	}
	if got := seen[0].Header.Get("X-Domain"); got != "www.workbuddy.ai" {
		t.Errorf("global X-Domain=%q", got)
	}
}

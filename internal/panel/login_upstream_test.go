package panel

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qing35478216/qiyworkapi/internal/loginsite"
)

// 未知 realm 必须在打上游之前 400，不能静默回落 cn——
// 否则用户以为在加国际号，实际拿到的是国内授权链接。
func TestLoginStartRejectsUnknownRealm(t *testing.T) {
	p := New(Config{APIKey: "k"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/login/start", strings.NewReader(`{"realm":"intl"}`))
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（body=%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "intl") {
		t.Fatalf("body 应回显非法 realm: %s", rec.Body.String())
	}
}

// 业务错误必须保留官方 code，供调用方按码表分流 pending / 终态
// （旧实现只拼字符串，12151 会被当成失败把刚签发的 token 丢掉）。
func TestDoJSONKeepsBusinessCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":12151,"msg":"account loading","data":null}`))
	}))
	defer srv.Close()

	_, status, err := doJSON(http.MethodGet, srv.URL, map[string]string{}, "", nil)
	var env *loginsite.EnvelopeError
	if !errors.As(err, &env) {
		t.Fatalf("err = %v, want *loginsite.EnvelopeError", err)
	}
	if env.Code != loginsite.CodeRetryFetchAccount || env.Msg != "account loading" {
		t.Errorf("envelope = %+v", env)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d want 200", status)
	}
	if !loginsite.CodePending(env.Code) {
		t.Errorf("12151 must be classified pending")
	}
}

// HTTP 5xx 保留状态码（调用方能区分上游故障与业务 pending）。
func TestDoJSONKeepsHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	_, status, err := doJSON(http.MethodGet, srv.URL, map[string]string{}, "", nil)
	var env *loginsite.EnvelopeError
	if !errors.As(err, &env) {
		t.Fatalf("err = %v, want *loginsite.EnvelopeError", err)
	}
	if env.Status != http.StatusBadGateway || status != http.StatusBadGateway {
		t.Errorf("status = %d/%d want 502", status, env.Status)
	}
}

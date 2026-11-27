package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qing35478216/qiyworkapi/internal/livecfg"
)

// 刚登录过就再次发起添加账号：必须在打上游之前被闸门拦下（429），
// 不能落到 doJSON（否则会因无网络返回 502）。
func TestLoginStartRejectsTooFrequent(t *testing.T) {
	dir := t.TempDir()
	hist := filepath.Join(dir, "login-history.json")
	raw, err := json.Marshal(map[string]any{
		"logins": []map[string]any{{"at": time.Now().Add(-2 * time.Minute), "uid": "u1", "realm": "cn"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hist, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	p := New(Config{
		APIKey:           "k",
		Live:             livecfg.New(livecfg.Snapshot{APIKey: "k", LoginMinInterval: 10 * time.Minute, LoginMaxPerHour: 4}),
		LoginHistoryPath: hist,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/login/start", strings.NewReader(`{"realm":"cn"}`))
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429（body=%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "登录过于频繁") {
		t.Fatalf("body 缺少原因提示: %s", rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("缺少 Retry-After 头")
	}
}

// 已有登录流程未完成时，第二个 loginStart 必须 409（全局单飞）。
func TestLoginStartRejectsConcurrentFlow(t *testing.T) {
	p := New(Config{APIKey: "k"})
	p.logins["active-state"] = loginSession{created: time.Now(), realm: "cn"}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/login/start", strings.NewReader(`{"realm":"cn"}`))
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409（body=%s）", rec.Code, rec.Body.String())
	}
}

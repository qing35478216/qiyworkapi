package loginsite

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDecorateAuthURL 授权链接参数补齐：两版都加 version；loginSessionId 仅 CN；
// nonce 仅 CN 且非空才加；已有同名参数不覆盖；无参数时用 ? 起头。
func TestDecorateAuthURL(t *testing.T) {
	cn := For("cn")
	gl := For("global")

	raw := "https://www.workbuddy.cn/login?platform=workbuddy&state=abc"
	got := DecorateAuthURL(raw, cn, "", "sess-1")
	want := raw + "&version=5.11.3&loginSessionId=sess-1"
	if got != want {
		t.Errorf("cn decorate = %q want %q", got, want)
	}

	// 国际版真机链接没有 loginSessionId（只加 version）。
	graw := "https://www.workbuddy.ai/login/?platform=workbuddy-ai&state=x"
	ggot := DecorateAuthURL(graw, gl, "nonce-1", "sess-1")
	gwant := graw + "&version=5.9.4"
	if ggot != gwant {
		t.Errorf("global decorate = %q want %q", ggot, gwant)
	}

	// 已有 version 不覆盖（服务端签发值优先），也不重复追加。
	have := "https://www.workbuddy.cn/login?platform=workbuddy&state=abc&version=9.9.9"
	if got := DecorateAuthURL(have, cn, "", ""); got != have {
		t.Errorf("existing version must not be overridden, got %q", got)
	}

	// CN 且给了 nonce 才追加 ww_device_nonce。
	withNonce := DecorateAuthURL(raw, cn, "n-1", "")
	if !strings.Contains(withNonce, "ww_device_nonce=n-1") {
		t.Errorf("cn nonce missing: %q", withNonce)
	}
	// 无 ? 的 URL 用 ? 起头。
	bare := DecorateAuthURL("https://www.workbuddy.cn/login", cn, "", "")
	if !strings.HasPrefix(bare, "https://www.workbuddy.cn/login?version=5.11.3") {
		t.Errorf("bare url decorate = %q", bare)
	}
	// 空串/非法 URL 原样返回。
	if got := DecorateAuthURL("", cn, "", "s"); got != "" {
		t.Errorf("empty url should stay empty, got %q", got)
	}
}

// TestNewLoginSessionID uuid v4 形态（8-4-4-4-12，第三段以 4 开头，第四段 8/9/a/b）。
func TestNewLoginSessionID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for i := 0; i < 20; i++ {
		id := NewLoginSessionID()
		if !re.MatchString(id) {
			t.Fatalf("login session id %q is not uuid v4", id)
		}
	}
}

// TestCodeTables 官方登录业务码语义：11217/12151 pending，12005/11212/11216 终态，
// 其余（含 0）都不属于这两类。
func TestCodeTables(t *testing.T) {
	for _, c := range []int{CodeRetryFetchToken, CodeRetryFetchAccount} {
		if !CodePending(c) {
			t.Errorf("code %d should be pending", c)
		}
		if CodeFatal(c) {
			t.Errorf("code %d must not be fatal", c)
		}
	}
	for _, c := range []int{CodeLicenseSeatLimit, CodeLicenseExpired, CodeTrialExpired} {
		if !CodeFatal(c) {
			t.Errorf("code %d should be fatal", c)
		}
		if CodePending(c) {
			t.Errorf("code %d must not be pending", c)
		}
	}
	for _, c := range []int{0, 40001, 12153} {
		if CodePending(c) || CodeFatal(c) {
			t.Errorf("code %d should be neither pending nor fatal", c)
		}
	}
}

// TestDeviceTokenEnv 设备 token 取值优先级：环境变量 > 文件；空值不返回。
func TestDeviceTokenEnv(t *testing.T) {
	t.Setenv("CODEBUDDY_LOGIN_DEVICE_TOKEN", "")
	t.Setenv("QIYWORKAPI_LOGIN_DEVICE_TOKEN", "")
	t.Setenv("CODEBUDDY_LOGIN_DEVICE_TOKEN_FILE", "")
	t.Setenv("QIYWORKAPI_LOGIN_DEVICE_TOKEN_FILE", "")
	if got := DeviceToken(); got != "" {
		t.Errorf("empty env should yield empty token, got %q", got)
	}

	t.Setenv("QIYWORKAPI_LOGIN_DEVICE_TOKEN", " v3:env ")
	if got := DeviceToken(); got != "v3:env" {
		t.Errorf("env token = %q want v3:env", got)
	}

	p := filepath.Join(t.TempDir(), "dt.txt")
	if err := os.WriteFile(p, []byte("v3:file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QIYWORKAPI_LOGIN_DEVICE_TOKEN", "")
	t.Setenv("QIYWORKAPI_LOGIN_DEVICE_TOKEN_FILE", p)
	if got := DeviceToken(); got != "v3:file" {
		t.Errorf("file token = %q want v3:file", got)
	}
}

// TestEnvelopeErrorString 业务错误与 HTTP 错误的可读形态。
func TestEnvelopeErrorString(t *testing.T) {
	if got := (&EnvelopeError{Code: 12151, Msg: "loading"}).Error(); got != "code=12151 msg=loading" {
		t.Errorf("code error = %q", got)
	}
	if got := (&EnvelopeError{Status: 502}).Error(); got != "http_error: upstream 502" {
		t.Errorf("http error = %q", got)
	}
}

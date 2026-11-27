// Package loginsite 官方设备授权（OAuth device code）的站点表与出站头族。
//
// 为什么单独成包：设备授权链路有两个调用方——面板内嵌流程（internal/panel/login.go）
// 与命令行工具（cmd/login）。域表、UA 版本、platform 取值、头族一旦各写一份，就会出现
// 「面板能登、CLI 不能登」这种最难查的分叉；更严重的是两边各写一套过期指纹，等于每次
// 登录都主动向风控自报「我不是官方客户端」。
//
// 口径来源（2026-11 上游真机抓包，与社区同源分支逐头核对）：
//
//	CN 5.11.3  POST /v2/plugin/auth/state?platform=workbuddy
//	           UA: WorkBuddy/5.11.3 WorkBuddy/5.11.3 CLI/2.176.0
//	           x-domain: www.workbuddy.cn（请求实际打到 copilot.tencent.com）
//	GL 5.9.4   POST /v2/plugin/auth/state?platform=workbuddy-ai
//	           UA: workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0
//	           x-domain: www.workbuddy.ai
//
// 三端点头族不是一套：auth/state 有 Content-Type / X-Domain /
// X-Custom-Channel-Id / 四个 x-no-*（带 X-No-Authorization）；auth/token **没有
// X-Domain**；login/account 有 X-Domain + Bearer + 三个 x-no-*（无 X-No-Authorization）。
// 旧实现把三者抹平（全带 X-Domain、state 缺 channel id），是逐头比对能一眼看出的差异。
//
// 旧实现还发 `?platform=CLI` + `CLI/2.63.2 CodeBuddy/2.63.2`（CodeBuddy IDE 时代的串，
// WorkBuddy 分发包里根本不存在），并给三条 node-fetch 语义的端点盖上 Origin/Referer/
// X-Requested-With 的浏览器痕迹——这是登录环节最显眼的非官方指纹。
package loginsite

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// 官方版本常量（随分发包走，两版不同档）。
//
//	国内版 WorkBuddy 5.11.3（内置 @genie/agent-cli 2.176.0）
//	国际版 WorkBuddy AI 5.9.4（内置 2.165.0）
const (
	ClientVersionCN     = "5.11.3"
	ClientVersionGlobal = "5.9.4"
	BuiltinCLICN        = "2.176.0"
	BuiltinCLIGlobal    = "2.165.0"
)

// Site 一个登录站点的全部端点要素。
type Site struct {
	Realm     string // 落盘 realm：cn / global
	API       string // 上游 API base（三端点同域）
	Origin    string // 授权页所在前端域（CN www.workbuddy.cn / GL www.workbuddy.ai）；设备码三端点不发，仅日志/兜底
	Platform  string // 设备授权 ?platform= 取值（真机 CN=workbuddy / GL=workbuddy-ai）
	UserAgent string // 设备码三端点的 UA（桌面端 RPC 族）
	Domain    string // X-Domain 取值（声明「哪个前端在调用」，非 API 主机名）
	Version   string // 客户端版本（日志/展示用）
}

var (
	siteCN = Site{
		Realm:     "cn",
		API:       "https://copilot.tencent.com",
		Origin:    "https://www.workbuddy.cn",
		Platform:  "workbuddy",
		UserAgent: "WorkBuddy/" + ClientVersionCN + " WorkBuddy/" + ClientVersionCN + " CLI/" + BuiltinCLICN,
		Domain:    "www.workbuddy.cn",
		Version:   ClientVersionCN,
	}
	siteGlobal = Site{
		Realm:     "global",
		API:       "https://www.workbuddy.ai",
		Origin:    "https://www.workbuddy.ai",
		Platform:  "workbuddy-ai",
		UserAgent: "workbuddy-ai/" + ClientVersionGlobal + " workbuddy-ai/" + ClientVersionGlobal + " CLI/" + BuiltinCLIGlobal,
		Domain:    "www.workbuddy.ai",
		Version:   ClientVersionGlobal,
	}
)

// For 按 realm 返回站点定义；空/未知/非法 → 国内站（零回归）。
func For(realm string) Site {
	if strings.EqualFold(strings.TrimSpace(realm), "global") {
		return siteGlobal
	}
	return siteCN
}

// Endpoints 返回设备授权三端点（auth/state、token、account）与 Origin。
func (s Site) Endpoints() (state, token, account, origin string) {
	platform := s.Platform
	if platform == "" {
		platform = "workbuddy"
	}
	return s.API + "/v2/plugin/auth/state?platform=" + platform,
		s.API + "/v2/plugin/auth/token?state=",
		s.API + "/v2/plugin/login/account?state=",
		s.Origin
}

// baseHeaders 设备码三端点共同头族（真机形态）：Accept / UA / X-Product / X-Request-ID。
// 不含 Content-Type（只有 POST 带）、不含 Origin/Referer/X-Requested-With
// （那是浏览器语义，桌面端 node fetch 不发）。
func (s Site) baseHeaders() map[string]string {
	return map[string]string{
		"Accept":       "application/json, text/plain, */*",
		"User-Agent":   s.UserAgent,
		"X-Product":    "SaaS",
		"X-Request-ID": NewTraceID(),
	}
}

// sentinelNo 未鉴权/未落库端点的 x-no-* 哨兵头。真机值是字符串 "true"（不是 "1"）。
func sentinelNo(noAuth bool) map[string]string {
	out := map[string]string{
		"X-No-User-Id":         "true",
		"X-No-Enterprise-Id":   "true",
		"X-No-Department-Info": "true",
	}
	if noAuth {
		out["X-No-Authorization"] = "true"
	}
	return out
}

// StateHeaders auth/state（POST，未鉴权）出站头。
//
// 真机（CN 5.11.3 / GL 5.9.4 同形）：公共头 + Content-Type + X-Domain +
// X-Custom-Channel-Id: unknown + 四个 x-no-*；deviceToken 非空才追加 X-Device-Token
// （qimei 设备风控 token，本仓无法生成，取部署侧导出值，绝不伪造）。
func (s Site) StateHeaders(deviceToken string) map[string]string {
	h := s.baseHeaders()
	h["Content-Type"] = "application/json"
	h["X-Domain"] = s.Domain
	// X-Custom-Channel-Id 真机恒为字面量 "unknown"（渠道号未知的分发包）。
	h["X-Custom-Channel-Id"] = "unknown"
	for k, v := range sentinelNo(true) {
		h[k] = v
	}
	if deviceToken = strings.TrimSpace(deviceToken); deviceToken != "" {
		h["X-Device-Token"] = deviceToken
	}
	return h
}

// TokenHeaders auth/token（GET，未鉴权）出站头。
// GET 无请求体，不带 Content-Type；真机该端点**没有 X-Domain**（与 state/account 不同，
// 这是唯一缺它的端点），也不带 X-Custom-Channel-Id / X-Device-Token。
func (s Site) TokenHeaders() map[string]string {
	h := s.baseHeaders()
	for k, v := range sentinelNo(true) {
		h[k] = v
	}
	return h
}

// AccountHeaders login/account（GET，Bearer 已签发）出站头。
// 已带 Authorization，故不发 X-No-Authorization（其余三个哨兵照发）。
func (s Site) AccountHeaders(bearer string) map[string]string {
	h := s.baseHeaders()
	h["X-Domain"] = s.Domain
	if bearer != "" {
		h["Authorization"] = "Bearer " + bearer
	}
	for k, v := range sentinelNo(false) {
		h[k] = v
	}
	return h
}

// 官方登录业务码（CLI auth 模块枚举）。语义分两类，混为一谈会让调用方在本该立刻
// 报错的场景空转到 TTL，或者把「token 已签发、账号信息还在落库」误判成失败：
//
//	11217 RetryFetchToken   / 12151 RetryFetchAccount              → 还没登完，继续轮询
//	12005 LicenseSeatLimit  / 11212 LicenseExpired / 11216 TrialExpired → 终态
const (
	CodeRetryFetchToken   = 11217
	CodeRetryFetchAccount = 12151
	CodeLicenseSeatLimit  = 12005
	CodeLicenseExpired    = 11212
	CodeTrialExpired      = 11216
)

// CodePending 报告该业务码是否表示「登录进行中，应继续轮询」。
func CodePending(code int) bool {
	return code == CodeRetryFetchToken || code == CodeRetryFetchAccount
}

// CodeFatal 报告该业务码是否属于官方定义的终态许可错误（SignLicenseError）：
// 官方对这三类直接把上游 msg 抛给用户并结束登录，不再轮询。
func CodeFatal(code int) bool {
	switch code {
	case CodeLicenseSeatLimit, CodeLicenseExpired, CodeTrialExpired:
		return true
	}
	return false
}

// EnvelopeError 上游 {code,msg} 业务错误。Code 供调用方按官方码表区分
// pending / 终态；Status 为 HTTP 状态码（区分传输层与上游 5xx）。
type EnvelopeError struct {
	Code   int
	Msg    string
	Status int
}

func (e *EnvelopeError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code != 0 {
		return fmt.Sprintf("code=%d msg=%s", e.Code, e.Msg)
	}
	return fmt.Sprintf("http_error: upstream %d", e.Status)
}

// DeviceToken 真机的 X-Device-Token（qimei 设备风控 token）。
//
// 该值由客户端设备指纹 SDK 本地生成后加密上报，**不能**由 machineId 推导；
// 取不到就返回空串，调用方据此不发该头——伪造一个假 token 比缺这个头更可疑。
// 取值来源（按优先级）：环境变量 → 文件（宿主从桌面端落盘处导出）。
func DeviceToken() string {
	for _, k := range []string{"CODEBUDDY_LOGIN_DEVICE_TOKEN", "QIYWORKAPI_LOGIN_DEVICE_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	for _, k := range []string{"CODEBUDDY_LOGIN_DEVICE_TOKEN_FILE", "QIYWORKAPI_LOGIN_DEVICE_TOKEN_FILE"} {
		if p := strings.TrimSpace(os.Getenv(k)); p != "" {
			if b, err := os.ReadFile(p); err == nil {
				if v := strings.TrimSpace(string(b)); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// DeviceNonce 真机授权链接的 ww_device_nonce（桌面端 Turing 指纹 SDK 产出）。
// 本仓无该 SDK，只能取部署侧导出值；空 = 不加该参数（不伪造设备指纹）。
func DeviceNonce() string {
	for _, k := range []string{"CODEBUDDY_LOGIN_DEVICE_NONCE", "QIYWORKAPI_LOGIN_DEVICE_NONCE"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// DecorateAuthURL 按真机实测补齐授权链接参数（对齐官方 appendAuthUrlParams 系列）：
//
//	version=<site.Version>  官方写自己的包版本（CN 5.11.3 / GL 5.9.4，与 UA 同源）。
//	                        授权页会把它转给 console/auth/risk-context 并编进回调，
//	                        缺失时页面按旧客户端渲染，是登录环节的版本指纹。
//	loginSessionId=<uuid>   官方客户端本地生成的登录会话标识，**只对
//	                        platform=workbuddy（CN）追加**；国际版真机链接没有它。
//	ww_device_nonce=<nonce> 仅 platform=workbuddy 且拿到 nonce 时追加。
//
// 已有同名参数时不覆盖（服务端签发值优先）；解析失败原样返回。
func DecorateAuthURL(raw string, s Site, nonce, sessionID string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	var add []string
	if s.Version != "" && q.Get("version") == "" {
		add = append(add, "version="+url.QueryEscape(s.Version))
	}
	if s.Platform == "workbuddy" {
		if sessionID != "" && q.Get("loginSessionId") == "" {
			add = append(add, "loginSessionId="+url.QueryEscape(sessionID))
		}
		if nonce != "" && q.Get("ww_device_nonce") == "" {
			add = append(add, "ww_device_nonce="+url.QueryEscape(nonce))
		}
	}
	if len(add) == 0 {
		return raw
	}
	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}
	return raw + sep + strings.Join(add, "&")
}

// NewLoginSessionID 生成 uuid v4 形态的登录会话标识。
// 官方由客户端本地生成、只做透传（授权页读它再编进回调），不是服务端签发的凭证，
// 本地随机即可；用 v4 形状只是为了与真机同一形态。熵源故障返回空串（调用方不加参数）。
func NewLoginSessionID() string {
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NewTraceID 生成 32 hex 纯随机请求 ID（真机 X-Request-ID 形态：无毫秒前缀）。
// 熵源故障时回落时间派生，仍恒 32 hex，不 panic、不空串。
func NewTraceID() string {
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		ms := time.Now().UnixMilli()
		return fmt.Sprintf("%012x%020x", ms, uint64(ms))
	}
	return hex.EncodeToString(b[:])
}

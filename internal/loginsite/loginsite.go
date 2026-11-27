// Package loginsite 官方设备授权（OAuth device code）的站点表与出站头族。
//
// 为什么单独成包：设备授权链路有两个调用方——面板内嵌流程（internal/panel/login.go）
// 与命令行工具（cmd/login）。域表、UA 版本、platform 取值、头族一旦各写一份，就会出现
// 「面板能登、CLI 不能登」这种最难查的分叉；更严重的是两边各写一套过期指纹，等于每次
// 登录都主动向风控自报「我不是官方客户端」。
//
// 口径来源（2026-11 上游真机抓包，与社区同源分支对齐）：
//
//	CN 5.11.3  POST /v2/plugin/auth/state?platform=workbuddy
//	           UA: WorkBuddy/5.11.3 WorkBuddy/5.11.3 CLI/2.176.0
//	           x-domain: www.workbuddy.cn（请求实际打到 copilot.tencent.com）
//	GL 5.9.4   POST /v2/plugin/auth/state?platform=workbuddy-ai
//	           UA: workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0
//	           x-domain: www.workbuddy.ai
//
// 旧实现发 `?platform=CLI` + `CLI/2.63.2 CodeBuddy/2.63.2`（CodeBuddy IDE 时代的串，
// WorkBuddy 分发包里根本不存在），并给三条 node-fetch 语义的端点盖上 Origin/Referer/
// X-Requested-With 的浏览器痕迹——这是登录环节最显眼的非官方指纹。
package loginsite

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
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
	Origin    string // 授权页跳转语义的 Origin（设备码三端点不发，仅日志/兜底）
	Platform  string // 设备授权 ?platform= 取值（真机 CN=workbuddy / GL=workbuddy-ai）
	UserAgent string // 设备码三端点的 UA（桌面端 RPC 族）
	Domain    string // X-Domain 取值（声明「哪个前端在调用」，非 API 主机名）
	Version   string // 客户端版本（日志/展示用）
}

var (
	siteCN = Site{
		Realm:     "cn",
		API:       "https://copilot.tencent.com",
		Origin:    "https://www.codebuddy.cn",
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
func (s Site) StateHeaders() map[string]string {
	h := s.baseHeaders()
	h["Content-Type"] = "application/json"
	h["X-Domain"] = s.Domain
	for k, v := range sentinelNo(true) {
		h[k] = v
	}
	return h
}

// TokenHeaders auth/token（GET，未鉴权）出站头。GET 无请求体，不带 Content-Type。
func (s Site) TokenHeaders() map[string]string {
	h := s.baseHeaders()
	h["X-Domain"] = s.Domain
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

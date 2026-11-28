// login.go 面板内嵌的 WorkBuddy CN OAuth 设备授权流程（cmd/login 的进程内移植）。
//
//	POST /panel/api/login/start → 拿 state+authUrl，state 存进程内（不再落 /tmp，
//	  原方案在 Windows 上不可用），返回授权 URL；
//	GET  /panel/api/login/poll   → 面板前端每 3s 轮询本接口；未完成返回 done=false，
//	  完成后取 uid/nickname、凭证落盘 auths/workbuddy-<uid>.json、热加载进池
//	  （pool.Add + Revive），并顺带签到 + 余额刷新 —— 免重启加载新账号。
//
// 无 PKCE（workbuddy 设备流由服务端签发 state），请求头与上游端点与 cmd/login 保持一致。
package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qing35478216/qiyworkapi/internal/auth"
	"github.com/qing35478216/qiyworkapi/internal/loginguard"
	"github.com/qing35478216/qiyworkapi/internal/loginsite"
)

// loginHTTP 设备授权专用 client：短超时、无 cookie（每请求携带 state，无会话态）。
var loginHTTP = &http.Client{Timeout: 30 * time.Second}

// validUID 校验上游返回的 uid 是否可安全用于拼文件名。
// 只放行字母、数字、下划线、连字符（腾讯侧 uid 实测为 UUID 形态），
// 长度上限 64 兜底异常超长串；拒绝 . / \ 等路径字符与空串。
func validUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// apiEnvelope 与 upstream 同形：{code,msg,data}，code!=0 视为业务错误。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 发一次 JSON 请求并解信封。headers 为站点出站头族（loginsite 提供）。
func doJSON(method, fullURL string, headers map[string]string, bearer string, body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := loginHTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		// 保留状态码：调用方据此区分「上游 5xx（真故障）」与「业务 pending」。
		return nil, resp.StatusCode, &loginsite.EnvelopeError{Status: resp.StatusCode}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		// 结构化错误：调用方按官方码表区分 pending（11217/12151）/ 终态（12005/11212/11216）。
		return nil, resp.StatusCode, &loginsite.EnvelopeError{Code: env.Code, Msg: env.Msg, Status: resp.StatusCode}
	}
	return env.Data, resp.StatusCode, nil
}

// loginGuard 以当前热配置构造登录错峰闸门。Path 为空时闸门自动失效（放行）。
func (p *Panel) loginGuard() *loginguard.Guard {
	s := p.cfg.Live.Load()
	minInterval := s.LoginMinInterval
	if minInterval == 0 {
		// 未注入热配置时回退默认，避免闸门静默失效（Path 为空仍会 no-op）。
		minInterval = 10 * time.Minute
	}
	return &loginguard.Guard{
		Path:        p.cfg.LoginHistoryPath,
		MinInterval: minInterval,
		MaxPerHour:  s.LoginMaxPerHour,
		MaxPerDay:   s.LoginMaxPerDay,
		Disabled:    s.LoginGuardOff,
	}
}

// loginStart 发起设备授权：POST auth/state 拿授权 URL。
// body 可带 {"realm":"global"}（缺省 cn）；state 会话记 realm，poll 同 realm 落盘。
func (p *Panel) loginStart(w http.ResponseWriter, r *http.Request) {
	realm := "cn"
	if r.Body != nil {
		var reqBody struct {
			Realm string `json:"realm"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&reqBody); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON（可选 {\"realm\":\"cn|global\"}）")
			return
		}
		// 大小写/空白归一（与 CLI 的 --realm 解析同口径）；非法值直接报错，
		// 不静默回落 cn——否则用户以为在加国际号，实际拿到的是国内授权链接。
		switch strings.ToLower(strings.TrimSpace(reqBody.Realm)) {
		case "", "cn":
			realm = "cn"
		case "global":
			realm = "global"
		default:
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("不支持的 realm %q（只能是 cn 或 global）", reqBody.Realm))
			return
		}
	}

	// 闸门一：错峰（距上次登录太近 / 滚动窗口内数量超限）。
	if d := p.loginGuard().Check(); !d.Allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Wait.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, fmt.Sprintf(
			"登录过于频繁：%s；请等待 %s后再添加账号（如需紧急补登，可在配置里临时设 login.disabled=true）",
			d.Reason, loginguard.HumanDuration(d.Wait)))
		return
	}

	// 闸门二：全局单飞，任何时刻只允许一个登录流程。
	// 顺手回收过期会话，防"开弹窗走开"的 state 滞留。
	p.loginMu.Lock()
	for s, sess := range p.logins {
		if time.Since(sess.created) > loginTTL {
			delete(p.logins, s)
		}
	}
	if len(p.logins) > 0 || p.loginPending {
		p.loginMu.Unlock()
		writeErr(w, http.StatusConflict,
			"已有登录流程进行中：请先在浏览器完成它，或等待 15 分钟授权超时后再添加下一个账号")
		return
	}
	p.loginPending = true
	p.loginMu.Unlock()

	inserted := false
	defer func() {
		if !inserted {
			p.loginMu.Lock()
			p.loginPending = false
			p.loginMu.Unlock()
		}
	}()

	site := loginsite.For(realm)
	epState, _, _, _ := site.Endpoints()
	data, status, err := doJSON(http.MethodPost, epState, site.StateHeaders(loginsite.DeviceToken(), loginsite.DeviceIDList(loginsite.Qimei36())), "", bytes.NewReader([]byte("{}")))
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("auth state (upstream %d): %v", status, err))
		return
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		writeErr(w, http.StatusBadGateway, "auth state: missing state or authUrl")
		return
	}
	// 授权链接补齐真机参数：version（两版都加，授权页会转给 risk-context）、
	// loginSessionId（仅 CN）、ww_device_nonce（有则加）。缺 version 时授权页按旧客户端渲染。
	st.AuthURL = loginsite.DecorateAuthURL(st.AuthURL, site, loginsite.DeviceNonce(), loginsite.NewLoginSessionID())
	p.loginMu.Lock()
	p.logins[st.State] = loginSession{created: time.Now(), realm: realm}
	p.loginPending = false
	p.loginMu.Unlock()
	inserted = true
	log.Printf("panel: 发起 OAuth 添加账号 realm=%s（state=%s...）", realm, st.State[:min(8, len(st.State))])
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": st.AuthURL, "state": st.State, "realm": realm})
}

// loginPoll 轮询登录态。未完成 → {done:false}；完成 → 建凭证、落盘、热加载、签到。
func (p *Panel) loginPoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeErr(w, http.StatusBadRequest, "missing state")
		return
	}
	p.loginMu.Lock()
	sess, known := p.logins[state]
	p.loginMu.Unlock()
	if !known {
		writeErr(w, http.StatusNotFound, "unknown or expired state（请重新发起添加账号）")
		return
	}
	site := loginsite.For(sess.realm)
	_, epToken, epAcct, _ := site.Endpoints()

	// auth/token 是权威登录状态端点：pending 时业务 code 非 0（11217）。
	// 终态许可错误（12005/11212/11216）必须立刻结束并抛出上游 msg，不能空转到 TTL。
	tokRaw, _, err := doJSON(http.MethodGet, epToken+state, site.TokenHeaders(), "", nil)
	if err != nil {
		var env *loginsite.EnvelopeError
		if errors.As(err, &env) && loginsite.CodeFatal(env.Code) {
			p.loginMu.Lock()
			delete(p.logins, state)
			p.loginMu.Unlock()
			log.Printf("panel: 登录被上游拒绝 state=%s... code=%d msg=%s", state[:min(8, len(state))], env.Code, env.Msg)
			writeJSON(w, http.StatusOK, map[string]any{
				"done":    false,
				"fatal":   true,
				"code":    env.Code,
				"message": fmt.Sprintf("登录被上游拒绝（code=%d）：%s", env.Code, env.Msg),
			})
			return
		}
		// pending / 网络抖动：面板前端继续轮询。
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": err.Error()})
		return
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "waiting for login"})
		return
	}

	// 完成：取 uid/nickname。官方 12151（RetryFetchAccount）是「token 已发但账号信息
	// 还在落库」，属 pending，必须保留会话继续轮询——否则刚登录成功的号会被误判成失败，
	// 用户重登又撞错峰窗口（10 分钟白等）。
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctRaw, _, errAcct := doJSON(http.MethodGet, epAcct+state, site.AccountHeaders(tok.AccessToken), "", nil)
	if errAcct != nil {
		var env *loginsite.EnvelopeError
		if errors.As(errAcct, &env) && loginsite.CodeFatal(env.Code) {
			p.loginMu.Lock()
			delete(p.logins, state)
			p.loginMu.Unlock()
			log.Printf("panel: 登录账号信息被上游拒绝 state=%s... code=%d msg=%s", state[:min(8, len(state))], env.Code, env.Msg)
			writeJSON(w, http.StatusOK, map[string]any{
				"done":    false,
				"fatal":   true,
				"code":    env.Code,
				"message": fmt.Sprintf("登录被上游拒绝（code=%d）：%s", env.Code, env.Msg),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": errAcct.Error()})
		return
	}
	_ = json.Unmarshal(acctRaw, &acct)
	if acct.UID == "" {
		// 账号信息缺 uid：保留会话等下一次轮询。token 已签发，丢弃等于浪费一个错峰窗口。
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "账号信息缺少 uid，稍后重试"})
		return
	}
	// UID 来自上游响应，未经校验就用于拼文件名会被路径穿越利用
	// （filepath.Join("./auths", "workbuddy-../../evil.json") → auths/evil.json）。
	// UID 是腾讯侧账号标识，实测为 UUID（十六进制与连字符），故只放行 [A-Za-z0-9_-]。
	if !validUID(acct.UID) {
		writeErr(w, http.StatusBadGateway, "上游返回的 uid 含非法字符，拒绝落盘（防路径穿越）")
		return
	}

	// 凭证落盘（嵌套形，与 auths/ 目录既有格式一致）→ 热加载进池。
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
		return
	}
	a := &auth.Auth{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
		Domain:       tok.Domain,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
		FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", acct.UID)),
	}
	// global 登录：落盘 auth.realm=global（Realm() 按此判域；不写则依赖 domain 后缀回落）。
	if sess.realm == "global" {
		if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
			writeErr(w, http.StatusInternalServerError, "set realm: "+err.Error())
			return
		}
	} else {
		// CN 也显式补 realm 键（幂等），让 auth 文件形态统一（与 LoadDir 存量迁移对齐）。
		_, _ = a.BackfillRealm()
	}
	if err := a.SaveAtomic(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save auth: "+err.Error())
		return
	}
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(acct.UID) // 全新登录 = 人工恢复口径：清掉旧号遗留的禁用/冷却/熔断

	// 记录本次成功登录，供下一次 loginStart 计算错峰窗口。
	// 记录失败不影响本次登录（凭证已落盘），仅记日志。
	guard := p.loginGuard()
	if err := guard.Record(acct.UID, sess.realm); err != nil {
		log.Printf("panel: 登录错峰记录失败 uid=%s（不影响本次登录）: %v", acct.UID, err)
	}
	nextLogin := ""
	if d := guard.Check(); !d.Allowed {
		nextLogin = d.NextAt.Format(time.RFC3339)
	}

	// 顺带签到 + 余额刷新（幂等；失败不影响登录结果，只体现在返回字段里）。
	// realm 分支：CN 走 DailyCheckin；global 无 CN 签到体系，改为注册激活 + trial 领取
	// （D4 门控同 scheduler：CN 任务端点对 global 不发起任何调用）。
	checkinMsg := ""
	remain := int64(-1)
	total := int64(0)
	if sess.realm == "global" {
		// 注册激活（幂等）：region required 时自动补地区（白名单首个，HK）后重新激活。
		// 失败不阻断登录结果（auth 已落盘），只在返回字段里体现。
		if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
			checkinMsg = "注册激活失败: " + err.Error()
			log.Printf("panel: global 注册激活 uid=%s: %v", acct.UID, err)
		} else if activated {
			log.Printf("panel: global 注册激活 uid=%s 完成", acct.UID)
		}
		// trial 加油包（幂等 14051 = 已领过，非错误）。
		if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
			checkinMsg = joinMsg(checkinMsg, "trial 领取失败: "+err.Error())
			log.Printf("panel: global trial uid=%s: %v", acct.UID, err)
		} else if claimed {
			log.Printf("panel: global trial uid=%s 已领", acct.UID)
		}
	} else if !a.IsEnterprise() {
		// 企业版跳过签到：上游 POST /v2/billing/meter/daily-checkin 对企业号
		// 400 code 10001「企业账号不支持该操作」，发了只会把该错误写进登录返回。
		// 下方 UserResource 余额查询照常（企业额度走 get-enterprise-user-usage 口径）。
		if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
			checkinMsg = err.Error()
		}
	}
	if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
		remain, total = rm, tt
		p.cfg.Pool.ReenableIfCredits(acct.UID, rm, tt)
	}

	p.loginMu.Lock()
	delete(p.logins, state)
	p.loginMu.Unlock()
	log.Printf("panel: 新账号已热加载 uid=%s nickname=%q realm=%s（免重启生效）", acct.UID, acct.Nickname, sess.realm)
	writeJSON(w, http.StatusOK, map[string]any{
		"done":            true,
		"uid":             acct.UID,
		"nickname":        acct.Nickname,
		"realm":           sess.realm,
		"credits":         remain,
		"credits_total":   total,
		"checkin_message": checkinMsg,
		"next_login_at":   nextLogin,
	})
}

// joinMsg 拼接 login 完成后的提示消息（多段用「；」连接，空段跳过）。
func joinMsg(parts ...string) string {
	out := ""
	for _, s := range parts {
		if s == "" {
			continue
		}
		if out != "" {
			out += "；"
		}
		out += s
	}
	return out
}

// loginRegions 返回 global 注册可选地区（panel 前端选地区弹窗用；CN 不调用）。
// 未持账号时返回白名单静态兜底（前端只读展示，不依赖上游）。
func (p *Panel) loginRegions(w http.ResponseWriter, r *http.Request) {
	// 静态白名单（对齐国际版 web 展示集）：面板前端只读展示，无需账号态。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"regions": []map[string]string{
			{"code": "HK", "name": "Hong Kong"},
			{"code": "MO", "name": "Macao"},
			{"code": "SG", "name": "Singapore"},
			{"code": "TH", "name": "Thailand"},
			{"code": "PH", "name": "Philippines"},
			{"code": "MY", "name": "Malaysia"},
			{"code": "ID", "name": "Indonesia"},
		},
	})
}

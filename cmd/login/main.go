// login.go — WorkBuddy OAuth 登录（设备授权流程，CN realm；--realm=global 供国际版）。
//
// 两个子命令，由 login.sh 顺序驱动：
//
//	login [--realm=cn|global] url   → POST /v2/plugin/auth/state?platform=workbuddy
//	                                  （global 为 workbuddy-ai）拿 state+authUrl，
//	                                  state 落 /tmp/qiyworkapi-login-state.json，
//	                                  stdout 打印补齐 version/loginSessionId 的授权 URL
//	login [--realm=cn|global] poll  → 读 state，GET /v2/plugin/auth/token?state= 一次，
//	                                  成功再 GET /v2/plugin/login/account?state= 拿 uid/nickname，
//	                                  stdout 打印完整 token+account JSON（含 realm 键）
//
// 端点、platform、UA 与头族全部来自 internal/loginsite（与面板共用同一份口径），
// 本文件只做流程编排。state 落盘带 realm，poll 读回校验与命令行 --realm 一致（防混域）。
// 无 PKCE（workbuddy 设备流由服务端签发 state）。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	auth2 "github.com/qing35478216/qiyworkapi/internal/auth"
	"github.com/qing35478216/qiyworkapi/internal/loginguard"
	"github.com/qing35478216/qiyworkapi/internal/loginsite"
)

// stateFile 登录 state 落盘路径（var 便于测试替换临时文件）
// Portable across OSes: the upstream hardcoded "/tmp/...", which on
// Windows resolves to <drive>:\tmp\... and aborts the OAuth flow with
// "The system cannot find the path specified". os.TempDir() is /tmp on Linux.
var stateFile = filepath.Join(os.TempDir(), "qiyworkapi-login-state.json")

// forceLogin 由 --force 置位：应急跳过错峰闸门（不推荐，仅排障用）。
var forceLogin bool

// loginHistoryPath 与面板共用同一份历史（面板默认 data/login-history.json）。
// 可通过 QIYWORKAPI_LOGIN_HISTORY 覆盖（Docker/自定义目录部署用）。
func loginHistoryPath() string {
	if v := strings.TrimSpace(os.Getenv("QIYWORKAPI_LOGIN_HISTORY")); v != "" {
		return v
	}
	return filepath.Join("data", "login-history.json")
}

// cliGuard 构造与面板同口径的错峰闸门。默认值与 cmd/server 保持一致：
// 间隔 10m、每小时 4 个、每日不限制；可用环境变量覆盖，便于脚本化部署。
func cliGuard() *loginguard.Guard {
	minInterval := 10 * time.Minute
	if v := strings.TrimSpace(os.Getenv("QIYWORKAPI_LOGIN_MIN_INTERVAL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			minInterval = d
		}
	}
	maxPerHour := 4
	if v := strings.TrimSpace(os.Getenv("QIYWORKAPI_LOGIN_MAX_PER_HOUR")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxPerHour = n
		}
	}
	maxPerDay := 0
	if v := strings.TrimSpace(os.Getenv("QIYWORKAPI_LOGIN_MAX_PER_DAY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxPerDay = n
		}
	}
	return &loginguard.Guard{
		Path:        loginHistoryPath(),
		MinInterval: minInterval,
		MaxPerHour:  maxPerHour,
		MaxPerDay:   maxPerDay,
	}
}

// ensureLoginAllowed 在发起 OAuth 前做一次错峰检查。
func ensureLoginAllowed() {
	if forceLogin {
		return
	}
	if d := cliGuard().Check(); !d.Allowed {
		fatal("登录过于频繁：%s；还需等待 %s（确需跳过请加 --force）",
			d.Reason, loginguard.HumanDuration(d.Wait))
	}
}

// exitFunc 供测试替换（默认 os.Exit；测试持临时替换为 panic 以进程内捕获 fatal）。
var exitFunc = os.Exit

// apiEnvelope 与 main.go:429-433 一致
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 与 oauth.go:33-66 一致：{code,msg,data} 信封，code!=0 → error。
// headers 为站点出站头族（loginsite 提供）；业务错误返回 *loginsite.EnvelopeError，
// 调用方按官方码表区分 pending / 终态。
func doJSON(client *http.Client, method, fullURL string, headers map[string]string, body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, &loginsite.EnvelopeError{Status: resp.StatusCode}
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, &loginsite.EnvelopeError{Code: env.Code, Msg: env.Msg, Status: resp.StatusCode}
	}
	return env.Data, resp.StatusCode, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	exitFunc(1)
}

type loginState struct {
	State string `json:"state"`
	Realm string `json:"realm,omitempty"` // url 落盘时写回的 realm，poll 读回校验防混域
}

// realm 取值枚举（与 internal/auth 的 Realm() 归一化输出一致）。
const (
	realmCN     = "cn"
	realmGlobal = "global"
)

// parseRealmArgs 解析开头的 --realm=cn|global（或分离式 --realm <v>）flag，缺省 cn。
// 大小写不敏感归一化；非法值/缺值报错。桌椅剩余参数（子命令）顺序不变。
func parseRealmArgs(args []string) (realm string, rest []string, err error) {
	realm = realmCN
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--realm":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("--realm requires a value")
			}
			v := strings.ToLower(strings.TrimSpace(args[i+1]))
			if v != realmCN && v != realmGlobal {
				return "", nil, fmt.Errorf("invalid --realm %q (want cn|global)", args[i+1])
			}
			realm = v
			i++
		case strings.HasPrefix(a, "--realm="):
			v := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(a, "--realm=")))
			if v != realmCN && v != realmGlobal {
				return "", nil, fmt.Errorf("invalid --realm %q (want cn|global)", v)
			}
			realm = v
		default:
			rest = append(rest, a)
		}
	}
	return realm, rest, nil
}

// resolveRealmInput 把交互式选域的一行输入归一化为 realm（纯函数，login.sh 交互分支
// 的核心决策，可测）。规则：
//
//	"1"/"cn"（大小写不敏感）/""（回车默认）→ cn
//	"2"/"global" → global
//	其他 → ("", false)（调用方回默认 cn）
func resolveRealmInput(input string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "", "1", "cn":
		return realmCN, true
	case "2", "global":
		return realmGlobal, true
	}
	return "", false
}

// promptRealm 交互式选域：向 out 打印选项提示（out 接 stderr，stdout 留给 realm 本身），
// 从 in 读一行，返回归一化 realm。非法输入警告后回落 cn；EOF（非交互/管道）回落 cn。
func promptRealm(in io.Reader, out io.Writer) string {
	fmt.Fprintln(out, "选择登录版本: 1) 国内版(cn) 2) 国际版(global) [默认 1/cn]: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		// EOF/非交互 → 回落默认 cn
		return realmCN
	}
	if realm, ok := resolveRealmInput(line); ok {
		return realm
	}
	fmt.Fprintln(out, "无效选择，默认国内版 cn")
	return realmCN
}

// validateRealmMatch 校验 state 文件 realm 与命令行 --realm 一致（防混域）：
// state 无 realm（旧文件）放行；非空且不一致 → error。
func validateRealmMatch(stateRealm, cliRealm string) error {
	if stateRealm != "" && stateRealm != cliRealm {
		return fmt.Errorf("realm mismatch: state file realm=%q, command --realm=%q（url 与 poll 需同一 realm）", stateRealm, cliRealm)
	}
	return nil
}

// runURL 执行 url 子命令：向站点 state 端点 POST 取授权 URL，
// state 落盘（带 realm），stdout 打印补齐真机参数的 authURL。out 接 stdout；
// stateFile 为落盘路径（可注入临时文件便于测试）。空 realm 视为缺省（调用方已归一）。
func runURL(realm, statePath string, client *http.Client, out io.Writer) {
	ensureLoginAllowed()
	site := loginsite.For(realm)
	epState, _, _, _ := site.Endpoints()
	data, _, err := doJSON(client, http.MethodPost, epState, site.StateHeaders(loginsite.DeviceToken(), loginsite.DeviceIDList(loginsite.Qimei36())), bytes.NewReader([]byte("{}")))
	if err != nil {
		fatal("auth state failed: %v", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		fatal("auth state: missing state or authUrl")
	}
	// 与面板同口径：补 version（两版都加）+ loginSessionId（仅 CN）+ 设备 nonce（有则加）。
	st.AuthURL = loginsite.DecorateAuthURL(st.AuthURL, site, loginsite.DeviceNonce(), loginsite.NewLoginSessionID())
	raw, _ := json.Marshal(loginState{State: st.State, Realm: realm})
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		fatal("write state: %v", err)
	}
	fmt.Fprintln(out, st.AuthURL)
}

// runPoll 执行 poll 子命令：读 state 文件（realm 校验），向站点 token 端点 GET 一次，
// 成功再 GET login/account（带 Bearer），stdout 打印完整 token+account JSON。
//
// 错误按官方码表分流：11217/12151 是 pending（继续等/稍后重跑 poll），
// 12005/11212/11216 是终态（立刻报错，不让用户白等）；其余按网络/上游故障处理。
// state 文件只在成功后删除——pending 时保留，避免重登撞错峰窗口。
func runPoll(realm, statePath string, client *http.Client, out io.Writer) {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		fatal("read state: %v (先跑 login url)", err)
	}
	var ls loginState
	if err := json.Unmarshal(raw, &ls); err != nil {
		fatal("parse state: %v", err)
	}
	// 防混域：state 落盘 realm 与命令行 --realm 不一致则拒绝（url 与 poll 必须同域）
	if err := validateRealmMatch(ls.Realm, realm); err != nil {
		fatal("%v", err)
	}
	site := loginsite.For(realm)
	_, epToken, epAcct, _ := site.Endpoints()
	// auth/token 是权威登录状态端点：pending 时业务 code 非 0（11217），
	// 完成时 code=0 + token bundle。
	tokRaw, status, errTok := doJSON(client, http.MethodGet, epToken+ls.State, site.TokenHeaders(), nil)
	if errTok != nil {
		var env *loginsite.EnvelopeError
		if errors.As(errTok, &env) {
			if loginsite.CodeFatal(env.Code) {
				fatal("登录被上游拒绝（code=%d）：%s", env.Code, env.Msg)
			}
			if loginsite.CodePending(env.Code) {
				fatal("登录未完成（code=%d）：请先在浏览器完成登录，再重跑 poll（state 已保留）", env.Code)
			}
		}
		if status == 0 || status >= 500 {
			fatal("token endpoint error: %v", errTok)
		}
		fatal("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		fatal("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
	}
	// login/account 拿 uid/nickname（带 Bearer）
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctRaw, _, errAcct := doJSON(client, http.MethodGet, epAcct+ls.State, site.AccountHeaders(tok.AccessToken), nil)
	if errAcct != nil {
		var env *loginsite.EnvelopeError
		if errors.As(errAcct, &env) {
			if loginsite.CodeFatal(env.Code) {
				fatal("登录被上游拒绝（code=%d）：%s", env.Code, env.Msg)
			}
			if env.Code == loginsite.CodeRetryFetchAccount {
				fatal("token 已签发，账号信息仍在落库（code=12151）：请等 1-2 分钟后重跑 `./login poll`（state 已保留，不要重新 url）")
			}
		}
		fatal("获取账号信息失败：%v（state 已保留，可稍后重跑 poll）", errAcct)
	}
	_ = json.Unmarshal(acctRaw, &acct)
	if acct.UID == "" {
		fatal("账号信息缺少 uid（state 已保留，可稍后重跑 poll）")
	}
	// 记录成功登录，供下一次 url 计算错峰窗口（与面板共用 data/login-history.json）。
	if err := cliGuard().Record(acct.UID, auth2.ResolveRealm(realm, tok.Domain)); err != nil {
		fmt.Fprintf(os.Stderr, "login: 记录登录时间失败（不影响本次登录）: %v\n", err)
	}
	oraw, _ := json.Marshal(buildLoginOutput(tok, realm, acct))
	fmt.Fprintln(out, string(oraw))
	os.Remove(statePath)
}

// buildLoginOutput 组装 poll 输出的完整 JSON（login.sh 据此落盘 auth 文件）。
// realm 永不空：显式 --realm 优先（ResolveRealm 处理），否则按上游返回的 domain 推断——
// 保证登录落盘的 auth 文件恒带 realm 键。
func buildLoginOutput(tok struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}, realm string, acct struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}) map[string]any {
	return map[string]any{
		"access_token":  tok.AccessToken,
		"refresh_token": tok.RefreshToken,
		"expires_in":    tok.ExpiresIn,
		"domain":        tok.Domain,
		"realm":         auth2.ResolveRealm(realm, tok.Domain),
		"uid":           acct.UID,
		"enterprise_id": acct.EnterpriseID,
		"nickname":      acct.Nickname,
	}
}

func main() {
	// --force 与 realm 解析解耦：先摘出来，其余参数保持原顺序。
	args := make([]string, 0, len(os.Args[1:]))
	for _, a := range os.Args[1:] {
		if a == "--force" {
			forceLogin = true
			continue
		}
		args = append(args, a)
	}
	realm, rest, err := parseRealmArgs(args)
	if err != nil {
		fatal("%v (usage: login [--realm=cn|global] [--force] <url|poll|realm>)", err)
	}
	if len(rest) < 1 {
		fatal("usage: login [--realm=cn|global] [--force] <url|poll>")
	}
	// 每个流程独立 cookie jar（oauth.go:22-29：多账号登录互不串会话）
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	switch rest[0] {
	case "url":
		runURL(realm, stateFile, client, os.Stdout)

	case "poll":
		runPoll(realm, stateFile, client, os.Stdout)

	case "realm":
		// 交互式选域（login.sh 无 --realm 传参且 stdin 为 tty 时调用）。
		// 提示打到 stderr，stdout 只输出归一化 realm，供 $( ) 捕获。
		realm := promptRealm(os.Stdin, os.Stderr)
		fmt.Println(realm)

	default:
		fatal("unknown subcommand %q (want url|poll|realm)", rest[0])
	}
}

# 更新日志

本项目遵循[语义化版本](https://semver.org/lang/zh-CN/)。发版流程：先在 `cmd/server/main.go` 升 `appVersion` 并推送 main，再打同名 `v*` tag，CI 自动构建五平台二进制与多架构镜像并生成 Release。

## [未发布]

### 修复

- **跟随官方 5.12.1 分发包更新身份口径**（2026-11-28，本机 WorkBuddy 升级当日）：CN 客户端版本 `5.11.3` → `5.12.1`、内置 CLI `2.176.0` → `2.181.0`，UA 变为 `WorkBuddy/5.12.1 WorkBuddy/5.12.1 CLI/2.181.0`；桌面端 `/v2/report` 指纹三元组同步为官方 5.12.1 上报值（commit `cb16190f…`、releaseDate `1795706679794`）。`X-Client-Platform` 不再硬编码 `windows`（沿自 Windows 抓包），改为按运行平台推导 `macos/windows/linux`——5.12.1 的 `buildClientIdentityHeaders` 就是从 `process.platform` 推的，硬编码在 macOS/Linux 宿主上等于自相矛盾的指纹；`/v2/report` 载荷的 `os/arch/osVersion/cpuModel` 同理由运行平台决定（macOS 实测 `darwin/arm64/24.6.0/Apple M1 Pro`）。global 侧仍为 `5.9.4` / `2.165.0`（无新版证据，不盲跟）
- **global 出站 base 按 auth 文件 domain 选，不再按 realm 恒定猜**：国际域有 `workbuddy.ai`（旧桌面端）与 `codebuddy.ai`（官方 CLI 国际站）两套主机名指向同一后端，此前 global 账号一律打 `www.workbuddy.ai`，`codebuddy.ai` 签发的凭证会被打到非其签发域。现按「config 覆盖 > auth domain 推导 > 默认」选 base，chat/billing/web（report、成长任务、注册激活）全部同口径；`isGlobalDomain` 同步认 `codebuddy.ai` 家族（否则这类账号会被判成 cn，直接错域）。`X-Domain` 与 `Origin/Referer` 同步改为账号签发域（此前 global 硬编码 `www.workbuddy.ai`，且 `/v2/report`、market expert、`/v2/user-asset` 把带协议的 URL 当成了 `X-Domain` 值）。实测（2026-11-28）：同一 global token 打两域 billing 均 200 且数据一致，两域确为同一后端
- **登录链路逐头复核（第二轮）**：`auth/token` 去掉真机没有的 `X-Domain`，`auth/state` 补 `X-Custom-Channel-Id: unknown` 与可选 `X-Device-Token`（有值才发，不伪造）；授权链接补 `version`（两版）与 `loginSessionId`（仅 CN）——缺 `version` 时授权页按旧客户端渲染并把旧版本编进 risk-context；`auth/token`/`login/account` 按官方码表分流：`11217/12151` 视为 pending 继续轮询（此前 12151「token 已签发、账号信息落库中」会被判失败并丢弃 token，重登又撞 10 分钟错峰窗口），`12005/11212/11216` 立刻终态报错；面板 `realm` 大小写归一、非法值 400（不再静默回落 cn）；token 刷新请求体改为真机的 2 字节 `{}`，补齐 `Accept`/`X-Product`/旧 AT `Authorization`/`X-Request-ID`/`X-User-Id`/`X-Domain` 与 `X-No-Authorization` 哨兵

### 新增

- **登录 `auth/state` 补 5.12.1 的 `X-Device-Id-List`**：新版桌面端把设备指纹 qimei36 以 `{"qimei36":"<36hex>"}` JSON 串放在这个头里上报（app.asar 的 `WorkbuddyAuthStateHeaderProvider` 逐行核对）。本仓不生成设备指纹，取值按「环境变量 `CODEBUDDY_QIMEI36`/`QIYWORKAPI_QIMEI36` → 桌面端落盘缓存 `~/.workbuddy/qimei-cache.json`」读取，取不到就不发——假 qimei 比缺头更容易被识别
- **出站身份按 realm 对齐官方分发包（2026-11 口径）**：CN `5.11.3` / 内置 CLI `2.176.0`，global `5.9.4` / `2.165.0`（旧实现统一 5.5.4 / 2.137.1，落后两个大版本）。UA 第二段 global 由 `WorkBuddy AI`（带空格，官方从不产生）改为真实的 `workbuddy-ai`；CN 业务 Origin 由 `codebuddy.cn`（CodeBuddy CLI 时代遗留）改为 `workbuddy.cn`；CN 请求补上 5.11.3 桌面端新增的 `X-Client-Version/Platform/Product` 三头（国际包不发，按 realm 隔离）；桌面端 `/v2/report` 指纹三元组（版本/commit/releaseDate）按 realm 分档，不再给国内号盖国际包的指纹
- **登录设备码链路身份重写**：`platform=CLI` → `workbuddy`（CN）/ `workbuddy-ai`（GL）；UA 由 CodeBuddy IDE 时代的 `CLI/2.63.2 CodeBuddy/2.63.2` 改为桌面端 RPC 族；头族改为真机形态（`X-Product: SaaS` + 32 hex `X-Request-ID` + `X-Domain` + `X-No-*` 字符串 `true` 哨兵），去掉设备码端点不该有的 Origin/Referer/X-Requested-With。面板与 CLI 共用新增的 `internal/loginsite`，避免两处各写一套指纹
- **429 / 模型封锁补 Retry-After**：限流与模型不可用时按「上游明示 → 模型解封时刻 → 池内最早解冻」给出等待秒数（`auth_dead` 不参与，等它没用），客户端不再盲猜退避（社区 issue #15 的诉求）
- **登录错峰闸门**：面板与 `login.sh` 共用 `data/login-history.json`，默认两次成功登录至少间隔 10 分钟、滚动 1 小时最多 4 个，并保证同一时刻只有一个 OAuth 流程；被拦下时返回 429/CLI 提示剩余等待时间。配置项 `login.*` 热生效，应急可设 `login.disabled=true` 或 CLI `--force`。目的：避免多个账号集中登录被上游判定为批量小号，整批吊销 refresh token（2026-11-27 多项目同发事故）
- **会话死亡软暂停（auth_dead）**：上游以 401/12153 明确拒绝令牌时，账号即时暂停选号（30 分钟到期自动复探），不再每个请求挨个撞一遍死号。触发面全覆盖：chat 401（含 APISIX 边缘层裸 HTML 401——此前只认 12153 业务信封，2026-10-17 上游批量吊销 10 个号会话后网关持续「假健康」）、按需刷新失败、keepalive 刷新失败。恢复路径：刷新成功 / 聊天成功 / 重新登录（令牌变更自动清除，无需到期或人工解冻）。`/status` 与面板新增「需重新登录」计数与账号标识；连续 3 次仍走既有永久禁用（防误杀语义不变）

## [1.13.2] - 2026-10-17

安全与工程加固版本。

### 安全

- `login.sh` 落盘凭证文件权限由 umask 默认值（通常 644）修正为显式 `600`，与 Go 侧 `auth.SaveAtomic` 对齐——凭证含明文 accessToken / refreshToken，不应人人可读

### 工程

- CI 增加 `gofmt -l` 格式门禁与 `go test -race ./...` 竞态检测（池调度 / 会话 / Redis 镜像均为并发代码）
- 清理 8 个历史未按 gofmt 格式化的文件，全库格式统一
- `appVersion` 去掉 `-panel` 后缀（品牌残留），版本串即语义化版本
- Dockerfile 基础镜像由 `golang:1.23-alpine` 对齐为 `golang:1.22-alpine`（与 go.mod 的 `go 1.22.5` 一致）

### 文档

- README 补「macOS / Linux 单文件运行」部署路径（此前只有 Docker 与 Windows）与 `server.log` 轮转指引
- README 源码构建补国内 GOPROXY 镜像说明
- 新增 `SECURITY.md`（漏洞报告政策与凭证安全须知）与 `CHANGELOG.md`

## [1.13.1] - 2026-10-17

重命名后的首个版本：项目标识由 workbuddy2api / wb2api 全量更为 qiyworkapi。

### 变更

- 可执行文件 `wb2api` → `qiyworkapi`（启动脚本 / Dockerfile / compose / CI / 全部文档同步）
- 环境变量前缀 `WB2A_` → `QIYWORKAPI_`
- 服务身份：healthz `service` 字段、`X-Service` 响应头、容器名、launchd label
- 内部标识：Redis key 前缀、SSE 哨兵 id、conversationId 前缀、面板 localStorage key
- LICENSE 重写为单行版权（MIT）
- README 品牌统一为 QiyWorkAPI；移除 fork 来源段落，「与上游的差异」章节更名「版本特性」（技术内容保留）
- 新增《轮询机制.md》（选号与调度机制深入解析）、《用法说明.md》（中文上手指南）、《WorkBuddy一键部署教程.md》

### 修复

- `config.example.json` 补回漏载的 `pool.balance_per_pick` / `balance_max`
- README 字段速查补 6 组缺失字段；修正 `BalanceMax` 注释与代码不一致（24.0 → 32.0）

## [1.13.0] - 2026-10-17

首个公开 Release：把 CodeBuddy 账号池封装为 OpenAI 兼容 API 的自托管网关。

核心能力：多账号加权轮转（成本分层 / 快过期偏好 / 闲置补偿 / 防惊群）、冷却 / 熔断 / 连败降权三级治理、会话粘性、五类定时任务（签到 / 活跃上报 / 猫猫旅行 / 保活 / 夜猫子）、成长任务一键完成（17/18）、内嵌 Web 管理面板（账号运维 / 用量分析 / 在线改配置 / 日志 / 任务中心）、流式与非流式、推理模型兼容、状态持久化（本地 + 可选 Upstash 镜像）。

[未发布]: https://github.com/qing35478216/qiyworkapi/compare/v1.13.2...HEAD
[1.13.2]: https://github.com/qing35478216/qiyworkapi/compare/v1.13.1...v1.13.2
[1.13.1]: https://github.com/qing35478216/qiyworkapi/compare/v1.13.0...v1.13.1
[1.13.0]: https://github.com/qing35478216/qiyworkapi/releases/tag/v1.13.0

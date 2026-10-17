# 更新日志

本项目遵循[语义化版本](https://semver.org/lang/zh-CN/)。发版流程：先在 `cmd/server/main.go` 升 `appVersion` 并推送 main，再打同名 `v*` tag，CI 自动构建五平台二进制与多架构镜像并生成 Release。

## [未发布]

### 新增

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

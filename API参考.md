# API 参考（面向二次开发）

本文档面向**开发其他程序调用本网关**的开发者：完整端点、全部请求字段、响应结构、错误语义与会话机制。所有描述以 `internal/server` 源码为准。

## 通用约定

| 项 | 值 |
|---|---|
| Base URL | `http://127.0.0.1:7863/v1`（**必须带 `/v1`**；**不要尾斜杠**，`/v1//chat/completions` 会 301） |
| 鉴权 | `Authorization: Bearer <api_key>`；仅当 `api_key` 非空时校验，为空则直接放行（公网部署必须设强密钥） |
| 请求体 | `application/json`，UTF-8，**无大小上限**（对齐上游） |
| 响应 | JSON；`POST /v1/chat/completions` 的响应带 `X-Request-Id` 头（请求追踪 ID，排查问题请附上）；`/healthz` 响应带 `X-Service: qiyworkapi` 头（身份标识，用于区分同端口其他服务） |
| 错误格式 | `{"error": {"message": "...", "type": "api_error", "code": "...", "gateway_hint": "..."}}`（`gateway_hint` 仅网关补充语义时出现） |

**模型名协议**：`model` 支持 realm 前缀——`cn:<裸名>`（国内，默认）或 `global:<裸名>`（国际版）。不带前缀按 `cn` 处理。`GET /v1/models` 返回的 `id` **自带 `cn:` 前缀**，可直接回传给调用接口。

## 端点总览

| 端点 | 方法 | 鉴权 | 说明 |
|---|---|---|---|
| `/v1/chat/completions` | POST | Bearer | OpenAI 兼容补全，流式 / 非流式 |
| `/v1/responses` | POST | Bearer | OpenAI Responses API（内部转换为 chat/completions 执行） |
| `/v1/models` | GET | Bearer | 模型列表（动态拉取，缓存 1h） |
| `/status` | GET | Bearer | 账号池状态汇总 |
| `/healthz` | GET | 无 | 健康检查（恒无鉴权） |

---

## POST /v1/chat/completions

### 请求字段

| 字段 | 类型 | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `model` | string | ✅ | — | 模型名，可带 `cn:` / `global:` 前缀（见上） |
| `messages` | array | ✅ | — | OpenAI 消息格式。`role` 支持 `system` / `developer` / `user` / `assistant` / `tool`；`content` 支持字符串或 parts 数组（`text` / `image_url`） |
| `stream` | bool | | `false` | `true` = SSE 流式；`false` = 网关本地聚合完整流为单响应（内部仍向上游强制走流式，行为透明） |
| `temperature` | number | | 透传 | 直接透传上游 |
| `top_p` | number | | 透传 | 直接透传上游 |
| `max_tokens` | int | | 透传 | 输出上限；各模型真实上限见 `/v1/models` 的 `max_output_tokens` |
| `presence_penalty` / `frequency_penalty` | number | | 透传 | 直接透传上游 |
| `stop` | string / array | | 透传 | 停止序列 |
| `n` | int | | 透传 | 采样条数 |
| `tools` | array | | 透传 | 工具定义（OpenAI function 格式）。**注意**：中转链路可能丢 `tools` 声明，网关会按会话历史出现过的工具名兜底还原 |
| `tool_choice` | string / object | | 透传 | 工具选择策略 |
| `parallel_tool_calls` | bool | | 透传 | 并行工具调用 |
| `reasoning_effort` | string | | 透传 | 思考档位：`off` / `low` / `medium` / `high`（模型支持的档位见 `/v1/models` 的 `reasoning_supported_efforts`；不可用时降级重试） |
| `user` | string | | 透传 | 终端用户标识。**带此字段时不启用派生会话键**（见「会话粘性」） |
| `conversation_id` | string | | — | 会话粘性键（见「会话粘性」），也接受 `conversationId`、`metadata.conversation_id`、`metadata.conversationId` |
| `prompt_cache_key` | string | | — | OpenAI 系前缀缓存键，网关同时用作会话粘性键 |
| `metadata` | object | | — | 任意元数据；其中 `conversation_id` / `conversationId` 被识别为会话键 |

### 网关行为（调用方需要知道的）

- **出站强制流式**：无论 `stream` 取值，网关对上游恒发 `stream:true`；非流式响应由网关聚合 SSE 为完整 `chat.completion`（含 `reasoning_content` / `tool_calls`）。
- **SSE 帧白名单重建**：流式响应按 OpenAI 规范重建，保留 `reasoning_content`，工具调用按 `index` 合并；保证恰好一个 `data: [DONE]`（上游漏发时兜底补写）；空流先写一帧 `error` 再补 `[DONE]`。
- **原生工具调用标记还原**：上游（DeepSeek 系）在请求未带 `tools` 时会把工具调用吐成**带全角竖线（U+FF5C）的标记文本**落在 `content` 里。网关在出站响应中识别并还原为结构化 `tool_calls`（流式 / 非流式均覆盖）；未命中时逐字节透传。
- **系统提示词**：默认 `passthrough` 模式逐字透传客户端 system（部署方可在 config 改 `custom` / `append`）。

### 非流式响应结构

```json
{
  "id": "chatcmpl-...",
  "object": "chat.completion",
  "created": 1753600000,
  "model": "deepseek-v4-flash",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "...",
      "reasoning_content": "...",
      "tool_calls": [{"id": "call_...", "type": "function", "function": {"name": "...", "arguments": "..."}}]
    },
    "finish_reason": "stop"
  }],
  "usage": {
    "prompt_tokens": 34, "completion_tokens": 11, "total_tokens": 45,
    "cached_tokens": 0, "prompt_cache_hit_tokens": 0, "prompt_cache_miss_tokens": 34,
    "completion_thinking_tokens": 9, "credit": 0
  }
}
```

> 注：响应 `model` 为**裸模型名**（不带 `cn:` / `global:` 前缀——前缀是网关侧路由协议）。`usage` 除标准三字段外还带缓存与计费明细（`cached_tokens` / `prompt_cache_*` / `completion_thinking_tokens` / `credit`），按需取用。

### 流式响应（SSE）

```
data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"..."},"finish_reason":null}]}

data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":"..."},"finish_reason":null}]}

data: [DONE]
```

### 调用示例

**curl（非流式）**：

```bash
curl -s http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer 你的api_key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "cn:deepseek-v4-flash",
    "messages": [{"role": "user", "content": "你好"}],
    "stream": false
  }'
```

**Python（OpenAI SDK，零改造）**：

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:7863/v1", api_key="你的api_key")

# 非流式
resp = client.chat.completions.create(
    model="deepseek-v4-flash",
    messages=[{"role": "user", "content": "你好"}],
)
print(resp.choices[0].message.content)

# 流式
for chunk in client.chat.completions.create(
    model="deepseek-v4-flash",
    messages=[{"role": "user", "content": "写一首诗"}],
    stream=True,
):
    delta = chunk.choices[0].delta
    if delta.content:
        print(delta.content, end="")
```

**带会话粘性的调用（多轮不跳号）**：

```python
resp = client.chat.completions.create(
    model="deepseek-v4-flash",
    messages=[{"role": "user", "content": "第二轮问题"}],
    extra_body={"conversation_id": "my-session-001"},  # 同一会话复用同一账号
)
```

**Node.js**：

```javascript
import OpenAI from "openai";
const client = new OpenAI({ baseURL: "http://127.0.0.1:7863/v1", apiKey: "你的api_key" });
const resp = await client.chat.completions.create({
  model: "deepseek-v4-flash",
  messages: [{ role: "user", content: "你好" }],
});
```

---

## POST /v1/responses

OpenAI Responses API 兼容端点。网关内部将其转换为 chat/completions 调用（loopback），**选号、粘性、治理与 chat 端点完全一致**。

### 请求字段

| 字段 | 类型 | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `model` | string | ✅ | — | 同 chat 端点，支持 `cn:` / `global:` 前缀 |
| `input` | string / array | ✅ | — | 输入内容；数组为 Responses 消息格式（内部转换为 messages） |
| `instructions` | string | | — | 系统指令（转换为 system 消息） |
| `tools` | array | | — | **仅映射 `type: "function"` 工具**（`web_search` 等内置工具上游不认，会被跳过） |
| `tool_choice` | string / object | | — | 工具选择（转换为 chat 语义） |
| `stream` | bool | | `false` | 流式（SSE） |
| `temperature` | number | | 透传 | |
| `top_p` | number | | 透传 | |
| `max_output_tokens` | int | | 透传 | 映射为 chat 的 `max_tokens` |
| `parallel_tool_calls` | bool | | 透传 | |
| `reasoning` | object | | — | 思考档位配置（如 `{"effort": "high"}`），转换为 chat 的 `reasoning_effort` |
| `store` | bool | | 透传 | |
| `previous_response_id` | string | | 透传 | 上一轮响应 ID（多轮链式调用） |

### 调用示例

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:7863/v1", api_key="你的api_key")

resp = client.responses.create(
    model="deepseek-v4-flash",
    input="用一句话介绍杭州",
)
print(resp.output_text)
```

---

## GET /v1/models

模型列表，**纯动态拉取**（缓存 1h；拉取失败返回空列表 + 5min 负缓存）。

```json
{
  "object": "list",
  "data": [
    {
      "id": "cn:deepseek-v4-flash",
      "object": "model",
      "created": 1753600000,
      "owned_by": "workbuddy",
      "context_length": 131072,
      "max_output_tokens": 8192,
      "reasoning_supported_efforts": ["off", "low", "medium", "high"],
      "reasoning_default_effort": "medium",
      "name": "DeepSeek V4 Flash",
      "description": "...",
      "tags": ["..."],
      "pricing": {"multiplier": 1.0}
    }
  ]
}
```

| 字段 | 说明 |
|---|---|
| `id` | 带 `cn:` 前缀（global 模型带 `global:`），可直接作为 `model` 回传 |
| `context_length` | 上下文窗口；四级查找：上游动态值 → 静态种子表 → 本地缓存 → models.dev |
| `max_output_tokens` | 真实输出上限（探测值，上游虚高值会被钳制）；无法确定时省略 |
| `reasoning_supported_efforts` | 支持的思考档位列表；无档位模型省略该字段 |
| `reasoning_default_effort` | 默认档位 |
| `supports_reasoning` / `supports_tool_call` / `supports_images` / `can_disable_thinking` | 能力旗标 |
| `credits` / `max_allowed_size` / `reasoning_summary` | 积分倍率 / 最大请求体 / 推理摘要配置（有值时透出） |
| `name` / `description` / `vendor` | 名称、描述、厂商 |
| 其余字段 | 上游模型对象全字段透出，空值省略 |

---

## GET /status

账号池状态汇总（需要 Bearer 鉴权）。顶层字段：

| 字段 | 说明 |
|---|---|
| `total` / `healthy` / `cooling` / `disabled` / `in_flight_full` | 账号计数汇总 |
| `realm_totals` | 按 `cn` / `global` 分组的同口径计数 |
| `sticky_sessions` | 当前粘性会话数 |
| `redis_mode` | 状态后端模式：`noop`（纯内存）/ `upstash` 等 |
| `model_locks` | 按「域 + 模型」聚合的限流视图（见下） |
| `accounts` | 每账号详情数组 |

`accounts[]` 主要字段（运维观测用）：

| 字段 | 说明 |
|---|---|
| `uid` / `nickname` / `realm` / `enterprise` | 账号标识与域（`cn` / `global`）、是否企业版 |
| `credits` / `credits_total` / `credits_expiring` / `credits_earliest_expiry` | 积分余额与到期分布 |
| `cooling` / `cool_kind` / `cool_remaining_sec` / `until` / `reason` / `soft_streak` | 冷却状态：类型（soft/hard/breaker/degrade 等）、剩余秒数、解除时间、原因、指数退避档位 |
| `disabled` / `disabled_reason` / `paused` | 禁用 / 暂停状态 |
| `success_count` / `err_total` / `last_success` / `last_err` | 成功失败计数与最近时间 |
| `rate_limited_models` | 该账号被模型级限流（6004）的模型清单 |
| `token_usage` / `model_costs` / `checkin_done` | 用量、成本账本、今日是否已签到 |

`model_locks[]` 字段：`realm`、`model`、`locked_accounts`、`total_accounts`、`state`（`all_locked` 整池不可用 / `partial` 部分限流 / `no_account` 没号可用）、`earliest_unlock`、`all_unlock`、`reason`。无锁定时为空数组。

---

## GET /healthz

健康检查，**恒无鉴权**：

```json
{"healthy": 11, "total": 11, "realm_servable": {"cn": true, "global": true}, "service": "qiyworkapi"}
```

- **200**：存在 healthy 账号且未占满
- **503**：无可用账号（正常现象——加一个账号即恢复）
- 响应带 `X-Service: qiyworkapi` 头；`service` 字段用于区分同端口上的其他服务

---

## 错误语义

统一错误体：

```json
{"error": {"message": "具体描述", "type": "api_error", "code": "invalid_request", "gateway_hint": "网关补充的处置建议（可选）"}}
```

| HTTP | `code` | 场景 | 调用方建议 |
|---|---|---|---|
| 400 | `invalid_request` | 请求体读取失败 / JSON 解析错误 / 缺 `model` / Responses 字段非法 | 修请求，勿重试 |
| 401 | — | `api_key` 不匹配 | 检查密钥 |
| 404 | — | 路径错误（如少了 `/v1`） | 修 Base URL |
| 429 | 限流文案 / `6004` | 账号软冷却（600s 起指数退避，封顶 2h）；`6004` = **模型级**限额，冷却到上游重置时间并豁免切模型 | 可重试；冷却自动恢复 |
| 502 | `upstream_error` | 上游错误透传（含上游原始文案） | 按 message 处理 |
| 503 | — | 无可用账号：全池冷却 / 熔断 / 连败降权，或全池积分低于 `credit_floor`（**不放行**） | 可重试；查看 `/status` |

**重试总则**：429 / 502 / 503 可安全重试（网关侧换号）；400 / 401 / 404 重试无意义。流式请求中途断流按断连处理，不计入账号失败。

---

## 会话粘性机制

多轮对话默认绑定同一账号，避免上下文跳号。会话键按以下优先级提取（代码顺序）：

1. `metadata.conversation_id`
2. `metadata.conversationId`
3. `conversation_id`（顶层）
4. `conversationId`（顶层）
5. `prompt_cache_key`
6. **派生键**：`system + 首条 user 消息` 内容哈希（`d-` 前缀）——通用 OpenAI 客户端不传会话字段也能享受粘性

注意事项：

- 带 `user` 字段的请求**不派生**会话键（用户级语义优先）
- 绑定账号在**当前模型**被 6004 限额时自动解绑重分配（换模型不钉死在限额号上）
- 会话键同时作为轮级聚合主键，同会话多轮上报可区分

---

## 客户端接入配置

| 客户端 | 配置 |
|---|---|
| OpenAI SDK（Python / Node / Go） | `base_url = "http://127.0.0.1:7863/v1"`，`api_key = 你的api_key` |
| Cherry Studio / NextChat / ChatBox | API 地址填 Base URL，密钥填 api_key，模型列表用「获取模型」拉取 |
| Codex CLI 类（Responses 协议） | Base URL 同 `/v1`，端点 `/v1/responses` |

## 使用边界

- 本项目仅限管理**你自己的** CodeBuddy 账号（详见仓库 README 使用声明）
- 公网部署**必须**设置强 `api_key`，并建议置于 TLS 反向代理之后
- 网关按账号池治理流量：单号连续请求会触发上游限流冷却，这是保护机制而非故障；账号越多体验越稳

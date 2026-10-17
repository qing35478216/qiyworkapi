# WorkBuddy 一键部署教程

零基础、零命令行：**把一段话发给 WorkBuddy，几分钟后你就有一个自己的 OpenAI 兼容网关在线链接**——不用装 Go、不用装 Docker、不用买服务器。

## 你会得到什么

- 一个在线链接：面板（加账号）和 `/v1`（OpenAI 兼容 API）都能直接访问
- 网关跑着你自己的 CodeBuddy 账号，客户端填上 Base URL 就能用

## 准备工作（2 分钟）

| 准备 | 说明 |
|---|---|
| WorkBuddy | 就是你正在用的这个，装好并登录即可 |
| CodeBuddy 账号 | 一个或多个，用来登录网关（OAuth 用） |
| 干净的项目目录 | **不要带 `auths/` 和 `config.json`**，见下 |

## 第一步：准备一个干净的目录

部署时整个目录会被上传，所以**凭证绝不能带上去**（`auths/` 里是明文 accessToken）。

```bash
# 方式 A：下载 Release 压缩包解压（推荐，自带预编译二进制）
#   https://github.com/qing35478216/qiyworkapi/releases
tar xzf qiyworkapi-v1.13.0-linux-amd64.tar.gz
cd qiyworkapi-v1.13.0

# 方式 B：克隆源码
git clone https://github.com/qing35478216/qiyworkapi.git
cd qiyworkapi
```

确认目录里**没有** `auths/` 和 `config.json`（Release 包默认就不含）。

## 第二步：把下面这段话发给 WorkBuddy

在 WorkBuddy 里打开这个目录，然后原样发送：

```
帮我把当前目录的 qiyworkapi 项目部署起来，并用一键部署发布成在线链接：

1. 构建 Linux 二进制（CGO_ENABLED=0 go build -o wb2api ./cmd/server）
2. 准备配置：cp config.example.json config.json，并把 api_key 改成强随机值
3. 启动命令必须监听沙箱端口：WB2A_LISTEN=:$PORT ./wb2api -config config.json
   （程序用 WB2A_LISTEN 环境变量指定监听地址，绑定 0.0.0.0）
4. 冒烟测试：/healthz 和 /panel/ 都要能访问
5. 一键部署发布成在线链接，确保面板和 /v1 路径都能通过链接访问
6. 最后把在线链接、面板地址、API Base URL、api_key 汇报给我
```

> 这段话只是把关键点（端口绑定、不带凭证）说死，防止部署出来访问不通。
> 即使你不写这么细，WorkBuddy 也能自己搞定——说了更稳。

## 第三步：添加你的账号

1. 浏览器打开 WorkBuddy 给你的**面板链接**
2. 右上角「**添加账号**」→ 按提示在浏览器里完成 CodeBuddy 登录
3. 凭证自动落盘并**热加载进池，无需重启**（多账号重复此步）

> 账号凭证保存在部署沙箱里，重启部署不丢失；但**不要**把本机的 `auths/` 传上去。

## 第四步：接入你的客户端

| 项 | 值 |
|---|---|
| API Base URL | `https://<你的部署链接>/v1` |
| api_key | 第二步里设的值（`Authorization: Bearer <key>`） |
| 面板 | `https://<你的部署链接>/panel/` |

Cherry Studio / NextChat / ChatBox：API 地址填上面的 Base URL，密钥填 api_key，模型用网关 `/v1/models` 列表。SDK 示例：

```python
from openai import OpenAI
client = OpenAI(base_url="https://<你的部署链接>/v1", api_key="你的api_key")
print(client.chat.completions.create(
    model="deepseek-v4-flash",
    messages=[{"role": "user", "content": "hi"}],
).choices[0].message.content)
```

## 部署之后，继续用一句话让 WorkBuddy 干活

部署不是终点，这些都能直接说：

- 「把 api_key 改成 `sk-xxx`」→ 改配置并重新发布
- 「再加一个账号」→ 面板操作指引（OAuth 必须本人在浏览器完成，替代不了）
- 「帮我把签到时间改成早上 8 点」→ 改 `schedule.checkin_hours`
- 「部署有点慢，帮我看看日志」→ 排查
- 「重新发布」/「把部署下线」→ 更新或收回在线链接
- 「打包一份 macOS 版发给朋友，不要带我的账号」→ 出干净便携包

## 注意事项（重要）

- **公开链接 = 任何人可访问你的网关**：`api_key` 一定要设强随机值，否则别人能白嫖你的账号额度。
- **`auths/` 是明文 accessToken / refreshToken**：永远不要上传、不要提交 git、不要打包外发。
- 本项目**仅限管理你自己的账号**；批量注册、对外售卖接口配额违反上游平台条款——仓库 README 有完整使用声明，部署前读一下。
- 部署沙箱是单端口环境，本项目正好单端口；Upstash Redis 镜像是可选项，不配走纯内存模式，功能不受影响。

## 遇到问题

| 现象 | 原因 / 处理 |
|---|---|
| 链接打不开 | 让 WorkBuddy 看部署日志（启动命令没绑 `$PORT` 最常见） |
| `/healthz` 返回 503 | 还没加账号——去面板「添加账号」 |
| API 返回 401 | api_key 不对；确认请求头是 `Authorization: Bearer <key>` |
| API 返回 404 | Base URL 少了 `/v1`，或多了尾斜杠 |
| 想改配置 | 把要求发给 WorkBuddy，它改完会问你要不要同步到线上 |

更多细节：[README](README.md)（完整文档）· [用法说明](用法说明.md)（本机/Docker 部署）· [轮询机制](轮询机制.md)（选号与调度原理）。

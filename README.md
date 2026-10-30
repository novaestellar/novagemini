<p align="center">
  <img src="docs/logo.png" width="132" alt="novagemini">
</p>

<h1 align="center">novagemini</h1>

> **把 Gemini 网页端变成 OpenAI 兼容 API 的自托管代理** — 文本 · 思考链 · 联网搜索 · 生图 · 图生图 · 音乐 · 画布 · 视频。单个二进制、零外部依赖、带账号池与出口池的中控面板。

<p align="center">
  <img src="https://img.shields.io/badge/version-4.22.0-9B72CB" alt="version">
  <img src="https://img.shields.io/badge/go-1.26.8%2B-D96570" alt="go">
  <img src="https://img.shields.io/badge/license-MIT-4285F4" alt="license">
  <img src="https://img.shields.io/badge/backend-gemini.google.com-2E2733" alt="backend">
</p>

---

## 📋 这是什么

novagemini 不是 API key 转卖，也不是逆向私有接口的猜谜。它用真实的浏览器指纹（chrome_146 TLS/HTTP2）向 `gemini.google.com` 的 `StreamGenerate` 端点发请求，把响应翻译成 OpenAI 协议，再把结果交回给你的客户端。

三个设计取舍决定了它和同类工具的区别：

**单二进制，无运行时依赖。** `go build` 出来的文件丢到任何一台机器上就能跑，面板、CSS、字体、Chart.js 全部 embed 进二进制。没有 Node、没有 Python、没有外部 CDN —— 内网和离线环境一样能开面板。

**状态都在一个 SQLite 文件里。** 账号、出口、请求记录、配额、API key 全在 `./data/gemini.db`。备份就是复制一个文件；迁移就是把它带走。

**面板是工作台，不是展示页。** 概览、请求记录、代理池、Cookie 池、媒体影集、运行时设置，六个页签对应六件真正要做的事。改配置立刻生效，不需要重启。

---

## ✨ 核心特性

### 文本与推理

- **OpenAI 兼容协议**：`/v1/chat/completions`、`/v1/models`、`/v1/responses`，流式与非流式都支持。
- **19 个模型**：Gemini 3.1 Pro、3.5 Flash-Lite、3.6 Flash、3.7 Flash、3.8 Flash、4 Argon（含 low / medium / high 三档推理深度），以及各自的 thinking 变体。
- **思考链透传**：思考模型的推理过程通过 `reasoning_content` 字段返回，客户端可以只显示正文、也可以全部显示。
- **联网搜索**：内置 `web_search` 工具，兼容 MCP 协议（`/mcp`），Claude Code、Cursor 之类可直接接。

### 媒体生成

- **文本生图 / 图生图**：`gemini-image`，返回 base64 data URL 或落盘链接。
- **音乐**：`gemini-music`（Lyria），异步任务，完成后写入 `data/media/`。
- **视频**：`gemini-video`（Veo），异步任务 + 轮询，适合长任务。
- **Canvas**：`gemini-canvas`，直接产出可交互的 HTML 文档。
- **影集**：生成物自动进面板「影集」页，按日期分组，可直接预览与下载。

### 账号池（Cookie 池）

- **一个浏览器配置驱动多个 Google 账号**：用 `auth_user` + `X-Goog-AuthUser` + `/u/N/app` 定位，同一份 cookie 服务 10 个账号。
- **真实分层识别**：读取每个账号自己的模型清单（`otAQ7b`）判断 pro / free，而不是猜。
- **手动覆盖**：探测不准时可以给账号打上 `tier_override`，覆盖自动结果。
- **自动轮换**：Cookie 过期自动用 `RotateCookies` 续期；续期失败自动降级、自动换号、自动恢复。
- **健康度**：每个账号记录成功/失败次数、最近错误、图片/音乐/思考配额，面板上一眼可见。

### 出口池（代理池）

- **每个代理是独立的 IP slot**，各自拥有并发 / RPM / RPH 配额，池子越大总容量越大。
- **熔断与自愈**：连续失败自动熔断，默认 120 分钟后自动放回池子（实测被拦出口 106–121 分钟自恢复）。
- **直连兜底可选**：默认不再退回直连，避免代理满时把主机 IP 一起打爆。

### 运维

- **管理面板**：六页签，运行时配置保存即生效。
- **请求留痕**：每次请求记录模型、真实上游、账号、出口、TTFB、总耗时、Token 估算。
- **限流**：按 IP 的并发 / RPM / RPH 三道闸，防单点打爆。
- **保留期**：`retention_days` 自动清理历史，默认 30 天。
- **接入 9Router**：作为 OpenAI 兼容 provider 挂进去，前缀 `gw2a`。

---

## 🚀 快速安装

### 方式一：下载二进制（最省事）

到 [Releases](https://github.com/novaestellar/novagemini/releases) 下载对应平台的文件（`windows-amd64`、`linux-amd64`、`darwin-arm64`），重命名成 `novagemini` 后直接运行。

```powershell
# Windows
.\novagemini.exe
```

```bash
# Linux / macOS
chmod +x novagemini && ./novagemini
```

### 方式二：从源码构建

```bash
git clone https://github.com/novaestellar/novagemini.git
cd novagemini
go build -o novagemini .
./novagemini
```

需要 Go 1.26.8 或更新。构建产物约 20 MB，面板资源已全部 embed。

### 方式三：Docker

```bash
docker build -t novagemini .
docker run -d --name novagemini -p 8083:8083 -v $(pwd)/data:/app/data novagemini
```

### 启动后应该看到

```text
novagemini v4.22.0
  Listening:   http://127.0.0.1:8083
  Base URL:    http://localhost:8083/v1
  API key:     sk-gemini-...383d  (mutable in admin UI)
  Admin UI:    http://localhost:8083/admin  (no auth (open))
  DB:          ./data/gemini.db
  Models:      [gemini-3.1-pro ... gemini-video]   19 个
  Cookie:      0/0 in pool
  Proxy:       0 in pool
  Retry:       3x / 2s
```

第一次启动会自动生成：

- `config.json` — 部署期配置
- `data/gemini.db` — 账号、出口、请求记录
- 一个随机的 API key，面板「设置」页可见也可改

打开 `http://127.0.0.1:8083/admin` 就是面板。

### 环境变量（可选）

| 变量 | 作用 |
|---|---|
| `ADMIN_TOKEN` | 给面板加访问密码；不设则本机直接进 |
| `API_KEY` | 固定 `/v1/*` 的 key；不设则自动生成 |
| `ALT_PORTS` | 额外监听端口，逗号分隔；`none` 表示不监听 |
| `SQL_DSN` | 换数据库，例如 `mysql://user:pass@host:3306/db`；不设则用 SQLite |

以上是程序真正读取的全部环境变量（代码里逐个 `os.Getenv`）。监听地址与端口不在其中：用 `config.json` 的 `host` / `port`，或命令行 `--port`。`HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY` 会被显式忽略——走系统代理会绕开 TLS 指纹，反而更容易被拦；出口代理请在面板「代理池」或 `--proxy` 设置。

完整模板见 [`.env.example`](.env.example)。`.env` 不入库。

---

## 💻 使用

### curl

```bash
curl http://127.0.0.1:8083/v1/chat/completions \
  -H "Authorization: Bearer sk-gemini-xxxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.6-flash",
    "messages": [{"role": "user", "content": "用一句话解释什么是 TLS 指纹"}],
    "stream": false
  }'
```

### OpenAI Python SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8083/v1",
    api_key="sk-gemini-xxxx",
)

resp = client.chat.completions.create(
    model="gemini-3.6-flash",
    messages=[{"role": "user", "content": "写一个 Go 的 worker pool"}],
)
print(resp.choices[0].message.content)
```

### 思考链

```python
resp = client.chat.completions.create(
    model="gemini-3.8-flash-thinking",
    messages=[{"role": "user", "content": "证明根号 2 是无理数"}],
)
print(resp.choices[0].message.reasoning_content)  # 推理过程
print(resp.choices[0].message.content)            # 最终答案
```

### 生图

```bash
curl http://127.0.0.1:8083/v1/chat/completions \
  -H "Authorization: Bearer sk-gemini-xxxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"gemini-image","messages":[{"role":"user","content":"一只坐在窗台上的橘猫，清晨侧光"}]}'
```

返回 base64 data URL；打开 `save_media_to_disk` 后返回 `/files/...` 链接，文件落在 `data/media/YYYY-MM-DD/`。

### 接入 9Router（可选）

在 9Router 里加一个 OpenAI 兼容 provider：

```json
{
  "prefix": "gw2a",
  "apiType": "chat",
  "baseUrl": "http://127.0.0.1:8083/v1"
}
```

之后模型列表里会出现 `gw2a/gemini-3.6-flash`、`gw2a/gemini-4-argon` 等 19 个条目。

---

## 🧩 模型

| 模型 | 说明 | 需要账号 |
|---|---|---|
| `gemini-3.1-pro` / `-thinking` | 最强通用模型 | 是（无 cookie 降级到 Flash-Lite） |
| `gemini-3.5-flash-lite` / `-thinking` | 最快，最省 | 否 |
| `gemini-3.6-flash` / `-thinking` | 日常主力 | 否 |
| `gemini-3.7-flash` / `-thinking` | 3.8 的别名（同一 hex） | 是 |
| `gemini-3.8-flash` / `-thinking` | 新模型，需要账号持有 rollout | 是（付费 + rollout） |
| `gemini-4-argon` | 复杂任务 | 是（付费 / Ultra） |
| `gemini-4-argon-low` / `-medium` / `-high` | 推理深度三档 | 是 |
| `gemini-4-argon-thinking` | Argon + 长思考 | 是 |
| `gemini-image` | 生图 / 图生图 | 是 |
| `gemini-music` | 音乐（Lyria） | 是 |
| `gemini-video` | 视频（Veo，异步） | 是（通常付费） |
| `gemini-canvas` | 交互式 HTML | 是 |

**降级规则**：免费账号请求付费模型时，Google 会静默返回 `3.6 Flash` 或 `3.5 Flash-Lite`。novagemini 会识别这种「名不副实」并换号，实在没有账号可用时才报 502 —— 不会把降级结果当成成功返回。

---

## 🍪 Cookie 池

没有 cookie 也能用：免费模型走匿名通道，能力受限（无思考链、无付费模型、无媒体）。

### 怎么导出

1. 浏览器登录 `gemini.google.com`。
2. 打开 DevTools → Application → Cookies → `https://gemini.google.com`。
3. 复制这些键：`SID`、`HSID`、`SSID`、`APISID`、`SAPISID`、`__Secure-1PSID`、`__Secure-1PSIDTS`、`__Secure-3PSID`。
4. 面板「Cookie 池」→ 添加账号 → 粘贴成 `k=v; k=v` 形式。

### 一个浏览器配置带多个账号

Google 一个浏览器配置最多 10 个账号（`/u/0` … `/u/9`）。novagemini 支持在同一个 cookie 下用 `authuser` 选账号：

- cookies 里的 `__NOVA_AU=<n>` 是内部标记，发请求前自动剥离；
- 面板上每个账号有自己的 `auth_user`，可以手动改；
- XSRF 缓存按「cookie + authuser」分开，不会串号。

超过 10 个账号请开第二个 Firefox 配置，而不是硬塞。

### 账号池怎么工作

请求进来 → 挑一个支持该模型的账号 → 失败就换号 → 全都失败才报错。换号时会重新取 XSRF，并把上游返回的 `Set-Cookie` 写回数据库，这样账号不会因为一直发旧 cookie 被判定过期。

---

## 🛡️ 代理池

- 每个代理 = 一个独立出口 + 独立配额；
- 请求自动找有空位的出口，全满返回 HTTP 429；
- 连续失败 5 次熔断，默认 120 分钟自动放回；
- 「重置」手动恢复；
- 配了代理就不再用直连，除非打开「无可用代理时退回直连」。

实测单个出口打 80–180 次就会被 Google 拦，扩容只能加出口。

---

## 🎭 指纹模拟

默认 `chrome_146`，TLS + HTTP/2 指纹与真实浏览器一致。可换成 `chrome_120`、`firefox_120` 等（`impersonate` 配置项）。这一层不是装饰：Grease 扩展、扩展头顺序、`x-goog-ext-*` 字段都跟着走，缺一个就会撞上「无法验证你的会话」。

---

## 🖥️ 管理面板

| 页签 | 内容 |
|---|---|
| 概览 | 24 小时请求量、成功率、平均延迟、模型分布、出口分布 |
| 请求记录 | 每次请求的模型、真实上游、账号、出口、TTFB、总耗时、Token |
| 代理池 | 增删改、启用/禁用、熔断状态、最近错误、手动重置 |
| Cookie 池 | 账号增删、标签、`auth_user`、层级覆盖、健康度、配额 |
| 影集 | 生成的图片/音乐/视频/画布，按日期分组，可直接下载 |
| 设置 | 运行时参数、API key、测试连通性 |

面板默认无密码（只绑 `127.0.0.1`）。要暴露到公网，先设 `ADMIN_TOKEN`，再走反代 + HTTPS。

---

## ⚙️ 配置

### 运行时配置：面板「设置」页，保存即生效

| 键 | 默认 | 说明 |
|---|---|---|
| `retry_attempts` | 3 | 失败重试次数 |
| `retry_delay_sec` | 2 | 重试间隔 |
| `request_timeout_sec` | 180 | 上游静默上限（不是总时长）。3.7/3.8 正常可能要 200-700 秒，所以这里只砍「停止发送数据」的请求 |
| `per_ip_concurrent` | 5 | 单 IP 并发 |
| `per_ip_rpm` | 30 | 单 IP 每分钟 |
| `per_ip_rph` | 80 | 单 IP 每小时 |
| `retention_days` | 30 | 历史保留天数 |
| `max_prompt_bytes` | 128000 | 提示词上限 |
| `proxy_cooldown_min` | 120 | 熔断恢复时间 |
| `default_model` | `gemini-3.6-flash` | 不指定模型时的默认值 |
| `multi_turn` | false | 复用上游会话 |
| `auto_delete_conversation` | false | 用完即删上游会话 |
| `temporary_chat` | false | 临时会话，不写入历史 |
| `anon_first` | false | 先试匿名，再试账号 |
| `fallback_direct` | false | 代理全满时退回直连 |
| `save_media_to_disk` | false | 媒体落盘并返回链接 |
| `media_inline_kb` | 1024 | 超过此值改走文件 |

### 部署期配置：改了需要重启

| 键 | 默认 | 说明 |
|---|---|---|
| `port` | 8083 | 监听端口 |
| `host` | 127.0.0.1 | 绑定地址 |
| `alt_ports` | `[]` | 额外监听端口（谨慎使用，会抢占） |
| `db_path` | `./data/gemini.db` | SQLite 路径 |
| `admin_token` | 空 | 面板密码 |
| `impersonate` | `chrome_146` | 指纹 |
| `gemini_bl` | `boq_gemini-web-uiserver_...` | 上游 build 号 |
| `gemini_bl_auto` | true | 自动从 `/app` 抓最新 build 号 |

---

## 📡 OpenAI 协议覆盖

| 端点 | 方法 | 说明 |
|---|---|---|
| `/v1/models` | GET | 模型清单 |
| `/v1/chat/completions` | POST | 对话，支持 `stream` 与 `reasoning_content` |
| `/v1/responses` | POST | Responses API |
| `/v1/videos` | POST | 创建视频任务 |
| `/v1/videos/{id}` | GET | 查询视频任务 |
| `/v1/media` | POST | 创建媒体任务（音乐等） |
| `/v1/media/{id}` | GET | 查询媒体任务 |
| `/api/media` | GET | 影集数据 |
| `/files/{date}/{name}` | GET | 媒体文件 |
| `/gallery` | GET | 影集页 |
| `/mcp` | POST | MCP（`web_search` 工具） |
| `/admin` | GET | 管理面板 |

---

## 🧪 最佳实践

**日常对话用 3.6 Flash。** 3.7 / 3.8 是同一个模型（共用 hex `56fdd199312815e2`），它们的中位延迟在 100 秒以上，尾部能到 700 秒 —— 这不是本项目的排队，而是 Google 对 rollout 的调度。要质量就用 3.8，要响应就用 3.6，不要拿 3.8 做交互式补全。

**媒体请求一次一条。** 生图、音乐、视频都吃配额，且失败重试会被 Google 视为异常模式。一次成功比五次重试值钱。

**账号按浏览器配置分组。** 一个 Firefox 配置最多 10 个账号；第 11 个不会报错，只会静默回到 `/u/0`。超过就开新配置。

**浏览器和服务不要同时用同一份 cookie。** 浏览器开着时 cookie 在内存里轮换，磁盘上的副本会过期，表现为 `no SNlM0e in page`。要刷新就跑一次账号刷新，之后浏览器可以关掉。

**面板不要裸奔在公网。** `admin_token` 为空时面板无密码。反代 + HTTPS + 强 token，三件一起上。

**cookie 保鲜有两条腿。** 服务端每 10 分钟对每个账号跑一次 `RotateCookies`（`rotate_disabled=0`）；同时 Windows 计划任务 `novagemini-cookie-refresh` 每 30 分钟把浏览器配置里的会话重新灌进服务端（`tools\refresh-cookies.cmd`）。浏览器开着时服务端会被 429，靠这份计划任务兜底；浏览器关着时靠服务端自己续。两条都断，账号才会真的过期。

**备份就是复制文件。** `data/gemini.db` + `config.json`，两个文件拿走，换机器即恢复。

---

## 📁 项目结构

```text
novagemini/
├── main.go                 入口、命令行参数
├── config.json             部署期配置（运行时生成）
├── internal/app/
│   ├── app.go              路由与中间件
│   ├── gemini.go           上游请求构造、模型表、响应解析
│   ├── sse.go              OpenAI 流式编码
│   ├── conversation.go     会话、媒体任务
│   ├── cookie_pool.go      账号池、分层、配额
│   ├── authuser.go         多账号（auth_user）支持
│   ├── tier.go             分层探测（otAQ7b）
│   ├── xsrf.go             XSRF 获取与缓存
│   ├── rotate.go           cookie 自动续期
│   ├── proxy.go            出口池、熔断
│   ├── ratelimit.go        按 IP 限流
│   ├── admin.go            面板 API
│   └── admin_ui/           面板静态资源（embed）
├── tools/
│   └── cookie_grab.py      从浏览器配置批量导入/刷新 cookie
└── data/
    ├── gemini.db           SQLite
    └── media/              生成物
```

---

## 🚧 已知边界

**3.8 / 4 Argon 的可用性按账号变化。** Google 按账号灰度，同一个模型今天能用的账号明天可能被降级成 3.6。服务会识别并换号，但换不到合适账号时只能返回 502。

**媒体配额稀少。** 每个账号的图片 / 音乐 / 视频配额独立且很小，用尽后需要等重置或换号。

**上游没有官方承诺。** 这是对网页端的封装，Google 改协议就得跟着改。`gemini_bl` 和模型 hex 是这套东西最脆弱的两处。

**长任务不适合同步等待。** 视频、音乐是异步任务，用 `/v1/videos` 与 `/v1/media` 轮询，不要在 chat 端点里死等。

---

## 🔧 故障排查

| 现象 | 原因 | 处理 |
|---|---|---|
| `no SNlM0e in page` | cookie 过期，或浏览器占用同一配置 | 刷新账号，关掉浏览器 |
| 502 `downgraded: served "3.6 Flash"` | 该账号没有这个模型的权限 | 换号，或改用 3.6 |
| 502 `all proxy slots full` | 出口全满 | 加代理，或开 `fallback_direct` |
| 401 | API key 不对 | 面板「设置」页看当前 key |
| 面板打不开 | `host` 绑了 `127.0.0.1` | 本机访问，或改 `host` |
| 思考链为空 | 用了非 thinking 模型 | 换 `-thinking` 变体 |
| 请求很慢但 TTFB 很小 | 上游在排队，不是本项目 | 见「最佳实践」 |

日志输出到 `data/srv.log`（stdout）与 `data/srv.err`（stderr）。重定向启动：

```powershell
Start-Process .\novagemini.exe -RedirectStandardOutput data\srv.log -RedirectStandardError data\srv.err
```

---

## ❓ 常见问题

**要不要 Google 账号？** 免费模型不需要。要思考链、付费模型、媒体，需要。

**能不能多机共享一个数据库？** SQLite 不适合。要共享请用 MySQL / PostgreSQL（`SQL_DSN`）。

**为什么不直接调用官方 API？** 官方 API 要 key 要计费，且部分模型不在公开 API 上。这个项目的目标是让已经订阅的账号替自己干活。

**会不会被封？** 任何网页端封装都有这个风险。限流、指纹、账号轮换都是为了把风险压到最低，但没有任何保证。

**和 gemini-web2api 有什么区别？** 那个是单账号、单文件、无面板；这个有账号池、出口池、面板、请求留痕和媒体影集。

---

## 🔒 隐私

- 所有请求只发往 `gemini.google.com`，不经过任何第三方；
- 不收集、不上报任何使用数据；
- 面板只绑 `127.0.0.1`；
- `.env`、`config.json`、`data/`、浏览器配置一律不入库（见 `.gitignore`）。

---

## 📄 License

MIT。见 [LICENSE](LICENSE)。

---

## 🙏 致谢

- [bogdanfinn/tls-client](https://github.com/bogdanfinn/tls-client) — 浏览器指纹
- [tiktoken-go](https://github.com/pkoukk/tiktoken-go) — Token 估算
- 面板视觉取自 [novaCF](https://github.com/novaestellar/novacf) 的暖纸 + 淡紫体系

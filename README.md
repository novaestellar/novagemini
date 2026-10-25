# NovaGemini

> **把 Gemini 网页端变成 OpenAI 兼容 API 的自托管代理** — 文本 · 思考链 · 联网搜索 · 生图 · 图生图 · 音乐 · 画布 · 视频,单二进制,带限流与账号池的中文管理面板。

<p align="center">
  <img src="docs/logo.png" width="132" alt="NovaGemini">
</p>

<p align="center">
  <a href="https://github.com/novaestellar/novagemini/releases/latest"><img src="https://img.shields.io/badge/release-4.22.0-4285F4" alt="release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-9B72CB" alt="license"></a>
  <img src="https://img.shields.io/badge/go-1.26%2B-D96570" alt="go">
  <img src="https://img.shields.io/badge/docker-distroless-6E7B8B" alt="docker">
</p>

**中文** | [English](README_EN.md)

---

## 📋 这是什么

NovaGemini 是一个反向代理:它不说 Google 官方 API(`generativelanguage.googleapis.com`)那一套,而是直接复刻 `gemini.google.com` 网页端协议。所以:

- **不需要 Google API Key**
- **不需要付费配额**
- 匿名(不挂 cookie)就能跑文本模型

```
[OpenAI SDK / Cherry Studio / Cursor / Dify / new-api / ...]
    ↓ http://localhost:8083/v1/chat/completions
[NovaGemini]
    ↓ 复刻 gemini.google.com 网页协议
[Google Gemini 网页端]
```

代价是能力边界跟着网页端走:网页端有的(联网搜索、生图、音乐、画布、视频)这里都能用;网页端没有的(深度研究)这里也没有。

---

## ✨ 核心特性

| 特性 | 说明 |
|------|------|
| 🔌 **OpenAI 兼容** | `/v1/chat/completions` · `/v1/models` · `/v1/responses`,普通对话是真实增量流式 |
| 🎬 **视频接口** | `/v1/videos` 异步任务:创建 → 轮询 → 下载 MP4(需 Pro 号) |
| 🖼️ **多模态** | 读图 / 读视频 / 文生图 / **图生图** / 音乐 / 画布,产物以 base64 内联返回 |
| 🧠 **思考链** | `reasoning_content` 字段,客户端渲染成可折叠「思考过程」 |
| 🌐 **联网搜索** | 基础模型自带;另在 `/mcp` 暴露成 `web_search` 工具 |
| 🎭 **TLS 指纹** | `utls` 模拟 Chrome 146 真实握手,不是 SDK 默认指纹 |
| 🍪 **账号池** | 多 Google 账号最久未用优先轮转,自动换发 `__Secure-1PSIDTS` + 保活 |
| 🛡️ **代理池** | 运行时增删改、失败熔断、每代理独立限流槽,账号粘住自己的出口 |
| ⏱️ **三级限流** | 每出口独立:并发 / RPM / RPH |
| 🖥️ **中文面板** | 概览 · 请求记录 · 代理池 · 账号池 · 设置,配置改完即时生效 |
| 💾 **持久化** | SQLite 默认,可选 MySQL / PostgreSQL(设 `SQL_DSN` 即可) |
| 🔒 **隐私** | prompt 与回复内容**永不入库**,只存元数据(长度、耗时、模型、状态) |

---

## 🚀 快速安装

### 方式一:下载二进制(最省事)

到 [Releases](https://github.com/novaestellar/novagemini/releases) 挑对应平台的包。不用 Go、不用 Docker,单个文件就是全部。

```bash
chmod +x novagemini_*
./novagemini_* --port 8083 --admin-token your-admin-token
```

数据默认落在 `./data/gemini.db`,换位置加 `--db /your/path.db`。

想用 MySQL / PostgreSQL,设 `SQL_DSN` 环境变量即可(不设就是 SQLite,无需改代码):

```bash
# MySQL(也接受 go-sql-driver 原生 user:pass@tcp(host:3306)/dbname)
SQL_DSN="mysql://user:pass@host:3306/dbname"

# PostgreSQL
SQL_DSN="postgres://user:pass@host:5432/dbname?sslmode=disable"
```

建表自动完成,三种库共用同一套 schema。单机用默认 SQLite 最省事,多实例共享一个池子才需要 MySQL / PG。

### 方式二:Docker

```bash
docker run -d --name novagemini \
  -p 127.0.0.1:8083:8083 \
  -v "$PWD/data:/data" \
  -e ADMIN_TOKEN=your-admin-token \
  ghcr.io/novaestellar/novagemini:latest
```

用 compose 的话把 `docker-compose.yml` 单独下下来就行,不用 clone:

```bash
curl -O https://raw.githubusercontent.com/novaestellar/novagemini/main/docker-compose.yml
ADMIN_TOKEN=your-admin-token docker compose up -d
```

镜像基于 distroless,以 nonroot(uid 65532)运行 —— 用 bind mount 时宿主目录要可写,否则会报 `unable to open database file (14)`。

### 方式三:从源码跑

```bash
git clone https://github.com/novaestellar/novagemini
cd novagemini
go build -o novagemini .
./novagemini --port 8083 --admin-token your-admin-token
```

### 启动后应该看到

```
novagemini v4.22.0
  Listening:   http://0.0.0.0:8083
  Base URL:    http://localhost:8083/v1
  API key:     sk-gemini-XX...XXXX  (mutable in admin UI)
  Admin UI:    http://localhost:8083/admin  (token auth)
  DB:          ./data/gemini.db
  Models:      [gemini-3.5-flash-lite gemini-3.6-flash]
  Cookie:      none (anonymous)
  Proxy:       none
  Impersonate: chrome_146
  Tokenizer:   tiktoken cl100k_base
  Per-IP 限流: 并发=5 / RPM=30 / RPH=80
  Retry:       3x / 2s
```

首次启动会自动生成 API key(banner 里是打码的,完整值在管理面板「设置」页)。

### 环境变量(可选)

只有三个变量在启动时读取,改了必须重启。复制模板即可:

```bash
cp .env.example .env
```

| 变量 | 说明 |
|------|------|
| `ADMIN_TOKEN` | 面板登录 token。留空 = 面板不鉴权(仅绑 `127.0.0.1` 时可接受) |
| `API_KEY` | 锁定 `/v1/*` 的 key(面板改不了),用于不希望运行时被改的部署 |
| `SQL_DSN` | MySQL / PostgreSQL 连接串,不设即 SQLite |

端口、绑定地址、DB 路径不在环境变量里,走 `config.json` 或命令行参数。

---

## 💻 使用

### curl

```bash
curl http://localhost:8083/v1/chat/completions \
  -H "Authorization: Bearer sk-gemini-..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.6-flash",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

> **Windows PowerShell 注意**:`curl` 是 `Invoke-WebRequest` 的别名,会重新解释 JSON 引号。用 `curl.exe` 加 `--%`。

### OpenAI Python SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8083/v1",
    api_key="sk-gemini-...",  # 面板「设置」页查看
)
resp = client.chat.completions.create(
    model="gemini-3.6-flash",
    messages=[{"role": "user", "content": "解释量子纠缠"}],
)
print(resp.choices[0].message.content)
```

### 客户端接入

任何 OpenAI 兼容客户端(Cherry Studio / ChatBox / Open WebUI / Dify / Cursor / …)都是同一套填法:

| 字段 | 值 |
|------|-----|
| Base URL / API 地址 | `http://localhost:8083/v1`(部分客户端只填 `http://localhost:8083`,会自己拼 `/v1`) |
| API Key | 管理面板「设置」页的 `sk-gemini-...` |
| 模型 | `gemini-3.6-flash` |

new-api / one-api 建渠道:类型选 OpenAI,Base URL 填 `http://localhost:8083`(与 new-api 同在 docker 里就写 `http://host.docker.internal:8083` 或容器名)。

**Codex CLI** 走 `/v1/responses`,把 base url 指到 `http://localhost:8083/v1` 即可。

**Gemini CLI 接不了**:它要的是 Google 原生的 `/v1beta/models/{model}:generateContent`,本项目只暴露 OpenAI 形状的接口,没做 `/v1beta`。

另有不鉴权的健康检查 `GET /`,返回 `{"status":"ok","version":...,"models":[...]}`,给探活用。

### 生图 / 图生图 / 音乐 / 画布

这些都跟普通对话一样走 `/v1/chat/completions`,在 `user` 消息里写要什么即可。产物字节以 base64 data URL 放进返回的 `content`(图片 `![image](data:image/png;base64,...)`,音频 `[audio](data:audio/mpeg;base64,...)`),支持 markdown 的客户端会直接渲染。

```bash
# 文生图
curl http://127.0.0.1:8083/v1/chat/completions \
  -H "Authorization: Bearer sk-gemini-..." -H "Content-Type: application/json" \
  -d '{"model":"gemini-image","messages":[{"role":"user","content":"画一只戴宇航员头盔的橘猫"}]}'
```

**图生图**(给一张图 + 一句改图指令):

```json
{
  "model": "gemini-image",
  "messages": [{
    "role": "user",
    "content": [
      {"type": "text", "text": "把物体的颜色改成蓝色"},
      {"type": "image_url", "image_url": {"url": "data:image/png;base64,<...>"}}
    ]
  }]
}
```

不转外链是有意的 —— Gemini 的产物链要带 cookie 才下得到,直接把链给客户端它打不开,所以服务端取回字节再转 base64。产物的 base64 **不计进** `completion_tokens`(否则一张图上百万 token,下游按它计费就离谱了)。这些都要登录态,没 cookie 时它们不进 `/v1/models`。

### MCP(web_search 工具)

同一个进程、同一个端口还挂了 MCP server,在 `/mcp` 上,把 Gemini 网页端的联网搜索暴露成 `web_search` 工具。不用单独起进程,匿名即可搜。

```json
{
  "mcpServers": {
    "gemini-search": {
      "url": "http://localhost:8083/mcp",
      "headers": { "Authorization": "Bearer sk-gemini-你的key" }
    }
  }
}
```

```bash
# Claude Code
claude mcp add --transport http gemini-search http://localhost:8083/mcp --header "Authorization: Bearer sk-gemini-你的key"
```

`web_search(query)` 返回 Gemini 联网查证后的答案,末尾附 `Sources:` 来源清单。

---

## 🧩 模型

Gemini 网页端服务端只认固定几个模型(清单来自 `batchexecute?rpcids=otAQ7b`)。

| 模型 | 说明 | 需要 |
|------|------|------|
| `gemini-3.6-flash` | 全方位,默认 | 匿名 |
| `gemini-3.5-flash-lite` | 极速、轻量 | 匿名 |
| `gemini-3.1-pro` | 最强,每次回答带思考链 | cookie |
| `gemini-3.8-flash` | 新款 Flash | **付费**号 cookie(免费号降级成 3.5 Flash-Lite) |
| `gemini-4-argon` | 4 Argon(复杂任务) | **付费 / Ultra** 号 cookie(免费号降级) |
| `gemini-4-argon-low` | 4 Argon,推理深度 low / 快 | 付费 / Ultra 号 |
| `gemini-4-argon-medium` | 4 Argon,推理深度 medium / 均衡 | 付费 / Ultra 号 |
| `gemini-4-argon-high` | 4 Argon,推理深度 high / 深 | 付费 / Ultra 号 |
| `gemini-3.6-flash-thinking` | 3.6 Flash 开扩展思考 | cookie |
| `gemini-3.5-flash-lite-thinking` | 3.5 Flash-Lite 开扩展思考 | cookie |
| `gemini-3.1-pro-thinking` | 3.1 Pro 开扩展思考 | cookie |
| `gemini-3.8-flash-thinking` | 3.8 Flash 开扩展思考 | 付费号 cookie |
| `gemini-4-argon-thinking` | 4 Argon 开扩展思考(上游叫 High 档) | 付费 / Ultra 号 |
| `gemini-image` | 生图 / 图生图(Nano Banana),产物 base64 | cookie |
| `gemini-music` | 音乐(Lyria,约 30 秒),产物 base64 | cookie |
| `gemini-canvas` | 画布,生成交互 HTML 文档(内联返回) | cookie |
| `gemini-video` | 生视频(异步,几十秒~几分钟),产物 base64 MP4 | Pro / 付费号 |

`gemini-3.7-flash`(及 `-thinking`)是 `gemini-3.8-flash` 的别名 —— Google 把 3.7 那个 hex 原地升成了 3.8。

没配 cookie 时 `/v1/models` 只返回前两个,选 `gemini-3.1-pro` 会直接报错并说明原因。因为匿名请求它必然被静默降级成 3.5 Flash-Lite —— 与其让客户端拿到一个「成功但其实不是 Pro」的回复,不如在选型时就失败。

`-thinking` 是网页 UI 上「扩展思考」的开关,跟模型正交 —— 三个基础模型都能开,不是三个额外的模型。只在登录态生效:匿名请求带上这个开关会被服务端静默忽略,所以没 cookie 时不暴露。

---

## 🧠 思考链(reasoning_content)

`gemini-3.1-pro` 每次回答前会先输出一段推理过程,本项目把它按事实标准的 `reasoning_content` 字段暴露出来,new-api、Cherry Studio、ChatBox 等客户端会渲染成可折叠的「思考过程」。

```json
{"choices":[{"message":{
  "role":"assistant",
  "content":"The core rule of this classic puzzle is...",
  "reasoning_content":"**Defining the Constraints**\n\nI've successfully defined..."
}}],
 "usage":{"prompt_tokens":26,"completion_tokens":337,"reasoning_tokens":77,"total_tokens":363}}
```

流式按 `delta.reasoning_content` 推,且思考块全部推完才开始推正文(上游就是这个顺序),客户端观感跟网页端一致。

`reasoning_tokens` 单独计数、**不含**在 `completion_tokens` 里:思考链默认被客户端折叠不展示,算进去等于让用户为看不见的输出买单。要计费的自己把两者相加。

---

## 🍪 Cookie(可选,解锁更多能力)

挂上 Google 账号 cookie 后请求走登录态,额外解锁:`gemini-3.1-pro`、扩展思考版、读图 / 读视频、生图 / 图生图 / 音乐 / 画布 / 视频,以及更长的上下文。

### 怎么导出

1. 浏览器登录 `gemini.google.com`
2. DevTools (F12) → Application → Cookies → `https://gemini.google.com`
3. 复制:`SID` / `HSID` / `SSID` / `APISID` / `SAPISID` / `__Secure-1PSID` / `__Secure-1PSIDTS`
4. 粘进面板「账号池 → 添加账号」;或写成 `cookie.txt` 后启动加 `--cookie-file cookie.txt`

```
SID=...; HSID=...; SSID=...; APISID=...; SAPISID=...; __Secure-1PSID=...; __Secure-1PSIDTS=...
```

JSON 形式 `{"cookie": "SID=...; ...", "sapisid": "..."}` 也吃。带 `SAPISID` 的请求会自动算 `SAPISIDHASH` 授权头,所以这一项不能少。

> **请用 Firefox 导出 cookie。** Chrome 新版开了 Device Bound Session Credentials,导出的会话绑在这台设备上,`*PSIDTS` 续不了,大概半小时到几小时就死。Firefox 没有这层绑定,同一套保活可以一直续。

### 账号池怎么工作

- 每个请求按**最久未用优先**挑一个 `enabled` 账号,挑中即推进轮转
- **自动续期**:上游几乎每个响应都会刷新 `SIDCC` / `__Secure-1PSIDCC` / `__Secure-3PSIDCC`,收下并写回账号;每 10 分钟另打一次 `RotateCookies` 换发 `__Secure-1PSIDTS`(约 30 分钟过期的那张票)
- **账号粘住自己的出口**:账号首次用到哪个出口就绑定下来,出口不可用了才换 —— 同一个账号从几十个不同 IP 发请求,在 Google 眼里是账号共享的典型特征
- **一个号挂了自动换下一个**,不让整个请求陪葬
- 失败判据只把 **401/403** 算作 cookie 的错:网络错误、代理失败、被 Google 拦(302)一律不计

> 「最近成功」只说明这个 cookie 参与的请求成功过,**不等于**它仍然有效 —— cookie 过期后 Gemini 不报错,只是把你当匿名用户。要确认有效性点面板里的「检测」按钮。

---

## 🛡️ 代理池

**为什么需要**:单 IP 突发打到一定次数就会被重定向到 `google.com/sorry/index`。实测这个次数是 **80–180**,跨度很大,由连接策略、出口质量和请求节奏共同决定。

| 出口 | 连接策略 | 并发 | 节奏 | 被拦时的成功次数 |
|------|----------|------|------|------------------|
| 住宅 | 复用连接池 | 10 | 无间隔 | 151 / 172 / 177 |
| 住宅 | 复用连接池 | 3 | 无间隔 | 103 / 111 |
| 住宅 | 每次新建连接 | 10 | 无间隔 | 106 / 109 |
| 住宅 | 每次新建连接 | 1 | 间隔 24s | 81 / 166 |
| 静态 | 复用连接池 | 10 | 无间隔 | 188 |
| 静态 | 复用连接池 | 1 | 10 次/分钟 | **800 次没被拦** |

**平缓节奏比什么都管用**:同一个静态 IP,突击打在 188 次被拦,改成 10 次/分钟连打 800 次、跨 110 分钟一次没被拦。所以默认 `per_ip_rph=80` 是很保守的下沿。

**被拦之后是硬拦,约两小时自动恢复。** 代理池的熔断冷却默认取 120 分钟就是照这个来的。

**怎么解决**:在面板「代理池」加多个代理,每个代理是一个独立 IP slot,享有独立的并发/RPM/RPH 配额。N 个代理 = N 倍总容量。

支持的协议:

```
http://user:pass@host:port
https://user:pass@host:port
socks5://user:pass@host:port
socks5h://user:pass@host:port     # 远程 DNS 解析,绕开本地 DNS 污染
```

调度规则:

- 配了代理后**不会再退回直连**(避免代理满了把主机 IP 也打爆)
- 失败 5 次自动熔断(面板可手动重置)
- 全部代理满 → 返回 HTTP 429(不消耗 Google 配额)
- **不读 `HTTPS_PROXY` / `ALL_PROXY` 环境变量** —— 代理只从代理池里取,否则宿主机上一个随手 export 的变量会悄悄改变出口 IP

---

## 🎭 指纹模拟

- **直连场景**(无代理)走 `bogdanfinn/tls-client`,TLS 握手 + HTTP/2 SETTINGS 帧 + ALPS 全部对齐真实 Chrome 146
- **走代理场景**换用 stdlib `net/http` + `http.ProxyURL`(兼容性最佳),但应用层 header(`Sec-CH-UA` / `Sec-Fetch-*` / `User-Agent`)仍按 Chrome 146 真实值伪装

> 注意走代理时 TLS 指纹是我们自己的,不是出口节点的 —— HTTP CONNECT 只建隧道,TLS 是我们跟 Google 端到端握的。但这不影响封禁阈值:对照实验里 tls-client 打到 111 次、Go stdlib 103 次,差 8 次,而同配置下出口之间的方差能到 36%。

**实测意外**:朴素 SDK 调用(如 Python `urllib`)触发风控时拿到 HTTP 429;伪装成 Chrome 后触发风控拿到 HTTP 302 跳转到 `google.com/sorry/index`。两者本质都是 IP 黑名单,但 302 证明 Google 真的把我们认成了浏览器。

---

## 🖥️ 管理面板

`http://localhost:8083/admin`,用 `--admin-token` 登录。

| 页面 | 内容 |
|------|------|
| **概览** | 24h KPI + 请求量/P50 延迟双轴趋势图 + 模型/代理分组统计 + IP 限流用量 + 一键连通性诊断 |
| **请求记录** | 明细列表(仅元数据,无 prompt/response 内容),状态/模型筛选 + 分页 |
| **代理池** | 运行时增删改 + 启用/禁用 + 失败次数熔断(每代理是独立 IP slot) |
| **账号池** | 导入多个 Google 登录态账号 + 一键「检测」是否仍是登录态 + 自动换发保活。列表只显示脱敏摘要 |
| **设置** | 运行时配置表单(保存即生效)+ API Key 轮换 + 部署期配置只读展示 |

面板前端是单个 HTML,Chart.js 随二进制 embed,**不走 CDN** —— 内网/离线部署也能开。

反代到子路径不用额外配置:面板里的地址全是相对的。把 `https://example.com/gemini/` 转发到本服务的 `/`,就能用 `https://example.com/gemini/admin` 打开。

---

## ⚙️ 配置

配置只有两个地方,按「改了要不要重启」分。

### 运行时配置 → 面板「设置」页(保存立刻生效)

存在数据库里,**优先级高于 `config.json` 和命令行参数**。

| 项 | 说明 |
|----|------|
| 默认模型 | 客户端没传 `model` 时用哪个 |
| 每 slot 并发 / RPM / RPH | 限流额度,0 = 不限 |
| Prompt 字节上限 | 默认 128000。超了:挂 cookie 时转成文本附件发,没挂时返回 400 `context_length_exceeded`。都不静默截断 |
| 多轮(multi_turn) | 默认关。开启后走原生 `conversation_id` 服务端续接,长会话不再撞单请求字节墙 |
| 自动删会话(auto_delete_conversation) | 默认关。出完结果自动删掉 gemini.google.com 上留下的这条会话 |
| 临时对话(temporary_chat) | 默认关。会话压根不进账号列表、不入训练,比「自动删会话」更干净 |
| 匿名优先(anon_first) | 默认关。不需要登录态能力的请求走匿名、不占用账号额度 |
| 重试次数 / 重试间隔 / 上游超时 | — |
| 明细保留天数 | 过期只删明细,聚合数据永久保留 |
| TLS 指纹 | `chrome_146`(默认)/ `chrome_144` / `chrome_133` / `firefox_147` / `safari_16_0` / `safari_ios_17_0` |
| Gemini `bl` 版本 | 上游前端版本号,过期时改这里 |
| 打印请求日志 | — |

所有值都在**后端**做范围校验(比如 `retry_attempts` 只接受 1-10、超时 5-600 秒),非法值会被拒绝并说明原因。

### 部署期配置 → 改了必须重启

| 项 | 位置 |
|----|------|
| 监听端口 | `ports` + `command: --port` |
| 数据库路径 | `volumes` + `command: --db` |
| `ADMIN_TOKEN` | environment,面板登录 token |
| `API_KEY` | environment,设了就锁死 API key |

`--proxy` 和 `--cookie-file` 是**播种参数**,不是第二套配置:启动时把值导入代理池 / 账号池(按 URL、cookie 内容去重),之后一律从面板管理。

### 命令行参数全集

| flag | 说明 |
|------|------|
| `--port` | 监听端口,默认 8083 |
| `--config` | 指定 `config.json` 路径 |
| `--db` | SQLite 路径,默认 `./data/gemini.db` |
| `--admin-token` | 面板登录 token,留空 = 面板不鉴权(只有绑 `127.0.0.1` 才可接受) |
| `--api-key` | 锁定 `/v1/*` 的 key,等价于 `API_KEY` 环境变量 |
| `--cookie-file` | 启动时把文件里的 cookie 导入账号池 |
| `--proxy` | 启动时把这个代理导入代理池 |
| `--impersonate` | TLS 指纹档位 |
| `--version` | 打印版本退出 |

**优先级**:面板改动 > CLI flag / `config.json` > 内置默认。

不传 `--config` 时会自动找当前目录的 `config.json`,其次 `$HOME/.config/novagemini/config.json`。可以把 `config.example.json` 复制成 `config.json` 当启动模板。

---

## 📡 OpenAI 协议覆盖

| 路径 | 状态 | 备注 |
|------|------|------|
| `POST /v1/chat/completions` | ✅ | 真流式:上游每出一帧就转发增量。带 `tools` 时退化为收完再发 |
| `POST /v1/responses` | ✅ | OpenAI Responses API(Codex CLI 用)。未做真流式 |
| `GET /v1/models` | ✅ | 匿名 2 个,挂了 cookie 才出第 3 个 |
| `GET /` | ✅ | 健康检查,不鉴权 |
| `/v1/videos` | ✅ | 异步视频:POST 建任务、GET 轮询、GET 下载 MP4 |
| `/mcp` | ✅ | MCP server(Streamable HTTP),暴露 `web_search` |
| Function calling | ⚠️ | **Prompt 级**实现(让模型输出 ```` ```tool_call``` ```` 块再解析),不是真协议层 |
| `tool_choice` | ⚠️ | `none` 完全不注入工具定义;`required` 会加强制措辞但无法真正强制 |
| Vision / 图片输入 | ⚠️ | 挂 cookie 时可用;匿名态返回 400 |
| `stream_options.include_usage` | ✅ | 在 `finish_reason` 之后补一个 usage chunk |
| usage token 数 | ✅ | tiktoken cl100k_base,与面板 requests 表同口径 |
| `n > 1` | ❌ | 返回 400。上游只给一个候选 |
| 采样参数 | ➖ | `temperature` / `top_p` / `max_tokens` / `stop` / `seed` 等收下即忽略 |
| `response_format` / `logprobs` | ➖ | 未实现,收下即忽略 |
| Audio(输入) | ❌ | 传 `input_audio` 返回 400 |
| `/v1/embeddings`、`/v1/images/*` | ❌ | 未实现,返回 404 |
| `/v1beta/models/...`(Gemini 原生) | ❌ | 未实现,只暴露 OpenAI 形状 |

**token 数**用 tiktoken cl100k_base 精确计算(中英文都准),不是 `len/4` 估算。Gemini 网页端不返回真 token 数,t偏差在 ±20% 以内。

---

## 📁 项目结构

```
main.go                    只有一句 app.Run();入口留在根目录
internal/app/              全部实现
  app.go                   flag 解析 + 路由注册 + 启动
  config.go                配置加载 + 默认值
  client.go                tls-client(chrome146)+ stdlib(走代理)双 client
  gemini.go                模型表 + 80 槽 payload + 模型 header + StreamGenerate + wrb.fr 解析
  xsrf.go                  带 cookie 时必需的 XSRF token:抓取 + 按 cookie 缓存 + 过期自愈
  messages.go              OpenAI messages → prompt,tool_call 解析
  server.go                /v1/* + 限流入口 + 参数校验 + metrics 写入
  sse.go                   SSE 写出器(懒发 header,失败仍能返回 502 JSON)
  ratelimit.go             每 IP slot 独立并发 / RPM / RPH 限流
  tokenizer.go             tiktoken cl100k_base 单例
  apikey.go                API key(启动参数锁定 / 面板可轮换 双轨)
  db.go                    schema:sessions / requests / accounts / kv
  dbdialect.go             SQLite / MySQL / PostgreSQL 方言
  proxy.go                 代理池 CRUD + 容量调度 + 熔断
  cookie_pool.go           账号池数据层(CRUD + 最久未用优先 + 健康度回写)
  rotate.go                会话保活(RotateCookies:1PSIDTS 哨兵 + SIDCC iframe)
  upload.go                附件上传(content-push 两步 resumable)
  context_file.go          超长对话转文本附件
  vision.go                图片输入:data: URL / http(s) 链接 → 待上传附件
  media.go                 媒体产物取回(图片 CDN 302 跟随 + 音乐/视频轮询)
  media_store.go           产物落盘
  videos.go                /v1/videos 异步视频任务
  mcp.go                   /mcp MCP server(web_search 工具)
  search.go                联网搜索来源解析
  bl.go                    上游前端版本号 bl 自动跟随
  scheduler.go             小时/天聚合 + 过期明细清理
  runtime.go               运行时配置快照(面板改完即时生效)
  admin.go                 /admin/api/* 鉴权 + REST
  admin_cookies.go         账号池的 admin REST(返回前脱敏)
  admin_ui.go              embed admin_ui/
  admin_ui/                管理面板前端(单页 + Chart.js,随二进制打包)
Dockerfile                 多阶段构建(alpine builder → distroless runtime)
docker-compose.yml         单容器,默认拉 ghcr 镜像,sqlite 挂 volume
.github/workflows/         docker.yml 推镜像 / release.yml 打 tag 发二进制
```

---

## 🚧 已知边界

- **单 IP 上限**:突发打实测 80–180 次后被重定向到 sorry 页。平缓打几乎打不满。要放大产能配代理池
- **视频 / 深度研究**:视频免费号被 Google 收走(需 Pro 号);深度研究是多步异步流程,未实现
- **Function calling**:prompt 级实现,模型不一定每次都按格式返回
- **长上下文有两堵墙**:请求体约 13 万字节、附件约 16 万字节。挂 cookie 只能把可用长度从 13 万提到约 16 万
- **账号池不自动摘除坏号**:失败会回写,但失败到一定次数不会自动禁用,需看面板手动停
- **Chrome 导出的 cookie 可能续不了**:见上文,用 Firefox 导出
- **假流式的那一半**:`/v1/responses` 和带 `tools` 的 chat 请求都是收完再发

---

## 🔧 故障排查

| 现象 | 多半是什么 | 怎么办 |
|------|-----------|--------|
| 我们返回 429 | 所有 IP slot 的并发/RPM/RPH 都占满了,**不是** Google 拒绝,没消耗上游配额 | 加代理,或在「设置」页调高限额 |
| 面板诊断显示 302 → `google.com/sorry/index` | 这个出口 IP 被 Google 拦了(80–180 次请求后) | 换出口/加代理。是硬拦不是概率性,原地重试没有意义 |
| 偶发空响应、面板记为上游拒绝 | 上游瞬时拒绝(1155),没有可预测阈值 | 重发一次通常就好。降 RPM 解决不了 |
| 请求全部超时 | 本机到 `gemini.google.com` 不通 | 配代理。注意**不读** `HTTPS_PROXY` 环境变量 |
| 启动即退出,报 `unable to open database file (14)` | 容器以 nonroot(uid 65532)运行,而 bind mount 的宿主目录属主是 root | 改用具名卷,或 `sudo chown -R 65532:65532 ./data` |
| 选 `gemini-3.1-pro` 直接报错 | 没配 cookie 时它不暴露,这是故意的 | 挂 cookie 后即可用 |
| 挂了 cookie 后请求全部 502 | cookie 已失效,取不到 XSRF token | 重新导出 cookie。判据:请求 `gemini-3.1-pro` 若回报 3.5 Flash-Lite 就是失效了 |
| cookie 大约半小时就失效,保活没用 | Chrome 设备绑定会话,`__Secure-1PSIDTS` 换发 401 | 用 Firefox 重新登录再导出 |
| 面板打不开 / 401 | `--admin-token`(或 `ADMIN_TOKEN`)没对上 | token 留空则不鉴权,只有绑 `127.0.0.1` 时才可接受 |
| 生图偶尔返回 502 `artifact retrieval failed` | 生成耗时很长时,CDN 会拒绝刚出结果的那条下载链 | 已内置一次重试;仍失败就重发。生成越快越稳 |
| Docker 默认 bridge 网络下上游可能返回空内容 | Google 拒绝某些 NAT 段 | 试 `network_mode: host` 验证是不是这个原因 |

---

## ❓ 常见问题

**这和 Google 官方 Gemini API 有什么区别?需要 API Key 吗?**
不需要。本项目不封装 `generativelanguage.googleapis.com`,而是直接复刻 `gemini.google.com` 网页端协议,所以没有 API Key、没有付费配额这回事。代价是能力边界跟着网页端走。

**会被封号吗?封禁是按账号还是按 IP?**
按**出口 IP + TLS 指纹**,不按账号。实测同一出口突发打 80~180 次会被重定向到 sorry 页,约两小时后自动恢复;平缓地打(例如每分钟 10 次)连打 800 次没被拦。付费(Google AI Pro)不会提高这个阈值。

**免费账号和 Pro / 付费账号差在哪?**

| | 匿名 | 免费号 cookie | 付费 / Ultra 号 |
|---|---|---|---|
| 文本模型 | 3.6 Flash、3.5 Flash-Lite | +3.1 Pro、扩展思考版 | +3.8 Flash、4 Argon |
| 读图 / 读视频 | ❌ | ✅ | ✅ |
| 生图 / 图生图 / 音乐 / 画布 | ❌ | ✅ | ✅ |
| 生视频 | ❌ | 通常不行 | ✅ |

**为什么每发一条消息,`gemini.google.com` 里就多出一个会话?**
默认每个请求是独立会话。三种办法:勾**临时对话**(会话压根不进列表,推荐)、勾**自动删会话**(出完结果再删)、(两者都只对登录号有意义)。已经堆了一堆的话,只能在网页端手动清。

**能当 Cursor / Dify 这类 agent 工具的后端吗?**
能接(OpenAI 兼容),纯聊天和写代码没问题。但网页端单次请求约 13 万字节封顶(挂 cookie 约 16 万),agent 工具一次塞几万字系统提示和历史时容易答非所问 —— 这是网页端的先天限制。Function calling 是 prompt 级实现,模型不一定每次都按格式返回。

**生图用的是哪个模型?能出 2K 吗?**
`gemini-image` 走网页端的生图工具位,不锁模型:网页端当前用什么后端(如 Nano Banana),这里就自动跟着。分辨率被网页端锁在约 100 万像素,没有 2K。

**为什么在 Dify / 反向代理后面流式变成了一次性输出?**
普通对话是真流式;带 `tools` 的请求和 `/v1/responses` 是收完再发。响应已带 `X-Accel-Buffering: no`,如果经过 Nginx 等反代仍被缓冲,检查反代自己的缓冲设置。

**支持 MySQL / PostgreSQL 吗?**
支持。设 `SQL_DSN` 即可,不设默认 SQLite;建表自动完成,三种库共用一套 schema。

---

## 🔒 隐私

- Prompt 和 response 内容**永不入库** —— 只存元数据:模型、代理 ID、延迟、token 数、状态码、错误类型
- 历史 `prompt_preview` / `response_preview` 列从老版本迁移时自动 DROP
- Token 数用 tiktoken-go cl100k_base BPE 分词器精确计算,不是 `len/4` 估算

---

## 📄 License

MIT — 详见 [LICENSE](LICENSE)。

## 🙏 致谢

- [bogdanfinn/tls-client](https://github.com/bogdanfinn/tls-client) — Chrome 真指纹 TLS 库
- [pkoukk/tiktoken-go](https://github.com/pkoukk/tiktoken-go) — BPE tokenizer
- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) — 纯 Go SQLite(CGO-free)

# tools/ — 辅助脚本

这些脚本**不属于二进制**，是维护账号池用的外部工具。二进制本身（`novagemini.exe`）只负责转发请求；cookie 的获取、验证、同步都发生在浏览器侧，由这里的脚本完成。

---

## 每个脚本干什么

| 脚本 | 用途 | 什么时候用 |
|---|---|---|
| `cookie_grab.py` | 从本地浏览器读取 cookie、验证、导入/刷新账号 | 手工导入账号、批量导入、查看某个 profile 是否登录 |
| `browser_harvest.py` | 用 geckodriver 打开 Firefox 配置文件的**副本**，读取完整 cookie，验证后写入账号 | Firefox 刚登录、或某个账号的真实请求已经失败时 |
| `chrome_harvest.py` | 同上，但针对 Chrome | Chrome 里登录的账号；`--live` 用于 app-bound cookie |
| `session_keeper.py` | 按 `session-map.json` 逐个 profile 同步 cookie | 需要批量刷新时；默认**不**打开浏览器 |
| `verify_accounts.py` | 用**库里现存**的 cookie 逐个测 `/app` | 怀疑某个账号已失效时（注意配额） |

### 常用命令

```bash
# 看看某个 Firefox 配置是否还登录着
python tools\cookie_grab.py check --profile "firefox:<文件夹名>"

# Firefox：取一份 cookie，验证后写入指定账号（并记录 profile→账号 映射）
python tools\browser_harvest.py --profile "sttbandung105v3" --import --account 13 --api-key sk-gemini-…

# Chrome：必须 Chrome 完全退出（三横线 → 退出），否则只能读到 6 个残缺 cookie
python tools\chrome_harvest.py --live --profiles Default --import --account 15

# 逐个核验（每个账号一次请求）
python tools\verify_accounts.py
python tools\verify_accounts.py --ids 12,17
```

---

## 什么时候**不要**用

这一节比上一节重要。以下规则都是实测踩出来的。

**1. 不要把 harvest 当定时任务。** 同一个会话被三个客户端轮流使用时，Google 会当作会话被盗并撤销：

```
浏览器（创建会话） → headless 浏览器（harvest 读取） → novagemini（发请求）
```

实测对照：`novaestellar` 从不被 harvest / 反复验证，稳定运行数天；而被反复 harvest 的账号在数小时内陆续失效。**刚登录后 harvest 一次，然后就停手。**

**2. 不要频繁 verify。** 本机 IP 的配额是 5 并发 / 30 RPM / 80 RPH，而且**所有账号共享这一份配额**。一次"逐个账号测试"就是 11 次请求，几次下来配额耗尽，健康账号也会返回 429/502 —— 看起来像账号批量死亡，其实是自己把配额打光。验证请间隔 20 秒以上，或者干脆等真实请求失败再查。

**3. 不要把一个账号同时交给浏览器和服务器。**

```
rotate_disabled=0  →  服务器持有并轮换（像 novaestellar，最稳定）
rotate_disabled=1  →  浏览器持有，服务器只借用；此时浏览器不要再用该账号
```

harvest 写入的是 `rotate_disabled=1`（浏览器优先）。如果决定交给服务器，就在面板里把该账号改成 `rotate_disabled=0`，并停止在那个 profile 里使用 Gemini。

**4. 不要在 Firefox 运行时判断账号状态。** 配置文件的 SQLite 会被浏览器独占，副本可能读不到最新 cookie；`verify_accounts.py` 读的是库里的副本，也可能与浏览器当前会话不同步。

---

## 配置文件

| 文件 | 内容 |
|---|---|
| `firefox-profiles.json` | 友好名 → Firefox 配置文件夹（如 `sttbandung105v3` → `sttbandung105v3`） |
| `session-map.json` | Firefox 配置文件夹 → 账号 id 列表，`session_keeper.py` 用它决定写到哪里 |
| `refresh-cookies.cmd` | 旧的刷新包装脚本（对应的计划任务已删除，仅供参考） |
| `session-keeper.cmd` | `session_keeper.py` 的包装脚本（计划任务 `novagemini-session-keeper`，**当前处于禁用状态**） |
| `novagemini-startup.vbs` | 开机启动 novagemini（放在 Startup 文件夹里的那份才是生效的） |

---

### Chrome 的两条硬限制（实测）

```
1. Chrome 的默认 User Data 目录不能被自动化
   chromedriver → "Chrome failed to start: crashed
                   (DevToolsActivePort file doesn't exist)"
   → 用 --live 读默认目录：不可能。move/复制也不行（app-bound cookie 解不开，只能读到 6 个残片）。

2. 因此 Chrome 里登录的账号只能这样取：
   a) 改在 Firefox 里登录（已验证可行），或
   b) 给这个账号单独建一个 Chrome User Data 目录，把账号登在那里，
      然后：
      python tools\chrome_harvest.py --live --user-data-dir "<folder-itu>" \
             --profile-directory Default --import --account <id>
```

判断登录落在哪个浏览器，看 cookie 数据库的修改时间：Firefox 是
`Profiles\<名字>\cookies.sqlite(-wal)`，Chrome 是
`User Data\<名字>\Network\Cookies`。

## 防止机密再次进入仓库

`secret_guard.py` 是提交前的闸门，会拦下：novagemini/客户端/9router 的密钥、
GitHub/Notion token、Google 会话 cookie 值，以及 `session-map.json`、
`firefox-profiles.json` 这两个含账号名称的本地状态文件。

```bash
python tools\secret_guard.py --install   # 装成 .git/hooks/pre-commit（每个克隆装一次）
python tools\secret_guard.py             # 手动检查当前已 stage 的文件
```

规则很简单：**密钥只存在于两处** —— `service.json` 的 `env`（给进程注入），
以及面板（脚本运行时通过 loopback 读取）。仓库里任何地方都不写第二份。

历史里曾经出现过的那份密钥已从提交历史中清除（git filter-repo + force push），
但 GitHub 会保留一段时间内不可达的旧对象 —— 知道旧 SHA 的人仍可能读到。
彻底消除只有两条路：把仓库设为私有，或向 GitHub 申请清除不可达对象。

## 身份如何确认

`browser_harvest.py` / `chrome_harvest.py` 在写入前会做两件事：

1. 用 cookie 请求 `gemini.google.com/app`，**不跟随重定向**（跟随会把登录页当成已登录）；
2. 从页面里读出**登录账号的邮箱**，与目标账号标签比对。

只有两者都通过才写入。**不通过就不写** —— 宁可留下一个明确的"需要重新登录"，也不要写进一份已经失效的会话。

写入时同时设置：

```
impersonate      = firefox_147 / chrome_146   （跟随 cookie 的来源浏览器）
auth_user        = 0                          （每个配置一个账号，就是索引 0）
rotate_disabled  = 1                          （浏览器持有）
```

`auth_user=0` 不能省：早期一个配置承载多个账号时，cookie 里带有 `__NOVA_AU=<n>` 标记，服务端会去请求 `/u/<n>/app`，而新会话是索引 0 —— 结果是 302，看起来像账号全死。

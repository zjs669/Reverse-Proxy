# Reverse-Proxy

把 **CodeBuddy / Trae / Qoder / 豆包工作 / DeepSeek** 的网页端账号，聚合成一个 **OpenAI 兼容** 的本地 API 网关。

> 名字里的 “Reverse-Proxy” 指的就是这件事：把各家**网页端**会话反向代理成标准 OpenAI 接口。

一句话：客户端只改 `base_url` + 一把密钥，背后由网关轮换你的多个账号、翻译各家协议、自动续期 token。

```bash
curl -sN http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer <你的密钥>" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4.1-flash:high","stream":true,
       "messages":[{"role":"user","content":"你好"}]}'
```

> ⚠️ **仅供个人学习自用**。请遵守各家服务条款，账号风险自负。
> 本仓库**不含**任何真实凭证：`auths/`、`data/`、`config.json` 一律在 `.gitignore` 中。

---

## 与上游项目的关系（来源说明）

本仓库是 **[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)** 的**魔改分支**，而后者又是 **[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)** 的增强分支（上游仓库已删除）。

```text
Sliverkiss/workbuddy2api          （原始项目，上游已删库）
        └── linguo2625469/workbuddy2api-panel   （增强分支：Web 面板 + 成长任务 + 搜索）
                └── zjs669/Reverse-Proxy        （本仓库：多供应商改造）
```

**血缘与许可**：上游为 MIT 协议。本仓库保留其全部原有能力（CodeBuddy 账号池调度、错误分类、熔断冷却、提示词体系、定时任务），并在此之上做多供应商扩展。原项目作者的版权与 MIT 许可声明继续有效。

**本仓库相对 panel 版的主要改动**（都是「新增」，未改动原有 CodeBuddy 链路的行为）：

| 改动 | 说明 |
|---|---|
| **Provider 抽象层** | 把硬编码在 `Client` 里的 CodeBuddy 协议抽成 `Provider` 接口（`ChatStream`/`StreamTo`/`Aggregate`/`Classify`/`FetchModels`/`RefreshToken`），按 `auth.Provider` 分派，未识别时回落 CodeBuddy |
| **新增 Trae 供应商** | `chat_v3` 协议 + `content` parts 数组 + `X-Cloudide-Session` 头 |
| **新增 Qoder 供应商** | COSY 私有协议 + 换表编码 + 嵌套 SSE，强制 HTTP/1.1（上游对 h2 的 SSE 返回 `INTERNAL_ERROR`） |
| **新增 豆包 供应商** | 网页版 Cookie 鉴权 + `/chat/completion` SSE（**不支持 Function Calling**） |
| **新增 DeepSeek 供应商** | 网页版 Bearer userToken + **每请求 PoW 求解**（内嵌官方 wasm + wazero）+ 上游会话树上下文（`chat_session_id` + `parent_message_id`） |
| **四层鉴权** | 主密钥 / 区域密钥 / 供应商密钥 / 账号级密钥（一把 key 锁死一个号） |
| **会话粘性** | `conversation_id → 账号` 绑定，TTL 滚动续期，可镜像 Redis |
| **DeepSeek 会话状态** | `会话键 → {账号, chat_session_id, parent_message_id}` 持久化，换号/过期自动重建 |

**与 panel 版的功能取舍**：本仓库**没有**移植 panel 的 Web 管理面板与「成长任务一键完成」（`/panel` 路由、任务中心自动化）。若你需要可视化运维面板，请直接用 panel 版；本仓库专注「多供应商协议翻译 + 账号池调度」这一层，通过 `curl` / 任意 OpenAI SDK 使用。

> 详细的多供应商协议差异、踩坑记录与接入规范见 **[PROVIDERS.md](PROVIDERS.md)**。

---

## 使用声明

与上游一致：**本项目仅限个人自用**（管理自己的账号、给本地工具提供一个 OpenAI 兼容入口）。

- ❌ 禁止批量注册小号、对外售卖 API / 账号池 / 卡密分发；
- ❌ 禁止二次加壳、加密、捆绑收费后分发；
- ✅ 欢迎自用、反馈问题、提交 PR。

使用非官方客户端可能触发上游风控或封号，风险由使用者自行承担。本项目按「原样」提供，不提供任何担保。

---

## 目录

- [与上游项目的关系（来源说明）](#与上游项目的关系来源说明)
- [使用声明](#使用声明)
- [特性](#特性)
- [快速开始](#快速开始)
- [配置详解](#配置详解)
- [登录 / 续期](#登录--续期)
- [模型清单](#模型清单)
- [调用示例](#调用示例)
- [HTTP 端点](#http-端点)
- [账号池与路由](#账号池与路由)
- [token 自动续期](#token-自动续期)
- [常见问题](#常见问题)
- [安全](#安全)
- [License](#license)

---

## 特性

| 能力 | 说明 |
|---|---|
| OpenAI 协议兼容 | `POST /v1/chat/completions`（流式/非流式）、`GET /v1/models` |
| 多供应商 | CodeBuddy（国内 cn / 国际 intl）、Trae、Qoder、豆包工作、**DeepSeek 网页版** |
| 多账号池 | 加权选号、在途并发限制、失败换号、软冷却 + 熔断 |
| 四层鉴权 | 主密钥 / 区域密钥 / 供应商密钥 / **账号级密钥**（一把 key 锁死一个号） |
| 模型目录动态拉取 | `/v1/models` 直接来自上游，新增模型自动出现（豆包从网页目录解析） |
| token 自动续期 | 按「剩余寿命」提前换新，短寿命凭证（Trae 8h）也不会断 |
| 工具调用 | CodeBuddy / Trae / Qoder 支持 `tools` / `tool_calls`；豆包 / DeepSeek 不支持（见 FAQ） |
| 会话粘性 | 同一会话尽量复用同一账号，降低上下文丢失 |
| 定时任务 | 签到、活跃上报、token 保活（仅 CodeBuddy 体系有意义） |

---

## 快速开始

### 0. 前提

- Docker + Docker Compose
- 至少一个可用的上游账号（下面「登录」一节）

### 1. 准备配置

```bash
cp config.example.json config.json   # 按需修改，见「配置详解」
mkdir -p auths data
```

### 2. 构建并启动

```bash
docker compose up -d --build
docker logs reverse-proxy --tail 20
```

看到这行即成功：

```
Reverse-Proxy listening on :7863 (api_key=false, account_keys=0, provider_keys=0)
```

### 3. 登录上游账号

```bash
# CodeBuddy：自动 OAuth（打印 URL → 浏览器登录 → 回车轮询拿 token）
docker exec -it reverse-proxy /app/login codebuddy cn 我的主号

# Trae / Qoder / 豆包工作：按提示粘贴凭证 JSON（Windows 用文件重定向，见下）
docker exec -i reverse-proxy /app/login trae 我的trae < trae.json
docker exec -i reverse-proxy /app/login doubao 我的豆包 < doubao.json
```

登录成功后 `auths/auth.json` 会多一条账号，并**自动往 `config.json` 的 `account_keys` 写一把随机密钥**（已存在则沿用不覆盖），控制台会把它打印出来：

```
✅ 配置成功：键 doubao_1（provider=doubao nickname=我的豆包 uid=doubao-work-1）
🔑 已自动生成 API 密钥并写入 account_keys（doubao_1 / doubao-work-1）：
   3f9a…（48 位）
```

### 4. 重启加载

```bash
docker restart reverse-proxy    # 或用 compose up -d --force-recreate
```

### 5. 验证

```bash
curl -s http://127.0.0.1:7863/healthz
# {"healthy":2,"service":"Reverse-Proxy","total":3}

curl -s http://127.0.0.1:7863/v1/models -H "Authorization: Bearer <密钥>"
```

---

## 配置详解

`config.json` 顶层字段（完整示例见 `config.example.json`）：

```jsonc
{
  "listen": ":7863",
  "auth_file": "./auths/auth.json",   // 上游凭证（login 写入）
  "state_file": "./data/state.json",  // 账号池运行态（冷却/熔断/禁用）

  "account_keys": {                    // ★ 对外 API 密钥，见下节
    "codebuddy_1": { "<uid>": "<secret>" }
  },

  "models": {                          // 排除清单（非白名单），空 = 全放行
    "codebuddy": [], "intl": [], "trae": [], "qoder": [], "doubao": []
  },

  "schedule": {
    "checkin_hours": [9, 21],          // 签到（CodeBuddy 体系）
    "travel_hours":  [9, 21],
    "activity_hours":[10],
    "keepalive_hours":[22],
    "token_watch_enabled": true,       // ★ token 自动续期看护
    "token_watch_interval": "10m",     //   巡检间隔
    "refresh_window": "30m",           //   通用提前续期窗口
    "refresh_window_by_provider": { "trae": "2h" }
  },

  "pool":            { "max_in_flight": 3, "breaker_threshold": 3,
                       "breaker_cooldown": "30m", "breaker_cooldown_max": "6h" },
  "session_sticky":  { "enabled": true, "ttl": "30m", "gc_interval": "5m" },
  "cooldown":        { "soft_rate": "600s", "soft_rate_max": "2h", "disabled": false },
  "upstream":        { "timeout_seconds": 120 },
  "server":          { "max_body_mb": 8 }
}
```

### 四层鉴权：四类密钥怎么选

只要 `config.json` 里**任意一种**密钥存在，请求就必须带合法 `Authorization: Bearer <key>`。

| 类型 | 配置位置 | 作用范围 | 适用场景 |
|---|---|---|---|
| 主密钥 | `"api_key": "<secret>"` | 全部账号、不限区域 | 自己的 IDE/客户端，简单粗暴 |
| 区域密钥 | `"api_keys": {"<secret>": "cn"}` | 指定区域（`cn` / `intl`） | 国内/国际号分开用 |
| 供应商密钥 | `"provider_keys": {"<secret>": "trae"}` | 该供应商的所有号 | 「这把 key 只走 Trae」 |
| **账号密钥** | `"account_keys": {"标签": {"<uid>": "<secret>"}}` | **锁死单个账号**，不轮换 | 一个客户端配一个号 |

账号密钥的格式要点：

```jsonc
"account_keys": {
  "codebuddy_1": { "<真实uid>": "<你自己定的密钥>" }   // 内层 key 必须是账号真实 uid
}
```

- 外层 `codebuddy_1` 只是标签，随便取但要唯一；**内层 uid 必须真实**，否则路由不到。
- 密钥不能为空，且不能与其他任何密钥重复（加载时校验，重复直接报错）。
- 只配了账号密钥时，`/v1/models` 也只会返回**该账号所属供应商**的模型。

### 定时任务与自动续期

- `checkin / travel / activity` 只对 **CodeBuddy** 账号有意义（其他家没有对应体系，自动跳过）。
- `token_watch_*` 对所有能自助续期的账号生效，与钟点无关，详见 [token 自动续期](#token-自动续期)。

---

## 登录 / 续期

### 命令形态（别把区域参数写错）

| 供应商 | 命令 | 备注 |
|---|---|---|
| CodeBuddy 国内 | `login codebuddy cn <昵称>` | 有区域参数，键名 `codebuddy_1` |
| CodeBuddy 国际 | `login codebuddy intl <昵称>` | 有区域参数，键名 `codebuddyai_1` |
| Trae | `login trae <昵称>` | **无**区域参数 |
| Qoder | `login qoder <昵称>` | **无**区域参数 |
| 豆包工作 | `login doubao <昵称>` | **无**区域参数，单账号固定槽位 `doubao_1` |
| DeepSeek | `login deepseek <昵称>` | **无**区域参数，单账号固定槽位 `deepseek_1` |

> `<昵称>` 只是显示名（进日志和 `auth.json`），随便取。
> Trae/Qoder/豆包多写一个 `cn` 会被当成昵称，不会报错但键名会变。

容器内执行（推荐）：

```bash
docker exec -i reverse-proxy /app/login doubao 我的豆包 < cred.json
```

### 各家凭证怎么取

| 供应商 | 要什么 | 去哪拿 | 有效期 |
|---|---|---|---|
| CodeBuddy | OAuth（自动） | 命令打印 URL → 浏览器登录 → 自动轮询 | 较长 |
| Trae | `Cloud-IDE-JWT` | trae.cn 登录 → F12 → Network → 请求头 `Authorization: Cloud-IDE-JWT <一串>` | **8 小时** |
| Qoder | `dt-…` / `drt-…` token | qoder 登录后 F12 抓请求头，或用 `scripts/qoder_bootstrap.py` 引导 | 30 天 |
| 豆包工作 | 账号 Cookie（`sessionid` / `ttwid` / `passport_csrf_token`） | 豆包工作客户端登录 → F12 → Network → 任一 chat 请求 → 请求头 `Cookie:` 整段 | ~30 天 |
| DeepSeek | `userToken`（`mb5+…`） | chat.deepseek.com 登录 → F12 → Application → Local Storage → `userToken` 的 `value` 字段（或 Network 里 `Authorization: Bearer` 后那串） | 长期，失效需重登 |

凭证文件格式（`cred.json`）—— **每家最少只需这些字段**（省略的字段自动补全：uid 兜底随机/缺省值、机器指纹自动生成、expiresAt 自动解析）：

```jsonc
// Trae —— 必填 accessToken + uid
{ "accessToken": "<Cloud-IDE-JWT 原文>", "uid": "<X-Uid>" }

// Qoder —— 最简：只给 PAT（login 自动兑换 jobToken + 生成机器指纹 + 兜底 uid）
{ "pat": "pt-…" }
// 或已有设备流令牌（uid 也可省）：
{ "accessToken": "dt-…", "refreshToken": "drt-…" }

// 豆包 —— 最少只给 sessionid（ttwid / passport_csrf_token 可选增强）
{ "sessionid": "…" }

// DeepSeek —— 最少只给 accessToken（device_id / Cookie 可选）
{ "accessToken": "mb5+…" }
// 也可直接粘 localStorage 原文，脚本会自动取 value：
{ "userToken": { "value": "mb5+…", "__version": "0" } }
```

### Windows 上粘不进去 / 卡住？

`login` 用 `io.ReadAll(os.Stdin)` 读到 **EOF** 才返回，而 Windows 的 cmd 里 `Ctrl+D` 传不进容器 PTY，所以 `-it` 交互会一直挂着。两个解法：

```bash
# 方案 A（推荐）：文件重定向，`-i` 不要 `-t`，读完自动 EOF
docker exec -i reverse-proxy /app/login doubao 我的豆包 < cred.json
# cmd 里也可以用管道
type cred.json | docker exec -i reverse-proxy /app/login doubao 我的豆包
```

```bash
# 方案 B：已经在卡住的窗口里 → 回车换行 → 输入 END 独占一行 → Ctrl+D（无效则 Ctrl+Z 回车）
```

### 续期

- **能自助续期**：CodeBuddy、Trae、Qoder → 交给 token 看护自动完成，基本无需管。
  - 例外：Trae 的续期接口**只接受未过期的 JWT**，一旦过期就只能重新登录（见 FAQ）。
- **不能自助续期**：豆包 Cookie → 约每 30 天重跑一次 `login doubao`；DeepSeek `userToken` → 上游**没有 refresh 接口**（收到 `40003` 只能重登），网关做存活校验并禁用失效号，日志会提示重跑 `login deepseek`。

---

## 模型清单

`/v1/models` 的模型来自上游**动态拉取**，会随账号权益变化，下面是常见集合。

### CodeBuddy（约 80 个，示例）

```
default-model, fast-model, balanced-model, primary-model
deepseek-v4.1-flash:low | :high | :max
gpt-6-astra:low | :medium | :high | :xhigh | :max
gpt-6.1-sol / gpt-6-sol / gpt-6-luna / gpt-5.6-sol / gpt-5.6-terra …
hy4-preview:high, hy3:low, hy3:high
```

### Trae（约 33 个，示例）

```
auto:low|medium|xhigh        fast:low|medium|xhigh
qwen3.8-max / qwen3.8-flash / deepseek-v4-pro / deepseek-flash
glm-5.3 / glm-5.3-flash / kimi-k3.1-pro …
```

### Qoder

```
auto:low | auto:medium | auto:xhigh | fast:low …
```

### 豆包工作（5 档，从网页目录解析）

| 对外模型名 | 官方档位 | 上下文 |
|---|---|---|
| `doubao-fast` | 快速 | 131k |
| `doubao-expert` | 专家 | 131k |
| `doubao-auto` | 工作任务 Auto | 256k |
| `doubao-2.1-lite` | 豆包 2.1 Lite | 256k |
| `doubao-2.1-pro` | 工作任务 Pro | 256k |

> `:low / :medium / :high / :xhigh` 是**推理强度**后缀，会映射成上游的思考深度。
> 不带后缀通常落到默认档。豆包的推理强度由请求体的 `reasoning_effort` 控制。

---

## 调用示例

### curl（流式）

```bash
curl -sN http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer $REVERSE_PROXY_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-v4.1-flash:high",
    "stream": true,
    "messages": [{"role": "user", "content": "用一句话介绍你自己"}]
  }'
```

### Python（OpenAI SDK）

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:7863/v1", api_key="<你的密钥>")
resp = client.chat.completions.create(
    model="deepseek-v4.1-flash:high",
    messages=[{"role": "user", "content": "你好"}],
    stream=True,
)
for chunk in resp:
    print(chunk.choices[0].delta.content or "", end="")
```

### 工具调用（Agent / IDE 后端）

```jsonc
{
  "model": "deepseek-v4.1-flash:high",
  "messages": [{"role": "user", "content": "看一下当前目录有哪些文件"}],
  "tools": [{
    "type": "function",
    "function": {
      "name": "run_shell",
      "parameters": {"type": "object", "properties": {"cmd": {"type": "string"}}}
    }
  }]
}
```

Only CodeBuddy / Trae / Qoder 会把 `tool_calls` 原样回给你；豆包只会返回纯文本。

---

## HTTP 端点

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| POST | `/v1/chat/completions` | ✅ | 对话（`stream` 支持 SSE） |
| GET | `/v1/models` | ✅ | 模型列表（按密钥所属账号/供应商过滤） |
| GET | `/status` | ✅ | 账号池状态（总数/健康/冷却/禁用/在途） |
| GET | `/usage` | ✅ | 各账号用量与状态明细 |
| GET | `/healthz` | ❌ | 探活；无可用账号时返回 503 |

---

## 账号池与路由

```
客户端 ──Bearer key──▶ 网关 ──选号──▶ 上游账号 ──协议翻译──▶ CodeBuddy/Trae/Qoder/豆包
                        │
                        ├─ 账号级密钥：锁死单号，不轮换
                        ├─ 供应商/区域密钥：在该范围内轮换
                        └─ 主密钥：全池轮换
```

- **选号**：按「余额/成功率/闲置补偿」加权随机，跳过冷却中、熔断中、在途满额的账号。
- **换号**：401/限流/5xx 会换下一个号重试；显式 `session dead` 连续 3 次才禁用（避免误杀）。
- **软冷却**：限流触发指数退避（`600s` → 封顶 `2h`），到期自动恢复。
- **会话粘性**：同一会话在 TTL 内尽量复用同一账号（`session_sticky`）。

---

## token 自动续期

短寿命凭证（Trae 只有 8h）靠「每天刷一次」必然死。看护逻辑（`internal/scheduler/token.go`）：

```
每 token_watch_interval（默认 10m）巡检全量账号
  ├─ 剩余寿命 > 窗口 → 跳过
  ├─ 若账号被凭证类原因禁用、且令牌仍有效 → 直接解禁
  ├─ 调用该供应商的 RefreshToken 换新
  │    ├ 成功 → 落盘 + 清 session 计数 + 若被禁用则复活
  │    └ 失败 → 退避 20m；已过期的令牌按指纹记死，不再重试
  └─ 已过期且自救失败 → 打一条 WARN（含可直接复制的重新登录命令）
```

窗口取值原则：**必须显著小于凭证寿命**。默认 `30m`，`trae` 覆盖为 `2h`（寿命 8h）。

配置：

```json
"schedule": {
  "token_watch_enabled": true,
  "token_watch_interval": "10m",
  "refresh_window": "30m",
  "refresh_window_by_provider": { "trae": "2h" }
}
```

日志关键字：

```
token-watch <uid>: renewed provider=trae left_before=1h58m left_after=8h0m revived=false
token-watch <uid>: 凭证仍在有效期（7h59m 后过期），已自动解除凭证类禁用（provider=qoder）
WARN: token-watch <uid>: trae token 已过期 12h34m，无法自助续期 → 需重新登录
```

---

## 常见问题

**Q：调用返回 `401 invalid_api_key`？**
`config.json` 里存在任一密钥后就要求 Bearer。检查：密钥是否与 `account_keys` 顶层一致、是否多了空格、改完是否 `docker restart`。

**Q：返回 `503` / 模型列表为空？**
该供应商没有可用账号，或 token 过期。看启动日志里的凭证体检：

```
WARN: [auth] trae/我的trae 凭证已于 2026-11-01 20:38 过期 … 需人工重新登录
```

**Q：Trae 明明刚登录，过一晚又 401？**
它的 JWT 只有 **8h**，且续期接口 `GetUserToken` **只接受未过期**的 token。确认看护在跑（日志有 `token 自动续期已启用`），若已错过窗口只能重新登录一次，之后由看护接管。

**Q：豆包说不了话 / 不能执行命令？**
豆包网页版接口**不支持 Function Calling**，请求里的 `tools` 会被忽略，只能纯文本问答。要跑 shell 请用 CodeBuddy / Trae / Qoder 的密钥。

**Q：豆包换模型名，回答没区别？**
上游按账号默认档位走，模型名目前只是标签（已知限制）。但**推理强度**（`reasoning_effort`）是真生效的。

**Q：DeepSeek 说不了话 / 不能执行命令？**
DeepSeek 网页版同样**不支持 Function Calling**（请求里的 `tools` 会被忽略）。另外它每次请求都要解一次 PoW（约 40ms），且上游会话 3 天过期——过期后网关自动重建会话（对客户端透明，但会丢上下文）。

**Q：DeepSeek 报 `40003` / 日志说 `session invalid`？**
`userToken` 失效。DeepSeek 网页版**没有 refresh 接口**，只能重新登录一次：

```bash
# 浏览器登录 chat.deepseek.com → F12 → Application → Local Storage → userToken 的 value
echo '{"accessToken":"mb5+…"}' | docker exec -i reverse-proxy /app/login deepseek 我的DS
docker restart reverse-proxy
```

**Q：DeepSeek 多轮对话记不住上文？**
网关按请求体里的 `metadata.conversation_id` 做会话绑定。客户端**每轮都要带同一个 `conversation_id`**；不带就会走「每轮新建会话 + 全量重放历史」模式（也能用，但更慢更费 token）。

**Q：qoder 突然不能对话了（流式挂死 / 180s 后 502）？**
通常是 `dt-` 过期且未被自动续期。当前版本会在日志里打 `凭证已自动续期` 并自动重试；若看到 `凭证失效且无 drt-/pat，无法自动续期`，重跑 `login qoder`（建议走 PAT 路线，长期有效）。详见 [PROVIDERS.md §2.3.1](PROVIDERS.md)。
**Q：改了 `config.json` 要重新 build 吗？**
不用。它是挂载文件，`docker restart reverse-proxy` 即可；只有改 Go 代码才 `docker compose up -d --build`。

**Q：Windows 下 `./login` 卡住不动？**
见 [Windows 上粘不进去](#windows-上粘不进去--卡住)。

**Q：容器连不上国际版 `codebuddy.ai`？**
宿主机代理的 fake-ip 会把它解析成 `198.18.x.x`。`docker-compose.yml` 已用 `extra_hosts` 固定真实 IP；IP 失效时按注释里的 DoH 命令重新解析替换。

---

## 安全

- `config.json`（含密钥）、`auths/`（含上游 token）、`data/`（运行态）**均在 `.gitignore`**，请勿提交。
- `docker-compose.yml` 把 `config.json` 挂成**可写**，因为 `login` 要自动写 `account_keys`；介意的话可以改回 `:ro`（然后手动维护密钥）。
- 暴露到公网前请务必配置密钥 + 反代 + HTTPS，并限制来源 IP。

## License

上游 [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) 与 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 均为 MIT 协议，本仓库沿用。原项目版权声明继续有效。

供个人学习研究使用。上游协议随时可能变化，按现状提供，不承担任何连带责任。

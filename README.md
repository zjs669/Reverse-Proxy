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
- [调用示例](#调用示例)
- [HTTP 端点](#http-端点)
- [本地接口](#本地接口)
- [账号池与路由](#账号池与路由)
- [token 自动续期](#token-自动续期)
- [Web 管理台](#web-管理台)
- [常见问题](#常见问题)
- [Android App（手机当网关）](#android-app手机当网关)
- [安全](#安全)
- [License](#license)

---

## 特性

| 能力 | 说明 |
|---|---|
| OpenAI 协议兼容 | `POST /v1/chat/completions`（流式/非流式）、`GET /v1/models` |
| 多供应商 | CodeBuddy（国内 cn / 国际 intl）、Trae（国内 cn / 国际 intl）、Qoder、豆包工作、**DeepSeek 网页版** |
| 多账号池 | 加权选号、在途并发限制、失败换号、软冷却 + 熔断 |
| 四层鉴权 | 主密钥 / 区域密钥 / 供应商密钥 / **账号级密钥**（一把 key 锁死一个号） |
| 模型目录动态拉取 | `/v1/models` 直接来自上游，新增模型自动出现（豆包从网页目录解析） |
| token 自动续期 | 按「剩余寿命」提前换新，短寿命凭证（Trae 8h）也不会断 |
| 工具调用 | CodeBuddy / Trae / Qoder 支持 `tools` / `tool_calls`；豆包 / DeepSeek 不支持（见 FAQ） |
| 会话粘性 | 同一会话尽量复用同一账号，降低上下文丢失 |
| 定时任务 | 签到、活跃上报、token 保活（仅 CodeBuddy 体系有意义） |
| **Android App** | 同一套网关打包成 APK，手机自己就是网关，见 [`apk/`](apk/) |

---

## 快速开始

前提：Docker + Docker Compose。

### 1. 构建并启动

```bash
cp config.example.json config.json   # 模板已开启管理台（口令 123456，务必改掉）
mkdir -p auths data
docker compose up -d --build
docker logs reverse-proxy --tail 20
```

零账号也能启动（第一次部署 `auths/` 为空是正常状态）。看到这行即成功：

```
Reverse-Proxy listening on :7863 (...)
```

### 2. 打开管理台添加账号

浏览器打开 **http://127.0.0.1:7863/admin**，用 `config.json` 里的 `admin.password` 登录（模板默认 `123456`，**上线前必须改**），然后：

**账号 → 右上角「+ 添加账号」** → 选供应商 → 按提示授权或粘贴凭证 → 完成即入池，页面会同时给出该号的专属密钥。

| 供应商 | 需要什么 |
|---|---|
| CodeBuddy 国内 / 国际 | 什么都不用填：生成授权链接，浏览器登录后自动收号 |
| Trae 国内版（trae.cn） | `sourceId`（= 请求头 `X-Cloudide-Session`），网关拿它铸 8h JWT |
| Trae 国际版（trae.ai） | 网页版 JWT（带 `data.source_id`）+ `sourceId`；两套独立体系 |
| Qoder | `pat`（`pt-…`，集成页可建） |
| 豆包工作 | F12 复制整段 Cookie（含 `sessionid`） |
| DeepSeek | localStorage 的 `userToken.value` |

各凭证的具体位置、寿命与续期方式见 **[PROVIDERS.md](PROVIDERS.md)**。

### 3. 客户端接入

| 项 | 值 |
|---|---|
| Base URL | `http://127.0.0.1:7863/v1` |
| API Key | 管理台「密钥」页里某账号的专属密钥（锁死该号）；或自配主密钥 `api_key`（调度全池） |
| 模型 | 直接问 `GET /v1/models` |

### 4. 验证

```bash
curl -s http://127.0.0.1:7863/healthz
curl -s http://127.0.0.1:7863/status -H "Authorization: Bearer <该号密钥>"
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
    "codebuddy": [], "intl": [], "trae": [], "qoder": [], "doubao": [], "deepseek": []
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
  "deepseek":        { "session_ttl": "24h",              // 上游会话坐标有效期（上游 3 天到期）
                       "session_file": "./data/deepseek_sessions.json" },
  "cooldown":        { "soft_rate": "600s", "soft_rate_max": "2h",
                       "disabled": false },  // ★ true = 关掉网关侧全部限流/风控（见「账号池调度」）
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

## 凭证与续期

各家“最少必要信息”与获取位置（管理台「+ 添加账号」弹窗里也有同样的指引）：

| 供应商 | 最少要什么 | 去哪拿 | 寿命 / 续期 |
|---|---|---|---|
| CodeBuddy 国内 / 国际 | 什么都不用粘（授权链接） | 管理台生成链接 → 浏览器登录 | 较长，自带 refreshToken 自动续 |
| Trae | `sourceId`（= `X-Cloudide-Session`） | trae.cn → F12 → Network → 任一请求头 | source_id 长期有效；8h JWT 由网关自动铸 |
| Qoder | `pat`（`pt-…`） | qoder.com.cn/account/integrations | 长期；`dt-` 约 30 天，临期自动换新 |
| 豆包工作 | `sessionid`（或整段 Cookie） | 豆包工作 → F12 → Network → `Cookie:` | ~30 天，过期后重新粘贴 |
| DeepSeek | `accessToken`（`userToken.value`） | chat.deepseek.com → F12 → Local Storage | 无 refresh 接口，失效后重新粘贴 |

- **能自助续期**：CodeBuddy / Trae / Qoder —— 网关 token 看护按剩余寿命自动换新，无需人工。
- **不能自助续期**：豆包 Cookie（约 30 天）、DeepSeek userToken（上游无 refresh）—— 失效后到管理台「账号」页重新添加一次即可；同 uid 会覆盖原槽位，不会产生重复账号。

管理台「账号」页还有「立即续期 / 解除冷却 / 删除账号」，日常运维不需要命令行。

---

## 模型

`/v1/models` 的模型来自上游**动态拉取**，随账号权益变化，不需要在配置里维护。
`:low / :medium / :high / :xhigh` 是**推理强度**后缀，会映射成上游的思考深度。
DeepSeek 的深度思考 / 联网搜索默认开启，请求体加 `{"thinking":false}` / `{"search":false}` 关闭。

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

### 关闭深度思考 / 联网搜索（DeepSeek）

```bash
curl -s http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer $REVERSE_PROXY_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-instant",
    "thinking": false,
    "search": false,
    "messages": [{"role": "user", "content": "1+1=?"}]
  }'
```

> 默认两者都开；显式传 `false` 才关。思考内容在流式里走 `delta.reasoning_content`，
> 非流式在 `message.reasoning_content`。

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

Only CodeBuddy / Trae / Qoder 会把 `tool_calls` 原样回给你；豆包 / DeepSeek 只会返回纯文本。

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

## 本地接口

这些接口只给**本机/容器内部**用（管理台前端、Docker HEALTHCHECK、运维排错），不要当对外 API 用。

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/healthz` | 容器探活（Dockerfile HEALTHCHECK 用）；池内无可用账号返回 503 |
| GET | `/status` | 账号池状态：总数/健康/冷却/禁用/满载；带账号密钥时只返回该号 |
| GET | `/usage` | 逐账号用量与状态明细（qoder 会回写剩余额度） |
| GET | `/admin/` | 管理台单页（内嵌 HTML，`/admin` 会 301 到这里） |
| POST | `/admin/api/login` | 口令换 Cookie（有效期 12h，HttpOnly + SameSite=Strict） |
| POST | `/admin/api/logout` | 清除登录 Cookie |
| GET | `/admin/api/overview` | 概览：运行时长、池汇总、成功率、按供应商分布 |
| GET | `/admin/api/accounts` | 账号列表（不返回凭证，也不返回专属密钥明文） |
| POST | `/admin/api/accounts/add` | 添加账号（粘贴型：trae / qoder / doubao / deepseek） |
| POST | `/admin/api/accounts/oauth/start` | CodeBuddy 设备流：返回授权链接 + ticket |
| GET | `/admin/api/accounts/oauth/status?ticket=` | 轮询授权结果（pending / ok / error） |
| GET | `/admin/api/accounts/{uid}` | 单账号详情（含专属密钥明文，前端默认掩码） |
| POST | `/admin/api/accounts/{uid}/renew` | 立即续期该号 |
| POST | `/admin/api/accounts/{uid}/revive` | 解除冷却 / 禁用 |
| POST | `/admin/api/accounts/{uid}/key` | 重置该号的专属密钥 |
| DELETE | `/admin/api/accounts/{uid}` | 删除账号（从 auth.json 移除并清 account_keys） |
| GET | `/admin/api/accounts/{uid}/models` | 拉取该号可用模型 |
| GET | `/admin/api/accounts/{uid}/usage` | 查该号剩余积分（qoder / codebuddy / trae 支持；豆包、DeepSeek 为网页接口，返回 supported=false + 原因） |
| POST | `/admin/api/renew-all` | 全量巡检续期（= 手动触发 token 看护） |
| GET / POST | `/admin/api/keys` | 读/写四类密钥（读默认掩码，`?reveal=1` 回明文） |
| GET / POST | `/admin/api/settings` | 读/写 `config.json`（写前跑完整校验） |
| GET | `/admin/api/logs?since=&tail=` | 增量拉取日志（环形缓冲 2000 行） |

约束：`/admin/*` 只接受 `allow_cidr` 白名单来源（其余一律 404），所有写操作需登录 Cookie + `X-Requested-With` 头。

---

## 账号池与路由

```
客户端 ──Bearer key──▶ 网关 ──选号──▶ 上游账号 ──协议翻译──▶ CodeBuddy/Trae/Qoder/豆包/DeepSeek
                        │
                        ├─ 账号级密钥：锁死单号，不轮换
                        ├─ 供应商/区域密钥：在该范围内轮换
                        └─ 主密钥：全池轮换
```

- **选号**：按「余额/成功率/闲置补偿」加权随机，跳过冷却中、熔断中、在途满额的账号。
- **换号**：401/限流/5xx 会换下一个号重试；显式 `session dead` 连续 3 次才禁用（避免误杀）。
- **软冷却**：限流触发指数退避（`600s` → 封顶 `2h`），到期自动恢复。
- **会话粘性**：同一会话在 TTL 内尽量复用同一账号（`session_sticky`）。

**彻底关掉限流与风控**（本地单实例/调试推荐）：

```jsonc
"cooldown": { "disabled": true },      // 任何错误都不罚账号：429/404 冷却、余额硬冷却、
                                        //   12153 禁用、5xx 熔断计数全部跳过，
                                        //   并在启动时抹平 state.json 里的历史冷却/禁用
"pool":     { "breaker_threshold": 0,  // 熔断器关闭（默认 3）
              "max_in_flight": 0 }      // 在途并发不限（默认 3，满额会让选号直接失败）
```

此时上游的 429/402/12153 会原样回传给客户端，网关不再替你退避——账号密钥不再因
冷却/满额报 `no_healthy_account`（账号真的未加载仍会报，那是 `auth.json` 缺凭证）。
代价是被限流时会持续硬撞上游，多账号池的调度优势也随之失去。

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

> **DeepSeek 不走换新，只做存活校验**：上游没有 refresh 接口，`RefreshToken` 调用 `/api/v0/users/current` 判定是否 40003；失效即禁用该号并在日志给出重登命令。
> 因此 `expiresAt` 只是「估算寿命」（登录时按 30 天写入），真正是否可用以上游返回为准。

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
# 过期但还能靠长期凭证自愈（trae 的会话 / 其他家的 refreshToken）——本次没换成功，20m 后再试：
WARN: token-watch <uid>: trae token 已过期 13h43m，仍可用长期凭证自愈（本次未换成功，20m0s 后重试）：…
# 真得人工重登（既无 refreshToken、也无会话凭证）：
WARN: token-watch <uid>: qoder token 已过期 3h20m，且无 refreshToken/会话凭证可自救 → 需重新登录：…
```

> 看到第一条（trae 过期但仍自愈）不必紧张：上游铸令牌偶发失败（网络抖动/5xx）时就是这样，
> 看护会自己重试；只有第二条才是真得去重粘凭证。

---

## 常见问题

**Q：调用返回 `401 invalid_api_key`？**
`config.json` 里存在任一密钥后就要求 Bearer。检查：密钥是否与 `account_keys` 顶层一致、是否多了空格、改完是否 `docker restart`。

**Q：返回 `503` / 模型列表为空？**
该供应商没有可用账号，或 token 过期。看启动日志里的凭证体检：

```
WARN: [auth] qoder/我的qoder 凭证已于 2026-11-01 20:38 过期，且无 refreshToken（不能自动刷新）；续期：./login qoder <昵称>
# trae 的体检措辞不同：它能靠登录会话自愈，所以不要求人工重登
```

**Q：账号密钥报 `503 no_healthy_account: pinned account … unavailable (cooling/disabled/not loaded)`？**
账号级密钥只走绑定的那一个号，该号被冷却/禁用就直接失败（绝不换号）。用同一把 key 请求
`GET /status` 可看它到底是 `cooling`（含 `cool_remaining_sec`）、`disabled`（含
`disabled_reason`）还是 `in_flight` 满额。不想等冷却就把 `cooldown.disabled` 设为 `true`
（见「账号池调度」）。

**Q：Trae 的 JWT 只有 8h，是不是得每天重登？**
不用。Trae 是**两层令牌**：8h 的 `Cloud-IDE-JWT` 只是短期衍生品，底下还有一枚长期有效的
登录会话标识 `source_id`（= JWT `payload.data.source_id` = 请求头 `X-Cloudide-Session`，
网关存在 `extra.sourceId`）。实测《GetUserToken 返什么令牌》：

| 发什么 | 拿回什么 |
|---|---|
| 只发 `X-Cloudide-Session`（连 `X-Uid` 都不带） | **新铸 JWT，寿命 = now + 8h** ✅ |
| 只发 `X-Cloudide-Token: <有效 JWT>` | 同一个 `exp` 的令牌（**回显，不续命**！） |
| 只发 `X-Cloudide-Token: <过期 JWT>` | 401 / `20101` Token 无效 |
| 只发 `X-Uid` | 401 / `20101` |

第三行是关键：**拿 JWT 本身续期是个错觉**——未过期时它只回显当前令牌（到期时刻不推后），
过期后直接 401。只有带上会话凭证才会真的把寿命重置为 now+8h。
所以旧版只发 `X-Cloudide-Token` 的实现，“自动续期成功”其实一点命也没续上。

因此 Trae 真正需要的输入只有 `source_id` 一个，在管理台「+ 添加账号 → Trae」只需粘：

```json
{ "sourceId": "<从 F12 复制的 X-Cloudide-Session>" }
```

剩下的交给看护：每 10m 巡检，剩 2h（`refresh_window_by_provider.trae`）就靠会话铸新令牌；
即使停机过夜、令牌已过期，启动后也能自救（实测：过期 13h43m 的号启动即续回 8h）。
真正还需要手工的只有一种：**会话本身失效**（在别的设备登出、或改密码顶掉会话）——
那时重新从 F12 复制一份 `X-Cloudide-Session` 跑同一条命令即可（`auth.json` 里没 `sourceId` 时也可这样补）。

> 2026-11-04 修了一个“看护看上去在跑、其实从未生效”的坑：旧版 `login` 自己解 JWT 的 `exp` 时，
> 先补 `=` 再用 `base64.RawURLEncoding` 解码——Raw 编码不接受填充，载荷长度不是 4 的倍数时
> 必然失败（实测约 3/4 的 Trae JWT 命中），于是 `expiresAt` 被写成 `farFuture`(2100)。
> `NeedsRefresh` 与 token 看护都按这个字段判定，结果是**既不续期也不告警**，
> 账号每次都在登录后整 8h 静默死亡（看起来就像“只能人工重粘 JWT”）。现在 `exp` 统一由
> `auth.JWTExp` 解析，且加载 trae 凭证时以令牌自身 `exp` 为权威收紧占位值（历史 2100 值下次
> 启动即修正）；续期失败时日志会区分“仍可自愈”与“真得重登”，不再默默放着死号。

**Q：豆包说不了话 / 不能执行命令？**
豆包网页版接口**不支持 Function Calling**，请求里的 `tools` 会被忽略，只能纯文本问答。要跑 shell 请用 CodeBuddy / Trae / Qoder 的密钥。

**Q：豆包换模型名，回答没区别？**
上游按账号默认档位走，模型名目前只是标签（已知限制）。但**推理强度**（`reasoning_effort`）是真生效的。

**Q：DeepSeek 说不了话 / 不能执行命令？**
DeepSeek 网页版同样**不支持 Function Calling**（请求里的 `tools` 会被忽略）。另外它每次请求都要解一次 PoW（约 40ms），且上游会话 3 天过期——过期后网关自动重建会话（对客户端透明，但会丢上下文）。

**Q：DeepSeek 报 `40003` / 日志说 `session invalid`？**
`userToken` 失效。DeepSeek 网页版**没有 refresh 接口**，只能重新登录一次，然后到管理台
「+ 添加账号 → DeepSeek」重新粘一次（同 uid 会覆盖原槽位，不会重复建号）：

```json
{ "accessToken": "<F12 → Application → Local Storage → userToken.value>" }
```

**Q：DeepSeek 能关掉深度思考 / 联网搜索吗？**
两者默认都开。请求体顶层加字段即可关：

```jsonc
{ "model": "deepseek-instant",
  "thinking": false,      // 关深度思考（等价 thinking_enabled:false / reasoning_effort:"none"）
  "search": false,        // 关联网搜索（等价 search_enabled:false）
  "messages": [{"role":"user","content":"你好"}] }
```

> 关掉思考会更快更省，但推理/数学题质量会下降；关掉搜索则不会再看网页。
**Q：DeepSeek 多轮对话记不住上文？**
网关按请求体里的 `metadata.conversation_id` 做会话绑定。客户端**每轮都要带同一个 `conversation_id`**；不带就会走「每轮新建会话 + 全量重放历史」模式（也能用，但更慢更费 token）。

**Q：qoder 突然不能对话了（流式挂死 / 180s 后 502）？**
通常是 `dt-` 过期且未被自动续期。当前版本会在日志里打 `凭证已自动续期` 并自动重试；若看到 `凭证失效且无 drt-/pat，无法自动续期`，在管理台重新添加一次该号（建议走 PAT 路线，长期有效）。详见 [PROVIDERS.md §2.3.1](PROVIDERS.md)。
**Q：改了 `config.json` 要重新 build 吗？**
不用。它是挂载文件：密钥类（`api_key` / `api_keys` / `account_keys` / `provider_keys`）在管理台保存即刻生效；其余字段（监听地址、调度小时等）需 `docker restart reverse-proxy` 才重读。只有改 Go 代码才 `docker compose up -d --build`。

**Q：容器连不上国际版 `codebuddy.ai`？**
宿主机代理的 fake-ip 会把它解析成 `198.18.x.x`。`docker-compose.yml` 已用 `extra_hosts` 固定真实 IP；IP 失效时按注释里的 DoH 命令重新解析替换。

---

## Android App（手机当网关）

不想用服务器？`apk/` 目录把同一套网关打包成 Android App —— **手机自己就是网关**，监听 `127.0.0.1:7863`，对外提供同样的 OpenAI 兼容接口。

| 项 | 说明 |
|---|---|
| 支持架构 | arm64-v8a（真机） |
| 供应商 | 与主项目一致；Trae 只填 `sourceId`、Qoder 只填 `pat`、CodeBuddy 点按钮生成授权链接 |
| 密钥 | 主密钥（调度全池）+ **账号级密钥**（一号一把，锁死单号），与 `gen_account_keys.py` 同口径 |
| 续期 | 网关每 10 分钟自动巡检；App 另提供「立即续期」按钮 |

**构建时要开 cgo**，否则网关的所有上游请求都会失败：

```
dial tcp: lookup api.trae.cn on [::1]:53: read udp [::1]:43413->[::1]:53:
read: connection refused
```

原因是 Go 标准库在 `GOOS=android` 下**强制要求 cgo 解析器**（`src/net/conf.go`，issue #10714）。`CGO_ENABLED=0` 编出来的二进制会退回纯 Go 解析器，去读 Android 上并不存在的 `/etc/resolv.conf`，最终落到 `localhost:53`。这跟账号、密钥、手机网络无关，**只与构建参数有关**。

完整构建步骤、目录结构、踩坑记录见 [`apk/README.md`](apk/README.md)。

---

## Trae 国际版（trae.ai）

Trae 国内版（trae.cn）与国际版（trae.ai）是**两套完全独立的体系**，域名、认证头、请求体格式、积分接口都不同。

| 项 | 国内版 | 国际版 |
|---|---|---|
| 换 token | `api.trae.cn` | `api.trae.ai` |
| 对话 / 模型 | `trae-api-cn.mchost.guru` | `coresg-normal.trae.ai` |
| 积分 | `api.trae.cn/trae/api/v2/…` | `ug-normal.trae.ai/trae/api/v1/…` |
| 需要的凭证 | `sourceId` | **网页版 JWT**（带 `data.source_id`）+ `sourceId` |
| `messages[].content` | 字符串 | **数组** `[{type,text}]` |

### 接入要点

```json
{
  "traeai_1": {
    "provider": "trae",
    "accessToken": "<网页版 JWT>",
    "expiresAt": 1793874419,
    "domain": "www.trae.ai",
    "uid": "<trae-intl-uid>",
    "extra": {
      "region": "intl",
      "sourceId": "<sourceId（续期唯一凭证，勿外传）>"
    }
  }
}
```

> 账号标签用 `traeai_` 前缀而不是 `trae_`：国内（trae.cn）与国际（trae.ai）域名、认证头、
> 积分接口全不同，光看 `trae_1` / `trae_2` 分不出打的是哪家，排查时必须一眼可辨。
> 由 `provision.KeyPrefix` 按 `extra.region` 自动生成。

- `extra.region = "intl"` 决定所有上游调用走 trae.ai（不写就按国内版处理，会全部 404/401）
- `extra.sourceId` 是续期的唯一凭证，**必须存** —— 网页版 JWT 只有 3 小时，靠它无限次换新

### 三个坑

1. 积分端点路径带 `current`：`user_current_entitlement_list`，少了就 404
2. 对话的 `content` 必须是数组，传字符串报 `4001 cannot unmarshal string`
3. 模型名里的 `__dollar` 必须剥掉（`gpt-5.2__dollar__dev` → `gpt-5.2__dev`），否则 `4001 param is invalid`

完整的请求头、最小参数、响应字段位置与实测记录见 `internal/upstream/trae.go` 顶部注释。

---

## Web 管理台

不想敲命令？开一个可视化网页，功能对齐 Android App：加账号、看账号、管密钥、手动续期、切配置。

`config.example.json` 已默认开启（口令 `123456`，**上线前必须改**）：

```json
{
  "admin": {
    "enabled": true,
    "password": "换成你自己的口令",
    "allow_cidr": ["127.0.0.1/32"]
  }
}
```

重启容器后打开 `http://<服务器IP>:7863/admin`。

> `allow_cidr` 只需写回环：Docker 端口映射下宿主机请求的源地址是容器默认网关（如 `172.18.0.1`），
> 网关启动时会自动把这一个网关地址加进白名单，所以配置里不用写任何内网网段。

| 能力 | 说明 |
|---|---|
| 概览 | 池汇总（总数/健康/冷却/禁用/满载）、累计成功率、按供应商分布、一键巡检续期 |
| 账号 | **+ 添加账号**（CodeBuddy 授权链接 / Trae sourceId / Qoder PAT / 豆包 Cookie / DeepSeek userToken），建号即入池；列表 + 详情（含**查额度/积分**）；续期；解除冷却；删除 |
| 密钥 | **账号专属密钥**：一行一个账号，就地 显示/复制/重置/删除；主密钥 / 区域密钥 / 供应商密钥在「设置」页直编 `config.json` |
| 设置 | 直接编辑 `config.json`，保存前跑完整校验 |
| 日志 | 网关 stdout 实时滚动，增量拉取 |

**设计要点**

- **内嵌在同一进程同一端口**（`/admin`），不另起服务：管理台要读写 `config.json`/`auth.json`/`state.json`，这些已被网关独占且有锁与原子写；续期/清冷却/看池状态也活在进程内存里。
- **默认关闭**，开启后默认只允许内网。白名单外的请求返回 **404**（不是 403），不暴露管理台存在。
- **改密钥/设置不用重启**：热重载走 `Handler` 的 atomic 配置指针，旧密钥立即失效、新密钥立即生效。
- **建号与 CLI 同源**：Web 的「+ 添加账号」与 `./login` 共用 `internal/provision`（换 token / 归一 / 落盘 / 同 uid 复用槽位），不会出现两套协议实现分叉；建号完成立即入池。
- **写配置必须过校验**：保存时先写临时文件、跑一遍完整 `Load()+normalize()`，校验不过就删临时文件、正式配置一个字都不动 —— 杜绝"存进一份网关启动即 fatal 的配置"。
- **账号接口绝不返回凭证**：`accessToken`/`refreshToken`/`extra.pat` 根本不出 DTO；专属密钥默认掩码。

---

## 安全

- `config.json`（含密钥）、`auths/`（含上游 token）、`data/`（运行态）**均在 `.gitignore`**，请勿提交。
- `docker-compose.yml` 把 `config.json` 挂成**可写**，因为管理台（以及 CLI 建号）要写 `account_keys` 与配置；介意的话可以改回 `:ro`（然后手动维护密钥）。
- 暴露到公网前请务必配置密钥 + 反代 + HTTPS，并限制来源 IP。

## License

上游 [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) 与 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 均为 MIT 协议，本仓库沿用。原项目版权声明继续有效。

供个人学习研究使用。上游协议随时可能变化，按现状提供，不承担任何连带责任。

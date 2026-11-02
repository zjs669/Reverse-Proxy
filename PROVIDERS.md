# 多供应商接入指南（PROVIDERS.md）

`workbuddy2api` 在原有 CodeBuddy 反代基础上，扩展了 **Provider 抽象层**，支持在**同一网关**里同时挂多个上游供应商：

| provider | 上游 | 状态 | 说明 |
| --- | --- | --- | --- |
| `codebuddy` | copilot.tencent.com / codebuddy.ai | ✅ 已支持 | 默认回落目标，无需显式配置 |
| `trae` | trae-api-cn.mchost.guru（国内版） | ✅ 已支持 | chat_v3 协议，SSE 事件 `output`/`done`/`[DONE]` |
| `qoder` | gateway.qoder.com.cn（国内版） | ✅ 已支持 | COSY 私有协议 + 换表编码 + 嵌套 SSE，HTTP/1.1 强制 |
| `doubao` | www.doubao.com（豆包网页版） | ✅ 已支持（**仅对话**） | Cookie 鉴权 + `/chat/completion` SSE；**不支持 Function Calling** |

> Qoder 上游对 HTTP/2 的 SSE 会返回 `INTERNAL_ERROR`，网关已为 Qoder 单独禁用 HTTP/2 协商（`Client.QoderHTTP`，`TLSNextProto` 置空）。
>
> 豆包是**网页版逆向**通道：鉴权靠 `sessionid` Cookie，协议无工具调用能力，只适合「纯聊天 / 写作 / 翻译」类负载，不能作为编码智能体（Claude Code / Codex 之类）的后端。

---

## 1. 账号密钥模型（仅 account_keys）

网关**没有**主密钥、**没有**区域密钥、也**没有**供应商密钥——鉴权完全由 `account_keys` 一张表驱动：
（`internal/server/handler.go` 的 `withAuth` 规定：**只要** `account_keys` 非空就鉴权；表为空才放行。当前配置里该表非空，所以必须带有效密钥才能调用。）

**账号密钥 `account_keys`** — 嵌套结构 `{名字: {uid: 密钥(secret)}}`：
- **外层「名字」**是展示用标签（与 `auth.json` 的账号标签对应，如 `qoder_1` / `codebuddy_2`，也可自定义为任意好记的名字）；
- **内层 `{uid: secret}`** 是一个账号一把专属密钥。带该密钥的请求**锁死**到内层 uid 对应的那一个账号（不参与轮换、不会回退到池中其他号）。

唯一的「连接」是：内层的 **uid** 必须等于 `auth.json` 里某账号的 `uid` 字段（外层名字仅给人看，池仍按 uid 关联）。

> 也就是说：`account_keys` 的密钥（如 `qoder_1` 下 `<qoder-uid>…→762c97…`）只能走**那一个** qoder 账号；要用别的账号就换对应的密钥。

### config.json 示例

```jsonc
{
  // ↓↓↓ 账号级密钥：{名字: {uid → 专属密钥}}（锁死单账号）↓↓↓
  "account_keys": {
    "qoder_1":     { "<qoder-uid>": "<qoder-key>…（qoder 专属）" },
    "trae_1":      { "<trae-uid>": "<trae-key>…（trae 专属）" },
    "codebuddy_1": { "<codebuddy-uid>-…": "<cb1-key>…（codebuddy_1 专属）" },
    "codebuddy_2": { "<codebuddy-uid>-…": "<cb2-key>…（codebuddy_2 专属）" }
  }
}
```

校验规则（`cmd/server/config.go` `normalize`）：
- 每个内层密钥（secret）不能为空，且**全局唯一**——同一把密钥不得出现在两个账号下（启动报错）。
- normalize 遍历 `{名字: {uid: secret}}` 生成反向索引 `AccountKeyToUID`（secret → uid），供 `withAuth` 快速反查锁号。

---

## 2. 凭证文件（auths/auth.json 单文件）

各供应商凭证**如何获取**见下面 §2.1–§2.4 各小节（统一用 `./login <provider> <region> <昵称>` 向导或手填）。

所有账号集中存放在**单个文件** `auths/auth.json`（由 `config.json` 的 `auth_file` 指向），顶层是一张 map，**键即账号标签**：

```jsonc
{
  "qoder_1":      { "provider":"qoder", "uid":"<qoder-uid>-…", "nickname":"…", "accessToken":"dt-…", "refreshToken":"drt-…", "expiresAt":…, "extra":{…} },
  "trae_1":       { "provider":"trae",  "uid":"<trae-uid>", "nickname":"…", "accessToken":"eyJ…", "extra":{…} },
  "codebuddy_1":  { "account":{…}, "auth":{…} },   // 嵌套形同样兼容
  "codebuddy_2":  { "account":{…}, "auth":{…} },
  "codebuddyai_1":{ "provider":"codebuddy", "uid":"…", "region":"intl", … }  // 国际版多 ai 前缀
}
```

**键命名规则**（由 `cmd/login` 向导自动生成，也可手填）：
- 形如 `<provider>_<n>`，序号自动顺延（`qoder_1` / `qoder_2` …）。
- 国际版 CodeBuddy 前缀为 **`codebuddyai_`**（比国内版多 `ai` 两字），其余照常。
- `nickname` 字段可自由设置、不校验，仅用于展示（如客户端列表、日志）。

`config.json` 设置：`"auth_file": "./auths/auth.json"`（旧版 `auth_dir` 多文件模式已废弃）。网关启动时 `LoadFile` 读入整张 map，按每个账号内部的 `uid` 编入账号池；token 刷新时 `SaveAtomic` **只原子回写这一个条目**，不打扰其他账号。

每个供应商的账号 JSON 形态（均兼容嵌套形 `{"auth":{...},"account":{...}}` 与扁平形）：

### 2.1 Trae（provider="trae"）

```jsonc
{
  "provider": "trae",
  "accessToken": "Cloud-IDE-JWT 的 JWT 原文",
  "refreshToken": "可选：Trae refresh token（ExchangeToken 刷新用）",
  "expiresAt": 0,
  "uid": "X-Uid（Trae 账号标识）",
  "nickname": "我的trae",
  "extra": {
    "appId": "",        // 留空用内置 traeAppID
    "appVersion": "",   // 留空用内置 traeAppVersion
    "sourceId": "JWT payload.data.source_id（X-Cloudide-Session 头值，脚本自动解析，无需手填）"
  }
}
```
- **请求头（缺一不可）**：
  - `Authorization: Cloud-IDE-JWT <jwt>` ← `accessToken` 原样填入（Bearer 风格的 `Cloud-IDE-JWT ` 前缀由网关加）
  - `X-Cloudide-Token: <jwt>` ← 与 `accessToken` 同源
  - `X-Uid: <uid>` ← 账号标识
  - **`X-Cloudide-Session: <source_id>` ← 必需！值取自 JWT 的 `payload.data.source_id`**（即你贴的那串 `9VPsQzKsIeka_M02NxXo4AdvJxPZUePYoadSSNmLNXM=.18e2a7a0272c8183`）。缺失时上游会拒绝或限流。网关已自动从 `accessToken` 解析并发送，凭证里的 `extra.sourceId` 即为此值。
- 模型名需带 `__dev` 后缀（网关自动补全）；`messages[].content` 必须是 `parts` 数组。
- **`expiresAt` 由 login 向导自动从 JWT 的 `exp` 字段解析写入**（无需手填）；缺失会导致网关每次对话都尝试刷新、无 refreshToken 时直接拒绝。JWT 是无状态 session 令牌，通常只有几小时有效，过期后取新 JWT 重跑向导即可。
- **已知现象**：Trae 上游对新账号/免费档可能直接返回"exceeded the rate limit"作为正常 SSE chunk（非 HTTP 429）。这是账号侧限流，不是网关问题；换账号或等冷却后可正常对话。
- **Token 获取**：跑 `./login trae cn <昵称>`（或容器内 `/app/login trae cn <昵称>`），粘贴 `Cloud-IDE-JWT` 后向导自动解析 `exp→expiresAt`、`data.source_id→extra.sourceId`，写进 `auth.json` 的 `trae_<n>` 条目。
- ⚠️ Trae 刷新协议（ExchangeToken）细节未经实机验证；刷新失败会禁用账号，重新取 JWT 写回即可。

### 2.2 Qoder（provider="qoder"）

```jsonc
{
  "provider": "qoder",
  "accessToken": "dt-（deviceToken，COSY 对话网关的 Bearer；PAT 兑换的 jt- 不适用于对话网关）",
  "refreshToken": "drt-（deviceToken refresh，~1 年轮换）",
  "expiresAt": 0,
  "uid": "Qoder 账号标识（任意唯一值）",
  "nickname": "qoder-1",
  "extra": {
    "machineID": "COSY 机器指纹 ID",
    "machineToken": "COSY 机器指纹 Token（RawURLEncoding）",
    "machineType": "COSY 机器类型"
  }
}
```
- COSY 请求签名需要持久机器指纹（`machineID`/`machineToken`/`machineType`），缺一不可（`NewCosySession` 缺任一直接报错）。
- 上游：Gateway `gateway.qoder.com.cn`（对话/模型）、OpenAPI `openapi.qoder.com.cn`（drt 刷新）。
- ⚠️ **PAT(pt-) → jobToken(jt-) 不能用于对话网关**：`jt-` 只对 `openapi.qoder.com.cn`（OpenAPI）有效；**对话网关 `gateway.qoder.com.cn` 只认设备令牌 `dt-`，喂 `jt-` 会被拒并返回 `105 Login expired`**。因此 `accessToken` 必须是设备流拿到的真 `dt-`，不是 PAT 兑换的 `jt-`。
- **怎么拿 `dt-`/`drt-`**：Qoder CN 客户端登录时自动走设备授权流。令牌存于 `~/.qoder-cn/credentials/auth.json`（AES-GCM + 系统绑定，**无法离线解密，不要直接读**）；实测可取的两处：**机器指纹**明文在 `~/.qoder-cn/shared_client/cache/machine_token.json`（`id`/`token`/`type` → `extra.machineID`/`machineToken`/`machineType`）；**`dt-`/`drt-`** 由客户端进程实时持有在内存（同账号只读扫描 `dt-`/`drt-` 前缀取）。令牌会轮换（`dt-` 约 30 天），写入后设合理 `expiresAt`。或走设备授权 URL `https://qoder.cn/device/selectAccounts?...` 浏览器登录后轮询取得。
- 使用 `scripts/qoder_bootstrap.py --print-fp` 生成机器指纹；`--dt <dt-> --drt <drt->` 把设备令牌写进 `auths/auth.json` 的 `qoder_<n>` 条目（或手填到该条目 `extra` 三件套）。

### 2.3 Doubao 豆包（provider="doubao"）

```jsonc
{
  "provider": "doubao",
  "accessToken": "sessionid Cookie 的值（唯一必需凭证）",
  "uid": "doubao-1",           // 任意唯一标识，用于池内区分/日志
  "expiresAt": 4102444800,     // Cookie 无显式过期时间，填一个远期值即可
  "nickname": "doubao-1",
  "extra": {
    "ttwid": "可选：ttwid Cookie",
    "passport_csrf_token": "可选：passport_csrf_token Cookie",
    "device_id": "可选：19 位设备 ID，缺省用内置常量",
    "web_id": "可选：19 位 web_id，缺省用内置常量",
    "fp": "可选：指纹令牌 verify_xxx，缺省不发送"
  }
}
```

**协议要点**（`internal/upstream/doubao.go`）：
- 对话：`POST https://www.doubao.com/chat/completion`，Query 固定携带 `aid=582478` / `device_id` / `web_id` / `version_code=20800` 等；**必须带 `Agw-Js-Conv: str` 与 `Referer: https://www.doubao.com/chat`**。
- 会话存活校验：`POST /passport/account/info/v2`（`RefreshToken` 复用它；`sessionid` 失效 → 账号被禁用，不会尝试「刷新」）。
- 请求体：`client_meta`（`bot_id` 固定）/ `messages[].content_block`（`block_type=10000` 文本块）/ `option` / `ext`。
- 思考档位：`reasoning_effort` → `need_deep_think`（`low`=快思 0 / `medium`=深思 1 / `auto`=2 / `high`=专家 3）。
- 响应：SSE，事件名 `SSE_ACK` / `json` / `message` / `done` / `gateway-error`，正文在 `data.content`（常为嵌套 JSON 字符串）；`block_type=10040` 为思考块分隔标记，思考内容被映射到 `reasoning_content`。
- **无 Function Calling**：请求里的 `tools` 会被忽略，`role:"tool"` 消息直接丢弃。若客户端强依赖工具调用（编码智能体），豆包不可用。
- 模型名直通（`doubao-seed-2.0` / `doubao-seed-1.6` / `doubao-1.5-pro` 等），`/v1/models` 走静态表（网页版无干净模型列表 API）。
  ⚠️ **注意**：网页版 `/chat/completion` 通过固定 `bot_id`（`7338286299411103781`）选对话体，请求体**不带 model 字段**，因此静态表里的模型名目前只是「可选的展示项」，实际回复统一走该 bot；等接入「模型 → bot_id 映射」后才会真正区分。

**sessionid 获取**（浏览器）：
1. Chrome/Edge 登录 `https://www.doubao.com/chat`；
2. F12 → Application → Cookies → `https://www.doubao.com`，复制 `sessionid`（以及可选的 `ttwid`、`passport_csrf_token`）；
3. 用 `scripts/doubao_session.py` 生成 `auths/auth.json` 的 `doubao_<n>` 条目，或在网页 F12 → Network 里任取一个 `chat/completion` 请求的 URL，把整段 Cookie 交给该脚本解析后写入 `auth.json`。

---

## 3. 路由与选号

- `withAuth` 中，`account_keys` 命中的密钥通过反向索引 `AccountKeyToUID` 把该账号 UID 下传 context。
- 命中账号密钥的请求走 `PickByUIDIn`，**只使用该 UID 对应的那一个账号**（不经过 region/provider 过滤、不参与轮换，协议由该账号自身 `provider` 决定）。
- 选中账号后，`h.cfg.Upstream.ProviderFor(acct)` 按账号自身 `provider` 取得实现：
  - `Name` / `ChatStream` / `RefreshToken` / `FetchModels` / `StaticModels` / `Classify` / `StreamTo` / `Aggregate`。

---

## 4. 调度器（scheduler / travel）隔离

签到 / 活跃 / 旅行任务只作用于 `codebuddy` 账号（`st.Provider == "" || st.Provider == "codebuddy"`），`trae` / `qoder` / `doubao` 走各自计费体系（豆包网页版根本没有签到概念），被跳过。

---

## 5. 构建与联调

```sh
cd workbuddy2api
docker compose up -d --build
```

- `auths/auth.json`、`data/`、`config.json` 都是挂载卷：新增/修改凭证后 `docker restart workbuddy2api` 即重新加载（`LoadFile` 只在启动时读；token 刷新由网关自动写回 `auth.json`，无需重启）。
- `scripts/` 已整目录打进镜像（容器内有 python3），也可以在容器里直接跑凭证脚本：

```sh
# 宿主机
python3 scripts/doubao_session.py --cookie "sessionid=...; ttwid=..." --uid doubao-1
# 或容器内（auths 是同一个挂载卷，落到宿主机 ./auths）
docker exec -it workbuddy2api python3 /app/scripts/doubao_session.py --help
docker exec -it workbuddy2api python3 /app/scripts/qoder_bootstrap.py --print-fp
```

### 验证某供应商可用

每个供应商用**该账号的专属密钥**（`account_keys`）访问：

```sh
# 用 trae_1 账号密钥调 /status（只返回该账号）
curl -s http://127.0.0.1:7863/status \
  -H "Authorization: Bearer <trae_1 账号密钥>"

# 用 qoder_1 账号密钥发起对话
curl -s http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer <qoder_1 账号密钥>" \
  -H "Content-Type: application/json" \
  -d '{"model":"qmodel_preview","stream":false,"messages":[{"role":"user","content":"hi"}]}'

# 豆包（先确认 auths/auth.json 里已有 doubao_<n> 条目并为其补 account_keys 密钥）
curl -N http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer <doubao 账号密钥>" \
  -H "Content-Type: application/json" \
  -d '{"model":"doubao-seed-2.0","stream":true,"messages":[{"role":"user","content":"用一句话介绍你自己"}]}'
```

### 排错

| 现象 | 可能原因 | 处理 |
| --- | --- | --- |
| Qoder 502 / `INTERNAL_ERROR` | 走了 HTTP/2 | 确认用 `Client.QoderHTTP`（已禁用 HTTP/2）；不要给 Qoder 复用 `ChatHTTP` |
| Qoder `qoder: missing machine fingerprint` | `extra` 缺 machineID/machineToken/machineType | 用 `scripts/qoder_bootstrap.py` 重新生成 |
| Qoder 401 → 反复 refresh 仍死 | dt-/drt- 失效（或误用了 PAT 换的 jt-） | 重新走设备流拿真 dt-/drt- 写回 auth.json 的 qoder_<n> 条目 |
| Trae 400 / 模型找不到 | 模型名未带 `__dev` 或目录未拉到 | 先调一次 `/models` 触发 `FetchModels` 建立映射 |
| Trae `ErrSessionDead` 频繁 | JWT 过期/无效 | 重新取 Cloud-IDE-JWT 写回 auth.json 的 trae_<n> 条目 |
| 豆包 401/403 或 `doubao session invalid` | `sessionid` 失效（登出/换设备/风控） | 重新从浏览器取 `sessionid` 写回 auth.json 的 doubao_<n> 条目，网关会热加载 |
| 豆包 `710022002` / 710022004 或 429 | 风控限流（缺 `ttwid` / 指纹异常 / 调用过快） | 补 `ttwid`、`passport_csrf_token`；降低并发；必要时换号 |
| 豆包回复为空但流正常结束 | 事件结构变化（`content` 嵌套更深） | 抓一次上游原始 SSE 存盘，对照 `doubaoWalk` 的解析分支 |
| 豆包工具调用不生效 | 网页协议不支持 Function Calling | 属预期限制，改用 codebuddy/trae/qoder |
| 请求全部 401 | 未带有效 `account_keys` 密钥 | 从 `config.json` / `账号令牌清单.md` 取对应账号的专属密钥 |

---

## 6. 已知限制（本仓库实现）

- **Qoder**：COSY 协议与 qoderwork2api 对齐移植；对话网关只认设备令牌 `dt-`（`drt-` 刷新）。PAT→jobToken 兑换出的 `jt-` **仅对 OpenAPI 有效、不能用于对话网关**（实测返回 `105 Login expired`），请勿把 `jt-` 当 `dt-` 填。
- **Trae**：refresh 协议为最佳努力实现，未经实机验证；失败即禁用账号，需手动刷新 JWT。
- **Doubao**：
  - 网页版逆向通道，**无 Function Calling / Tool Use**，不能作为编码智能体后端（`tools` 被忽略、`role:"tool"` 被丢弃）。
  - 鉴权只有 `sessionid` Cookie，无刷新机制；失效即禁用账号，需人工重新抓取。
  - 单账号无官方并发/频率保证，风控触发返回 429 或 `710022002`；多账号靠各自的 `account_keys` 密钥分流到不同号。
  - 模型能力与网页版订阅档位一致（免费号可能只给 Lite 档）；`/v1/models` 为静态表，且**当前 model 名不影响实际回复**（固定 bot_id），直接指定任意模型名结果相同。
- **通用**：全部改动已通过 `docker compose build` 实编译（见 §7）；上游协议层仍需真实凭证联调。

---

## 7. 已验证情况（2026-10-30）

在本机 Docker 实测通过：

```sh
docker compose up -d --build
# 编译成功；启动日志：account_keys=4（嵌套结构，无 provider_keys）
# 加载 4 个账号（qoder_1 / trae_1 / codebuddy_1 / codebuddy_2）

curl /healthz                     → {"healthy":4,"total":4}
curl /status (codebuddy_1 账号密钥) → 1 个账号，uid=<codebuddy-uid>…，provider=codebuddy
curl /status (qoder_1 账号密钥)     → 1 个账号，uid=<qoder-uid>…，provider=qoder
curl /v1/models (codebuddy_1 账号密钥) → 动态模型（上游实时拉取）
curl /v1/chat (doubao 账号密钥)     → 503 no_healthy_account（尚未放入 doubao 凭证，预期）
```

> 未验证部分：豆包真实对话链路（需要浏览器 `sessionid`）、trae/qoder 真实对话链路（需要各自凭证）。凭证落盘后 `docker restart workbuddy2api` 即可复测。

---

## 8. 接入新 Provider 代码规范（给什么 / 获取什么 / 踩坑点）

新增一个上游供应商（如再来个 `xxx`），按下面清单落地。核心是实现 `internal/upstream/provider.go` 的 `Provider` 接口并注册。

### 8.1 给什么（实现 `Provider` 接口的 8 个方法）
| 方法 | 职责 | 关键点 |
| --- | --- | --- |
| `Name()` | provider 标识 | 必须与 `auth.json` 条目 `provider` 字段、`config.go` `knownProviders` 三处一致 |
| `ChatStream(c, a, body)` | 发起对话，返回**上游原生流** | 返回 `(rc, status, respBody, err)`；非 2xx 时 `rc=nil`、`respBody` 带上游体供 `Classify` |
| `RefreshToken(c, a)` | 刷新凭证 | 原地改 `a.AccessToken/RefreshToken/ExpiresAt`；无刷新能力就返回 error（会禁用账号） |
| `FetchModels(c, a)` | 动态拉模型 | 返回 `[]ModelInfo`，**同时把「客户端名→上游 key」写进 `p.modelMap`** |
| `StaticModels()` | 拉取失败回退表 | 返回该 provider 自己的干净模型名（别复用 codebuddy 表） |
| `Classify(status, body)` | 错误分类 | 决定冷却/禁用：`ErrSessionDead` 会禁号、`ErrSoftRate`/`ErrHardCredit` 触发冷却 |
| `StreamTo(w, rc, model)` | 原生 SSE → OpenAI SSE | 流式；把上游增量翻译成 `chat.completion.chunk` |
| `Aggregate(rc, model)` | 原生 SSE → 单响应 | 非流式；聚合成一个 `chat.completion` |

### 8.2 还要改的接线点
1. **注册**（`internal/upstream/provider.go` `registerProviders`）：`c.providers["xxx"] = &xxxProvider{c: c}`。
   ⚠️ **持有 `modelMap` 等跨调用状态的 provider 必须存指针 `&`、方法用指针接收者**（见 §8.4 坑①）。
2. **凭证解析**（`internal/auth/auth.go` `Parse`）：识别新 provider 的凭证字段（`uid`/`nickname`/`accessToken`/`refreshToken`/`expiresAt`/`extra`）。
3. **白名单**（`cmd/server/config.go` `knownProviders`）：加上新 provider 名。
4. **登录向导**（`cmd/login/main.go`）：加 provider 分支（自动 OAuth 或粘贴凭证），键名按 `<provider>_<n>` 顺延。
5. **密钥**：跑 `python gen_account_keys.py` 为新账号补嵌套 `account_keys`，刷新 `账号令牌清单.md`。
6. **文档**：在本文件 §2 加凭证小节。

### 8.3 获取什么（凭证来源速查）
- **codebuddy**：`./login codebuddy cn|intl <昵称>` 走 OAuth 设备流。
- **trae**：`./login trae cn <昵称>` 粘贴 `Cloud-IDE-JWT`（自动解析 `exp`/`source_id`）。
- **qoder**：设备流 `dt-`/`drt-` + COSY 机器指纹三件套（`scripts/qoder_bootstrap.py`）。
- **doubao**：浏览器 `sessionid` Cookie（`scripts/doubao_session.py`）。

### 8.4 踩坑点（本仓库真实教训，务必避开）
1. **值接收者 + `modelMap` = 映射永久失效**：provider 以值存进 `providers` map，若 `FetchModels` 用值接收者，`p.modelMap=mm` 写在副本上被丢弃 → `resolveModel` 永远拿到空 map → 把客户端模型名**原样当上游 key 发出**（例：`qwen3.8-flash` 真实 key 是 `qfmodel`，发错名上游会走默认模型）。→ **有共享状态就指针接收者 + 存指针**。
2. **`monitorBody` 传 `cancel=nil` 会 panic**：`ChatStream` 成功返回流后 handler 会 `Close` 响应体；`idle.go` 的 `Close` 若无条件调 `b.cancel()` 而 cancel 为 nil → 空指针 panic → 连接被重置，客户端只看到 `Connection error`（curl `http=000`）。→ 要么给流挂真实 cancel，要么 `monitorBody` 对 `cancel==nil` 直接返回原底流（已这么修）。
3. **响应的 `model` 字段不可信**：上游常回 `auto`，网关用客户端请求名**覆盖回显**。判断真实模型只能看 `resolveModel` 出的 `upstreamKey`（`ChatStream` 已打 `client=... -> upstreamKey=...` 日志）。
4. **`resolveModel` 依赖 `modelMap` 已预热**：冷启动/拉取失败时映射为空、名字不翻译。上线后先调一次 `/v1/models` 触发 `FetchModels`，并用 `StaticModels` 兜底。
5. **`Classify` 决定账号生死**：401/403 归 `ErrSessionDead` 会直接禁号；凭证临期（如 JWT `exp`）必须正确写 `expiresAt`，否则每次对话都触发刷新、无 refreshToken 时直接拒（trae 就是典型）。
6. **provider 字段与账号标签解耦**：`auth.json` 顶层键只是给人看的标签，真正路由看条目内 `provider`/`uid`；改标签不影响选号。
7. **回退别串号**：`handler.go` `modelList` 拉取失败时按**绑定账号自身 provider** 回退其 `StaticModels`，不要无脑回落 codebuddy 表（已修）。
8. **provider 特有协议**：trae 模型名带 `__dev`、`content` 必须 parts 数组、`X-Cloudide-Session(source_id)` 必需；qoder 只认 `dt-`（喂 `jt-` 返回 `105`）、必须禁 HTTP/2（用 `Client.QoderHTTP`）；doubao 无 Function Calling。

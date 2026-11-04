# Reverse-Proxy 新设备精简部署

## 前提
- 把 `Reverse-Proxy` 文件夹整体拷到新设备
- 新设备装好 Docker（能直连 docker.io，无需配镜像源）
- 一并带上 `auths/auth.json` 可免登录

## 鉴权说明（重要）
网关只有一套**客户端访问密钥 `account_keys`**（在 `config.json` 里，已取消主密钥 / 区域密钥 / 供应商密钥），它打到 `auths/auth.json` 凭证池：

```
[你的 dsh / curl]  ──Bearer: account_key──▶  [reverse-proxy 网关]
                                                     │
                            auths/auth.json 里的各供应商 token
                                                     │
                                                     ▼
                                         [CodeBuddy / Trae / Qoder / 豆包 上游]
```

| | 网关密钥 `account_keys`（账号级） | auth.json（各供应商凭证） |
|---|---|---|
| 形态 | `{名字: {uid: 密钥}}`（嵌套） | 各账号 token |
| 保护哪段 | 你 ↔ 网关（锁死单账号） | 网关 ↔ 上游（消耗你的额度） |
| 作用 | 带这把 key 只走**那一个**账号 | 网关拿它真正调用上游 |

- 两套互不冲突：改 `account_keys` 不影响 `auth.json`；丢 `auth.json` 网关照样开门但转不出真实请求。
- `auths/auth.json` 里 `codebuddy_2`（uid `<codebuddy-uid>-...`，昵称「我的主号」）的 token 约 **2026-11-22** 过期，届时重跑 `login.sh intl <昵称>` 或 `./login codebuddy cn <昵称>` 刷新。

### 本机实际密钥（勿外泄）
- **各账号专属密钥**（`account_keys`，锁死单号）：完整明文见 `账号令牌清单.md`（该文件含密钥，**不入 git**）。
  账号：`codebuddy_1`、`codebuddy_2`、`qoder_1`、`trae_1`。
- **各供应商凭证**：已在 `auths/auth.json` 内（`accessToken`/`refreshToken`），无需手动填。

## 构建并启动
```sh
cd Reverse-Proxy
docker compose up -d --build
```

## 登录（首次或缺凭证时）

参数形态分两种，别混：

- **codebuddy**：`login codebuddy <cn|intl> <昵称>`（有区域维度；命令会**打印授权链接**，浏览器登录后自动轮询）
- **trae / qoder / doubao / deepseek**：`login <供应商> <昵称>`（**没有**区域参数，多写一个词会被当成昵称）

`<昵称>` 是账号显示名，随便取，只用于日志和 `auth.json` 的键名。

```sh
# CodeBuddy 走自动 OAuth（打印 URL → 浏览器登录 → 回车轮询拿 token）
docker exec -it reverse-proxy /app/login codebuddy cn 我的主号      # 国内版
docker exec -it reverse-proxy /app/login codebuddy intl 我的国际版  # 国际版 → 键名 codebuddyai_1

# 这四家是「粘贴凭证」型：先看它打印的取凭证指引，再从 stdin 粘入 JSON
docker exec -it reverse-proxy /app/login trae 我的trae          # 只需 {"sourceId":"…"}（X-Cloudide-Session）
docker exec -it reverse-proxy /app/login qoder 我的qoder        # 只需 {"pat":"pt-…"}
docker exec -it reverse-proxy /app/login doubao 我的豆包        # 只需 {"sessionid":"…"}
docker exec -it reverse-proxy /app/login deepseek 我的DeepSeek  # 只需 {"accessToken":"mb5+…"}
```

> ⚠️ 容器没起来（重启循环）时 `docker exec` 进不去，用一次性容器挂载同款卷来登录：
> ```sh
> docker run -it --rm --entrypoint /app/login -v %cd%/auths:/app/auths -v %cd%/data:/app/data -v %cd%/config.json:/app/config.json reverse-proxy-reverse-proxy
> ```

> **在 Windows 上不要用 `-it` 手粘**：`-it` 的伪终端里 Ctrl+D 传不进容器，程序会一直等 EOF 卡住。
> 把凭证存成文件 + `-i` 重定向即可（文件读完自动 EOF）：
> ```sh
> docker exec -i reverse-proxy /app/login doubao 我的豆包 < cred.json
> ```
> 登录成功后会**自动往 `config.json` 的 `account_keys` 写一把随机密钥**（已存在则沿用不覆盖）。

### 各家最少凭证 JSON（存成 cred.json 后按上面重定向粘入）

```jsonc
// Trae —— 只给登录会话标识（= 请求头 X-Cloudide-Session，长期有效）
// 向导会拿它向 GetUserToken 铸一枚新 JWT 入库；8h 的 JWT 不再需要人工重粘。
{ "sourceId": "<X-Cloudide-Session 的值>" }
// 也可以粘 JWT（自动解析 exp / data.id / data.source_id）：
{ "accessToken": "<Cloud-IDE-JWT 原文>" }

// Qoder —— 最简：只给 PAT（自动兑换 jobToken + 生成机器指纹 + 兜底 uid）
{ "pat": "pt-…" }
// 或已有设备流令牌（uid 也可省）：注意 jt- 不能对话，对话需真 dt-（见 PROVIDERS.md §6）
{ "accessToken": "dt-…", "refreshToken": "drt-…" }

// 豆包 —— 最少只给 sessionid（F12 整段 Cookie 直接粘也可以）
{ "sessionid": "…" }

// DeepSeek —— 最少只给 accessToken（即 localStorage 的 userToken.value）
{ "accessToken": "mb5+…" }
```

> CodeBuddy / WorkBuddy 不用粘 JSON：跑 `login codebuddy <cn|intl> <昵称>` 后它会**打印一条授权链接**
> （国内发起自 `https://copilot.tencent.com`、落地 `www.codebuddy.cn`；国际版两端都是 `www.codebuddy.ai`），
> 浏览器完成登录后命令自动轮询取 token。

## 重启加载账号
```sh
docker restart reverse-proxy        # 容器名见 docker-compose.yml 的 container_name
```

## 验证
```sh
curl -s http://127.0.0.1:7863/healthz
# 用某账号专属密钥查该账号（只返回锁定的那一个号）
curl -s http://127.0.0.1:7863/status -H "Authorization: Bearer <该账号专属密钥>"
```

## dsh 自定义提供方
- API 地址：`http://127.0.0.1:7863/v1`
- API 协议：`openai-completions`
- API 密钥：该号的专属密钥（见 `账号令牌清单.md`；每个号一个条目配自己那把 key）
- 模型目录：点“获取可用模型”

## 多供应商（trae / qoder / doubao）

网关可在同一端口挂多个上游。每个供应商的账号都在 `auths/auth.json` 里，用 `account_keys` 为该账号配一把专属密钥（跑 `python3 gen_account_keys.py` 自动补齐）访问：

| provider | 用途 | auth.json 条目 | 获取脚本 |
| --- | --- | --- | --- |
| `trae` | Trae 国内版（chat_v3） | `trae_<n>` | `login trae`（无区域参数） |
| `qoder` | Qoder 国内版（COSY） | `qoder_<n>` | `scripts/qoder_bootstrap.py` |
| `doubao` | 豆包网页版（**仅对话，无工具调用**） | `doubao_<n>` | `scripts/doubao_session.py` |

部署三步：

```sh
# 1) 在宿主机用脚本生成凭证（示例：豆包，写进 auth.json 的 doubao_<n> 条目）
python3 scripts/doubao_session.py --cookie "sessionid=...; ttwid=..." --uid doubao-1

# 2) 为该账号补生成 account_keys 密钥 + 重建/重启加载账号
python3 gen_account_keys.py
docker compose up -d --build

# 3) 用该账号的专属密钥验证
curl -s http://127.0.0.1:7863/status -H "Authorization: Bearer <doubao 的账号密钥>"
```

> 密钥、模板、协议细节、排错表全部在 **PROVIDERS.md**。`account_keys` 的密钥必须全局唯一（同一把不得出现在两个账号下，启动会报冲突）。

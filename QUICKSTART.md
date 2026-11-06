# Reverse-Proxy 快速上手

三步：起容器 → 网页加账号 → 客户端填 `base_url`。详细配置与原理见 [README.md](README.md)。

## 1. 构建并启动

```bash
cp config.example.json config.json
mkdir -p auths data
docker compose up -d --build
docker logs reverse-proxy --tail 20      # 看到 listening on :7863 即成功
```

零账号也能启动；第一次部署 `auths/` 为空是正常的。

## 2. 网页添加账号

1. 打开 **http://127.0.0.1:7863/admin**
2. 口令取 `config.json` 的 `admin.password`（模板是 `123456`，**上线前必须改**）
3. 左侧「账号」→ 右上角「**+ 添加账号**」→ 选供应商 → 按提示授权或粘凭证
4. 完成后账号立即入池，页面同时给出该号的**专属密钥**（复制备用）

| 供应商 | 需要什么 |
|---|---|
| CodeBuddy 国内 / 国际 | 不用填：生成授权链接，浏览器登录后自动收号 |
| Trae | `sourceId`（= 请求头 `X-Cloudide-Session`） |
| Qoder | `pat`（`pt-…`，集成页可建） |
| 豆包工作 | F12 复制整段 Cookie（含 `sessionid`） |
| DeepSeek | localStorage 的 `userToken.value` |

凭证去哪拿、寿命多长、怎么续期 → [PROVIDERS.md](PROVIDERS.md)。

## 3. 客户端配置

| 项 | 值 |
|---|---|
| Base URL | `http://127.0.0.1:7863/v1` |
| API Key | 管理台「密钥」页里某个账号的专属密钥（锁死该号，不轮换） |
| 模型 | 直接拉 `GET /v1/models` |

dsh / Cherry Studio / 任意 OpenAI SDK：提供方类型选 `openai-completions`，地址填上面的 Base URL。

## 4. 验证

```bash
curl -s http://127.0.0.1:7863/healthz
curl -s http://127.0.0.1:7863/status -H "Authorization: Bearer <某号密钥>"
```

## 日常操作都在管理台

| 想做什么 | 在哪做 |
|---|---|
| 加号 / 重登 / 换凭证 | 「账号 → + 添加账号」（同 uid 会覆盖原槽位） |
| 看状态、续期、解除冷却、删除 | 「账号」→ 展开该号 |
| 生成 / 重置 / 复制专属密钥 | 「密钥」 |
| 改监听端口、调度、冷却等 | 「设置」（部分项需重启容器） |
| 看实时日志 | 「日志」 |

> 所有本地接口（`/healthz`、`/status`、`/admin/api/*`）的用途见
> [README.md 的「本地接口」](README.md#本地接口) 一节。

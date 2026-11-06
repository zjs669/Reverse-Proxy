# 40 · App 设计（三条路线共用）

不管走 A/B/C，App 的 UI 部分是一样的。这份文档描述**要做什么**，不含实现细节。

---

## 1. 页面：三个 Tab

```
┌────────────────────────────────────┐
│ Reverse-Proxy          [悬浮窗 ⬤]  │
├────────────────────────────────────┤
│  状态  │  配置  │  日志              │
├────────────────────────────────────┤
│ ● 运行中       127.0.0.1:7863      │
│ Key: sk-abc…  [复制]               │
│ 账号 3（健康 3 / 冷却 0 / 禁用 0）  │
│ 在途 0 · 粘性 1 · Redis noop       │
│                                    │
│ [启动] [停止] [复制 Base URL]      │
│ [自测 /healthz]  ☐ 开机自启        │
│ [忽略电池优化]                      │
└────────────────────────────────────┘
```

### Tab 1 · 状态
- 运行态、监听地址、API Key（可复制）
- 每 3s 拉一次 `GET /status`，显示账号池摘要
- 按钮：启动 / 停止 / 复制 Base URL / 自测 `/healthz` / 忽略电池优化
- 「开机自启」开关

> 为什么状态走 HTTP 而不是读内部变量：账号过滤（区域/供应商/账号级密钥）
> 只有一份实现，App 显示与外部客户端不会分叉。

### Tab 2 · 配置（自由度全靠这里）
两个**多行文本框**，等宽字体：

| 框 | 内容 |
|---|---|
| `config.json` | 完整网关配置，字段同仓库 `config.example.json` |
| `auths/auth.json` | 账号凭证，顶层 map |

每个框下面：`保存` / `恢复默认`（config）/ `导入文件`（auth）。
保存时用 `JSONObject` 校验；服务在跑就**自动重启**。

> **为什么不做表单**：配置 40+ 字段、嵌套 5 层，做表单既难维护又限制自由度。
> 文本框 + JSON 校验 + 内置模板最省事。

### Tab 3 · 日志
- 环形缓冲，最多 2000 行
- 顶部：清空 / 复制全部 / 行数 / 自动滚动开关

---

## 2. 悬浮窗

### 收起态（胶囊）

```
┌───────────────────┐
│ ● 127.0.0.1:7863  │   ← 绿点=运行，红点=停止
└───────────────────┘
```

### 展开态（卡片）

```
┌────────────────────────────┐
│ Reverse-Proxy    [ 收起 ]  │
│ 127.0.0.1:7863             │
│ 12:31 deepseek ok 812ms    │
│ 12:31 trae 429 cooldown    │
│ [ 停止服务 ]               │
└────────────────────────────┘
```

### 实现要点
- `WindowManager.LayoutParams`，`TYPE_APPLICATION_OVERLAY`（API 26+）
- `FLAG_NOT_FOCUSABLE`
- **纯 View（LinearLayout + TextView）**，不用 Compose —— 悬浮窗里塞 ComposeView 要额外管生命周期
- 拖动：`OnTouchListener` 里判断位移 > 8px 才算拖动，否则算点击
  （**不能**用 `setOnClickListener`：`OnTouchListener` 返回 false 时 click 不会派发）
- `WindowManager` 操作必须回主线程（`Handler(Looper.getMainLooper())`）

---

## 3. 服务与权限

### 前台服务
```xml
<service android:name=".ProxyService"
         android:foregroundServiceType="dataSync" />
```
Android 14 要求运行时也传类型：
```kotlin
startForeground(id, notif, ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC)
```

### 权限清单

| 权限 | 用途 | 类型 |
|---|---|---|
| `INTERNET` | 访问上游 + 本机回环 | 普通，装即授予 |
| `FOREGROUND_SERVICE` | 前台服务 | 普通 |
| `FOREGROUND_SERVICE_DATA_SYNC` | Android 14 必需 | 普通 |
| `POST_NOTIFICATIONS` | 前台服务通知（13+） | 运行时 |
| `SYSTEM_ALERT_WINDOW` | 悬浮窗 | 危险，需手动开 |
| `RECEIVE_BOOT_COMPLETED` | 开机自启（可选） | 普通 |

**不申请**：`READ/WRITE_EXTERNAL_STORAGE`、`MANAGE_EXTERNAL_STORAGE`、
`QUERY_ALL_PACKAGES`、位置、短信 —— 一个都不需要。
导入文件走 `ACTION_OPEN_DOCUMENT`，不需要存储权限。

### 明文 HTTP
客户端要连 `http://127.0.0.1:7863`，API 28+ 默认禁明文。用 `network_security_config`
**只对回环放行**（比全局 `usesCleartextTraffic="true"` 安全）：

```xml
<network-security-config>
    <base-config cleartextTrafficPermitted="false" />
    <domain-config cleartextTrafficPermitted="true">
        <domain includeSubdomains="false">127.0.0.1</domain>
        <domain includeSubdomains="false">localhost</domain>
    </domain-config>
</network-security-config>
```

---

## 4. 文件布局

```
/data/data/com.reverse.proxy/files/
├── config.json          ← 用户在 Tab2 编辑
├── auths/auth.json      ← 从 PC 粘贴
└── data/
    ├── state.json       ← 网关写（账号池运行态）
    └── deepseek_sessions.json
```

配置里的路径要写绝对路径（Android 上进程工作目录不是 App 目录）。

---

## 5. auth.json 长什么样

顶层 map，键是账号标签，值是凭证。嵌套形和扁平形都支持（`internal/auth/auth.go` 的 `Parse`）：

```jsonc
{
  "deepseek_1": {
    "provider": "deepseek",
    "auth":   { "accessToken": "<userToken>", "refreshToken": "", "expiresAt": 1790000000, "domain": "" },
    "account":{ "uid": "<uid>", "nickname": "我的号" },
    "extra":  {}
  },
  "trae_1": {
    "provider": "trae",
    "auth":   { "accessToken": "<jwt>", "expiresAt": 1790000000 },
    "account":{ "uid": "<uid>" },
    "extra":  { "cloudideToken": "...", "appId": "...", "appVersion": "..." }
  }
}
```

**最省事的路径**：PC 上跑 `./login <provider> <昵称>` 生成 `auths/auth.json`，
然后把整个文件内容复制到手机粘贴。各 provider 的 `extra` 必填字段见仓库 `PROVIDERS.md`。

---

## 6. 内置默认配置模板

监听回环、定时任务全关（手机上跑签到没意义还耗电）：

```jsonc
{
  "listen": "127.0.0.1:7863",
  "api_key": "<首次启动自动生成随机串>",
  "auth_file": "<filesDir>/auths/auth.json",
  "state_file": "<filesDir>/data/state.json",
  "deepseek": { "session_file": "<filesDir>/data/deepseek_sessions.json" },
  "schedule": {
    "checkin_enabled": false,
    "travel_enabled": false,
    "activity_enabled": false,
    "keepalive_enabled": false,
    "token_watch_enabled": true
  },
  "server": { "max_body_mb": 8 }
}
```

> `token_watch_enabled` 保留开启：短寿命凭证（Trae 只有 8h）靠它续期，
> 手机上挂着反而比 PC 更合适。

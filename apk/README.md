# Reverse-Proxy for Android

把 [Reverse-Proxy](https://github.com/zjs669/Reverse-Proxy) 网关打包成 Android App：**手机自己就是网关**，不需要云服务器。

App 只是个外壳 —— 真正的网关是随 APK 一起分发的 Go 二进制，由 App 以子进程方式拉起，监听 `127.0.0.1:7863`，对外提供 OpenAI 兼容接口。

---

## 功能

| 页面 | 能干什么 |
|---|---|
| **主页** | 看服务状态、复制 Base URL / 总密钥、自测连通性、开机自启、主题切换 |
| **账号** | 按供应商添加账号；每个账号显示**专属密钥**（一号一把）；查看该号可用模型；手动续期；删除 |
| **日志** | 网关 stdout 实时滚动，可复制 / 清空 / 跟随 |

### 两种密钥，别混用

- **总密钥**（主页）：可调度**全部**账号，按池轮换。客户端什么都不用改，随便换号用它。
- **专属密钥**（账号页展开）：一号一把、互不通用，请求**只走那一个号**，不参与轮换。要固定用某个号时用它。

专属密钥存在 `config.json` 的 `account_keys`（结构 `{账号标签: {uid: 密钥}}`），与 PC 侧 `gen_account_keys.py` 同口径（32 字节随机 → 64 位 hex），两边生成的可以互换。

---

## 支持的供应商

| 供应商 | 用户要给什么 | App 替你做什么 |
|---|---|---|
| **CodeBuddy / WorkBuddy**（含国际版） | 什么都不用给 | 点「生成授权链接」→ 上游返回一次性链接 → 浏览器登录 → **每 3 秒轮询收号**，拿到 uid 自动入库 |
| **Qoder** | `{ "pat": "pt-…" }` | 用 PAT 换 jobToken，自动补 uid 与机器指纹 |
| **Trae** | `{ "sourceId": "<X-Cloudide-Session>" }` | 用登录会话铸新 Cloud-IDE-JWT，自动解析 uid / sourceId |
| **DeepSeek** | `{ "accessToken": "mb5+…" }` | uid 缺省填 `deepseek-1` |
| **豆包** | `{ "sessionid": "…" }` | uid 缺省填 `doubao-work-1` |

**为什么 Trae 只给 sourceId 就够了**：Cloud-IDE-JWT 只有 8 小时寿命，而 `sourceId`（= 请求头 `X-Cloudide-Session`）是长期登录会话标识。`GetUserToken` 认它，所以既能当场换出 JWT，也能在之后无限次自动续期 —— 用户不用掐着 8 小时重粘。

**手动续期**：账号页展开卡片点「立即续期」。网关自己也会每 10 分钟巡检一次（`token_watch_enabled`），按钮只是让你不用等、并且能立刻验证会话是否还有效。

---

## 构建

### 前置

- JDK 17
- Android SDK（compileSdk 34）

**只想打包、不想编 Go？** 仓库里已经带了编好的网关二进制
（`app/src/main/jniLibs/arm64-v8a/libreverse-proxy.so`，10.4MB），
**直接跳到第 2 步** `assembleDebug` 就行，不需要 NDK、不需要 Go。

**要重新编网关**，另外需要：

- **Android NDK r26** —— 必需，原因见下一节
- Go 1.22+（或 Docker 里的 `golang:1.23-alpine`）

### 1. 编网关二进制（必须开 cgo）

```bash
cd <repo-root>
CC=$ANDROID_NDK/toolchains/llvm/prebuilt/<host>/bin/aarch64-linux-android21-clang \
CGO_ENABLED=1 GOOS=android GOARCH=arm64 \
go build -trimpath -ldflags="-s -w" -o apk/app/src/main/jniLibs/arm64-v8a/libreverse-proxy.so ./cmd/server
```

> `<host>` 是 `linux-x86_64` / `windows-x86_64` / `darwin-x86_64`，按你的构建机选。

**为什么文件名是 `lib*.so` 而不是可执行文件**：Android 10+ 禁止 App 在私有目录执行二进制（W`^`X 限制）。放进 `jniLibs` 后安装时会解压到 `nativeLibraryDir`，那个路径天然可执行、不需要 `chmod`。配合 `packaging { jniLibs { useLegacyPackaging = true } }` 才会真正落盘。

### 2. 打包 APK

```bash
cd apk
./gradlew assembleDebug     # 或 gradle assembleDebug
# 产物：app/build/outputs/apk/debug/app-debug.apk
```

### 3. 装机

```bash
adb install -r app/build/outputs/apk/debug/app-debug.apk
```

---

## ⚠️ 必须开 cgo：一个很贵的坑

**症状**：账号、密钥、网络全都正常，但所有请求都失败，日志里刷：

```
dial tcp: lookup api.trae.cn on [::1]:53: read udp [::1]:43413->[::1]:53:
read: connection refused
```

**根因**在 Go 标准库 `src/net/conf.go`：

```go
// DNS requests don't work on Android, so prefer the cgo resolver.
// Issue #10714.
case "android":
    return true
```

Go 官方要求 Android **必须用 cgo 解析器**。但 `CGO_ENABLED=0` 编出来的二进制 `cgoAvailable=false`，被迫退回纯 Go 解析器 → 去读 `/etc/resolv.conf` → Android 压根没这个文件 → 用内置默认值 `localhost:53` → 没人监听 → 解析失败。

**这跟账号、密钥、手机网络都无关，纯粹是二进制的构建方式。** 实测同一台手机：

- `ping api.trae.cn` 正常（走 netd）
- 网关报 `[::1]:53 connection refused`

**试过但不可行的方案**：在 App 里起 UDP 53 转发器，让 Go 落到 `localhost:53` 时命中。真机直接被拒：

```
bind 127.0.0.1:53 failed: EACCES (Permission denied)
```

Android 10+ 禁止非特权 App 绑定 <1024 端口，这条路在所有手机上都不成立。

**结论**：只能用 `CGO_ENABLED=1` + NDK 交叉编译。这样走的是 bionic 的 `getaddrinfo` → Android netd，**任何能上网的手机都能解析**，不依赖 resolv.conf、不需要 root。

代价：二进制变成动态链接（依赖系统 `libc.so` / `liblog.so`），不再是纯静态单文件。Android 5.0+ 都有，覆盖够用。

---

## App 存了哪些文件

全部在 `/data/user/0/com.reverse.proxy/files/`：

| 文件 | 内容 |
|---|---|
| `config.json` | 网关配置。`api_key`=总密钥，`account_keys`=各号专属密钥 |
| `auths/auth.json` | 账号凭证（顶层 map：标签 → 条目，兼容嵌套形/扁平形） |
| `runtime/config.json` | 实际喂给 Go 的配置（路径已改写成绝对路径） |
| `data/state.json` | 账号池运行态（网关自己写） |

**为什么要有 `runtime/`**：用户的 `config.json` 可能来自 PC，里面是 `"./auths/auth.json"` 这种相对路径。Android 上进程工作目录不是 App 目录，相对路径会解析错，所以启动前统一改写成绝对路径。

`auth.json` 顶层键就是账号标签（`codebuddy_1` / `qoder_1` …），也是账号卡片第二行显示的内容 —— 排查时能直接跟配置文件对上。

---

## 一键重编

```bash
# 1) 网关二进制（cgo 必需）
CC=$ANDROID_NDK/toolchains/llvm/prebuilt/$HOST/bin/aarch64-linux-android21-clang \
CGO_ENABLED=1 GOOS=android GOARCH=arm64 \
go build -trimpath -ldflags="-s -w" \
  -o apk/app/src/main/jniLibs/arm64-v8a/libreverse-proxy.so ./cmd/server

# 2) APK
cd apk && ./gradlew assembleDebug
```

---

## 目录结构

```
apk/
├── app/
│   ├── build.gradle.kts
│   └── src/main/
│       ├── AndroidManifest.xml
│       ├── java/com/reverse/proxy/
│       │   ├── MainActivity.kt      三个页面的 UI
│       │   ├── ProxyService.kt      前台服务，持有网关进程生命周期
│       │   ├── ProxyProcess.kt      找二进制 / 启动 / 读日志 / 停止
│       │   ├── FileStore.kt         files/ 下落点、config.json 读写
│       │   ├── AuthStore.kt         auth.json 解析、增删、uid 补齐、续期写回
│       │   ├── Providers.kt         各供应商最小凭证格式与引导文案
│       │   ├── ProvidersNet.kt      trae/qoder/codebuddy 的换 token 与续期
│       │   ├── AddAccountSheet.kt   添加账号弹窗（按供应商分流）
│       │   ├── GatewayApi.kt        回连自己 /status /models /healthz
│       │   ├── OverlayController.kt 悬浮窗
│       │   ├── Preference.kt        设置项（含开机自启）
│       │   ├── BootReceiver.kt      开机广播
│       │   ├── Theme.kt / UiKit.kt  主题与通用控件
│       │   └── ...
│       ├── jniLibs/arm64-v8a/libreverse-proxy.so   ← 网关二进制（10.4MB，已入库）
│       └── res/
├── docs/                            设计取舍与踩坑记录
├── build.gradle.kts
├── settings.gradle.kts
└── gradle.properties
```

---

## 已知限制

- **只打 arm64-v8a**。Go 的 `android/arm` / `android/386` 需要额外工具链，模拟器是 x86_64 也跑不了。真机都是 arm64，够用。
- **网关不提供续期 HTTP 接口**，只有内部定时任务。所以「立即续期」是 App 侧按同一套协议直连上游实现的，不经过网关。
- **`/status` 的账号数与实际不一致**：网关用总密钥查 `/status` 时，`provider` 为空按历史兼容只统计 codebuddy（`internal/pool/pick.go` 的 `providerMatch`），trae/qoder/doubao/deepseek 不在其中。App 主页显示的账号数用的是本地 `auth.json` 真实值，不受影响；但「健康 N」仍是 codebuddy 口径。

---

## 文档

| 文件 | 内容 |
|---|---|
| `docs/10-toolchain.md` | 工具链体检、Docker 编 Go 的实测命令 |
| `docs/20-exec-approach.md` | 可执行二进制方案（含 Android 10+ exec 限制的绕法） |
| `docs/30-gomobile.md` | gomobile bind 路线（本方案未采用） |
| `docs/40-app-design.md` | 页面 / 悬浮窗 / 服务设计 |
| `docs/50-faq.md` | 为什么不用 Termux、能不能上架、耗电等 |
| `docs/60-build-and-verify.md` | 构建步骤 + 真机验证记录 |

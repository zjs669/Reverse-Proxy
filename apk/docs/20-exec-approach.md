# 20 · 路线 A/C：纯 Go 二进制方案（不需要 NDK）

核心思路：**网关仍然是一个独立进程**，App 只负责把它拉起来、看住它、给它 UI。

```
APK
├── assets/reverse-proxy          ← Go 编出来的 ELF（arm64）
└── Kotlin 代码
     ├─ 首次启动：解压到 filesDir/reverse-proxy
     ├─ chmod 700
     ├─ Runtime.exec([path, "-config", cfgPath])
     ├─ 读 stdout/stderr → 日志页
     └─ 前台 Service 保活 + 悬浮窗
```

**好处**：不改仓库任何 Go 代码。`cmd/server` 是 `package main` 也能直接编译成可执行文件，
`-config` flag、环境变量、日志输出全部原样可用。

---

## 1. 路线 C：模拟器上先验证（今天就能做，最快）

雷电自带 root。目的是**先确认网关在 Android 环境里能跑通**，再谈打包。

### 1.1 开 root

```text
雷电模拟器 → 右上角设置 → 其他设置 → ROOT权限 → 打开 → 重启模拟器
```

### 1.2 编 x86_64 二进制

```bash
cd /d D:\a\Reverse-Proxy
docker run --rm -v "D:\a\Reverse-Proxy:/src" -w /src ^
  -e GOFLAGS=-mod=readonly -e GOPROXY=https://goproxy.cn,direct -e GOSUMDB=off ^
  golang:1.23-alpine sh -c "GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /src/apk/bin/reverse-proxy-linux-amd64 ./cmd/server"
```

### 1.3 推到模拟器并跑起来

```bash
set ADB=D:\spider\platform-tools\adb.exe
%ADB% connect 127.0.0.1:5555
%ADB% push apk\bin\reverse-proxy-linux-amd64 /data/local/tmp/reverse-proxy
%ADB% shell chmod 755 /data/local/tmp/reverse-proxy

:: 准备配置和凭证（先在本机写好，再 push）
%ADB% shell mkdir -p /data/local/tmp/rp/auths /data/local/tmp/rp/data
%ADB% push config.json /data/local/tmp/rp/config.json
%ADB% push auths\auth.json /data/local/tmp/rp/auths/auth.json

:: 跑起来（放后台，日志进文件）
%ADB% shell "cd /data/local/tmp/rp && nohup /data/local/tmp/reverse-proxy -config config.json > run.log 2>&1 &"
%ADB% shell "cat /data/local/tmp/rp/run.log"
```

### 1.4 验证

```bash
:: 模拟器内部自测
%ADB% shell "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:7863/healthz"

:: 宿主机通过 adb 端口转发访问
%ADB% forward tcp:7863 tcp:7863
curl -s http://127.0.0.1:7863/healthz
```

看到 200 就说明**网关在 Android 上完全可用**，剩下的只是"怎么打包成 App"。

> ⚠️ 模拟器可能没有 `curl`。那就用宿主机 `adb forward` 那条命令验证。

---

## 2. 路线 A：做成真机 APK

### 2.1 最大的坑：Android 10+ 禁止执行 App 私有目录里的文件

从 Android 10（API 29）开始，`/data/data/<pkg>/files/` 下的文件**不能 exec**（W^X 策略）：

```text
java.io.IOException: Cannot run program ".../files/reverse-proxy":
error=13, Permission denied
```

### 2.2 三种绕法（按可靠性排序）

#### 绕法 1：把二进制命名成 `lib*.so` 放进 `jniLibs`（最稳）

Android 允许执行 `nativeLibraryDir` 下的 `.so`（这是加载 native 库的正规路径）：

```text
app/src/main/jniLibs/arm64-v8a/libreverse-proxy.so   ← 其实是 ELF 可执行文件，改个名
```

```kotlin
val bin = File(applicationInfo.nativeLibraryDir, "libreverse-proxy.so")
// 这个路径天然可执行，不需要 chmod
ProcessBuilder(bin.absolutePath, "-config", cfgPath).start()
```

需要在 `build.gradle.kts` 里：
```kotlin
android {
    packaging { jniLibs { useLegacyPackaging = true } }  // 关键：让 .so 解压到磁盘
}
```

> `useLegacyPackaging = true` 是必须的。默认情况下 `.so` 会留在 APK 里不落盘，
> 那样就没有可执行文件路径可用。

#### 绕法 2：`android:extractNativeLibs="true"`

老一点的写法，效果同绕法 1 的 `useLegacyPackaging`。

#### 绕法 3：降到 `targetSdk = 28`

API 28 及以下没有这个限制。但 Google Play 已经不接受，自用无所谓。
**不推荐**：新系统上装可能被拦截。

### 2.3 用哪个

**绕法 1**。它同时满足：真机可用、不需要 root、不需要降 targetSdk。

---

## 3. 进程管理

### 3.1 启动

```kotlin
val pb = ProcessBuilder(
    bin.absolutePath,
    "-config", File(filesDir, "config.json").absolutePath,
)
    .redirectErrorStream(true)
    .directory(filesDir)          // 工作目录 = filesDir，配置里的相对路径才正常

val proc = pb.start()
// stdout 逐行读进日志页
```

### 3.2 停止

```kotlin
proc.destroy()                        // SIGTERM，网关会优雅停机（落盘 state）
if (!proc.waitFor(5, TimeUnit.SECONDS)) proc.destroyForcibly()
```

> `cmd/server` 里已经处理了 `SIGTERM`（`signal.NotifyContext`），
> 所以 `destroy()` 能触发它落盘 state.json 和 deepseek 会话。

### 3.3 保活

- 前台 Service（`FOREGROUND_SERVICE_DATA_SYNC`）
- `START_STICKY`
- 进程死了自动重启（在 Service 里监测 `proc.isAlive`）

---

## 4. 配置与凭证放哪

```
/data/data/com.reverse.proxy/files/
├── config.json          ← 用户在 App 里编辑（listen 建议 127.0.0.1:7863）
├── auths/auth.json      ← 从 PC 的 ./login 产物粘贴过来
└── data/state.json      ← 网关自己写
```

配置里的路径要写**绝对路径**（Android 上进程的工作目录不一定是 App 目录）：

```jsonc
{
  "listen": "127.0.0.1:7863",
  "auth_file": "/data/data/com.reverse.proxy/files/auths/auth.json",
  "state_file": "/data/data/com.reverse.proxy/files/data/state.json"
}
```

> 更省事的做法：App 启动时读一遍配置，把相对路径替换成 `filesDir` 下的绝对路径，
> 再写一份临时配置给 Go 用。这样用户粘贴 PC 上的配置也能直接跑。

---

## 5. 体积与内存

| 项 | 大小 |
|---|---|
| Go 二进制（arm64，`-s -w`） | 10.4MB（实测） |
| APK 总体（含 Compose UI） | ~15MB |
| 运行时内存（空闲） | ~30MB |
| 运行时内存（一个流式请求） | ~50MB |

比 gomobile 方案（AAR 也要 15-30MB）差不多，但**省掉了 NDK 和 Gradle native 打包的复杂度**。

---

## 6. 局限（必须知道）

| 限制 | 说明 | 影响 |
|---|---|---|
| 只能 arm64 | `android/arm` / `amd64` 要 cgo | 老 32 位手机不支持；模拟器不支持 |
| 不能用模拟器测 | 同上 | 真机测，或用路线 C 先验证 |
| `lib*.so` 命名 hack | 依赖 `useLegacyPackaging` | 升级 AGP 时要留意该选项 |
| 进程隔离 | 网关是独立进程，App 崩了它还在 | 需要 Service 管理生命周期 |

如果这些限制你不能接受 → 走 `docs/30-gomobile.md`（路线 B，装 NDK）。

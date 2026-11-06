# 30 · 路线 B：gomobile bind（需要 NDK）

> **只有这些情况才需要看这份文档**：
> - 你要在 x86_64 模拟器上跑
> - 你要同时支持 arm64 + armeabi-v7a
> - 你不接受「`lib*.so` 命名 + exec」那种 hack
>
> 否则走 `docs/20-exec-approach.md`，不用装 NDK。

---

## 1. 为什么这条路必须先改仓库

`cmd/server` 现在是 `package main`，而且 `main()` 里有 4 处 `log.Fatalf`：

```text
cmd/server/main.go:38   log.Fatalf("load config: %v", err)
cmd/server/main.go:56   log.Fatalf("load auths: %v", err)
cmd/server/main.go:62   log.Fatalf("no accounts loaded ...")
cmd/server/main.go:237  log.Fatalf("http: %v", err)
```

`log.Fatalf` = `os.Exit(1)` = **直接杀掉整个 App 进程**。
gomobile 是把 Go 编成 `.so` 加载进 App 进程，所以这几行必须改成返回 error。

### 需要的最小改动

| 新文件 | 来源 | 改动 |
|---|---|---|
| `internal/gateway/config.go` | `cmd/server/config.go` | 只改 `package main` → `package gateway` |
| `internal/gateway/gateway.go` | `cmd/server/main.go` 的 `main()` | `log.Fatalf` → `return err`；支持 `Start(ctx)`/`Stop()` |
| `internal/gateway/health.go` | `cmd/server/main.go` 尾部 | 原样平移 |
| `cmd/server/main.go` | 重写 | 只剩 flag 解析 + 调 gateway |
| `androidlib/` | 新增 | gomobile 的导出函数（`//go:build android`） |

**协议、路由、鉴权、调度逻辑一行不改**，只搬位置 + 换错误处理。

> 上一轮我已经做过这套改动并验证通过（`go build` / `go test` / `docker build` 全绿），
> 但你要求还原，所以 `main` 分支现在是干净的。需要时再重做一遍即可。

### 另一个必须改的点：`pool` 的 flusher 泄漏

`internal/pool/persist.go` 的 `startFlusher()` 起了一个**永不退出**的 goroutine：

```go
go func() {
    t := time.NewTicker(interval)
    for range t.C { ... }   // ← 没有退出条件
}()
```

CLI 一辈子只 Start 一次，无所谓；APK 反复 Start/Stop 会每次泄漏一个 ticker。
需要加 `Pool.Close()`，把循环改成 `select { case <-stop: return; case <-t.C: ... }`。

---

## 2. 安装 NDK

```bash
:: 1. 装 SDK 命令行工具（见 docs/10-toolchain.md §4）
:: 2. 装 NDK（走清华镜像）
set SDK_TEST_BASE_URL=https://mirrors.tuna.tsinghua.edu.cn/android/repository/
sdkmanager --sdk_root=D:\Android\sdk "ndk;26.3.11579264"

:: 3. 验证
dir D:\Android\sdk\ndk\26.3.11579264\toolchains\llvm\prebuilt\windows-x86_64\bin\aarch64-linux-android21-clang.cmd
```

约 1GB 下载。**这是这条路唯一的硬门槛。**

---

## 3. 生成 AAR

```bash
:: 装 gomobile（需要本机有 Go）
go install golang.org/x/mobile/cmd/gomobile@latest
go install golang.org/x/mobile/cmd/gobind@latest
gomobile init -ndk D:\Android\sdk\ndk\26.3.11579264

:: 生成（在仓库根目录）
cd /d D:\a\Reverse-Proxy
gomobile bind ^
  -target=android/arm64,android/amd64 ^
  -androidapi 24 ^
  -o apk/app/libs/gateway.aar ^
  ./androidlib
```

产物 `gateway.aar` 里含：
- `jni/arm64-v8a/libgojni.so`
- `jni/x86_64/libgojni.so`
- `classes.jar`（Java 类 `androidlib.Androidlib`）

### 用 Docker 跑 gomobile（本机不装 Go）

```bash
docker run --rm -v "D:\a\Reverse-Proxy:/src" -v "D:\Android\sdk:/sdk" -w /src ^
  -e ANDROID_HOME=/sdk -e ANDROID_NDK_HOME=/sdk/ndk/26.3.11579264 ^
  -e GOPROXY=https://goproxy.cn,direct -e GOSUMDB=off ^
  golang:1.23-bookworm sh -c "
    apt-get update -qq && apt-get install -y -qq unzip >/dev/null
    go install golang.org/x/mobile/cmd/gomobile@latest
    go install golang.org/x/mobile/cmd/gobind@latest
    export PATH=\$PATH:/root/go/bin
    gomobile init -ndk /sdk/ndk/26.3.11579264
    gomobile bind -target=android/arm64,android/amd64 -androidapi 24 -o /src/apk/app/libs/gateway.aar ./androidlib
  "
```

> `golang:1.23-alpine` 不行（gomobile 需要 glibc 下的工具链），用 `bookworm` 变体。

---

## 4. Go 侧门面（androidlib）

```go
//go:build android

package androidlib

// 只导出 5 个函数；gomobile bind 自动生成 androidlib.Androidlib 类
func Start(configPath, authFile, stateFile, listen string) error
func Stop() error
func Addr() string
func Running() bool
func DrainLogs() string   // 取出并清空 2000 行环形日志缓冲
```

要点：
- `//go:build android`：PC 上 `go build ./...` 不编译它，仓库测试不受影响
- 日志：`log.SetOutput(ring)`，App 每 500ms 拉一次 `DrainLogs()`
  （**不要**用 gomobile 的 Java 回调：跨线程 + 生命周期难管）

---

## 5. Gradle 打包

```kotlin
// app/build.gradle.kts
android {
    defaultConfig {
        minSdk = 24
        ndk { abiFilters += listOf("arm64-v8a", "x86_64") }
    }
}
dependencies {
    implementation(files("libs/gateway.aar"))
}
```

```kotlin
// 调用
Androidlib.start(configPath, authPath, statePath, "127.0.0.1:7863")
```

---

## 6. 验收

| # | 项 | 怎么验 |
|---|---|---|
| 1 | `go build ./...` 通过 | Docker 里跑 |
| 2 | `go test ./...` 全绿 | Docker 里跑 |
| 3 | `gomobile bind` 产出 AAR | 看 `apk/app/libs/gateway.aar` |
| 4 | APK 装上能启动 | `adb install` |
| 5 | 手机浏览器开 `127.0.0.1:7863/healthz` 返回 200 | 真机 |
| 6 | Stop 后端口释放，再 Start 不报错 | 反复点启动/停止 |

---

## 7. 与路线 A 的取舍

| | 路线 A（exec） | 路线 B（gomobile） |
|---|---|---|
| 需要 NDK | ❌ | ✅ ~1GB |
| 需要改仓库 Go 代码 | ❌ | ✅ 约 150 行搬位置 |
| 模拟器可测 | ❌（amd64 要 cgo） | ✅ |
| 32 位 ARM 支持 | ❌ | ✅ |
| 构建速度 | 快（几秒） | 慢（首次 3-6 分钟） |
| 稳定性 | 依赖 `lib*.so` hack | 官方支持 |
| 进程模型 | 独立进程 | 同进程（崩了 App 一起崩） |

**如果只是想自己用、手机是 arm64、能接受只在真机测 → 路线 A 更省事。**

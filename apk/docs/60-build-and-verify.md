# 60 · 构建与真机验证记录

> 全部步骤都在这台机器上**实际跑过**，不是照抄文档。

## 0. 环境（已就绪）

| 组件 | 路径 | 状态 |
|---|---|---|
| JDK 17 | `D:\spider\jdk-17.0.20.1+1` | ✅ 原有 |
| Android SDK | `D:\Android\sdk` | ✅ 本次安装 |
| ├ platform-tools | `D:\Android\sdk\platform-tools` | ✅ |
| ├ platforms;android-34 | `D:\Android\sdk\platforms\android-34` | ✅ |
| └ build-tools;34.0.0 | `D:\Android\sdk\build-tools\34.0.0` | ✅ |
| Gradle 8.7 | `D:\Android\gradle-8.7` | ✅ 本次安装 |
| Go | **未装，用 Docker** | ✅ `golang:1.23-alpine` |

**没有装 NDK** —— 路线 A 不需要（见 `docs/10-toolchain.md`）。

### 安装命令（如需重装）

```bash
# 1. SDK 命令行工具
curl -L -o D:\Android\cmdline-tools.zip ^
  https://dl.google.com/android/repository/commandlinetools-win-11076708_latest.zip
powershell -NoProfile -Command "Expand-Archive D:\Android\cmdline-tools.zip D:\Android\tmpct -Force"
:: 解压后移到 sdk\cmdline-tools\latest
```

```bash
:: 2. 接受 license（必须逐条喂 y，--licenses 一次性回车不算接受）
set ANDROID_HOME=D:\Android\sdk
set JAVA_HOME=D:\spider\jdk-17.0.20.1+1
echo y>D:\Android\yes.txt & for /L %i in (1,1,50) do @echo y>>D:\Android\yes.txt
D:\Android\sdk\cmdline-tools\latest\bin\sdkmanager.bat --sdk_root=D:\Android\sdk --licenses < D:\Android\yes.txt

:: 3. 装包
D:\Android\sdk\cmdline-tools\latest\bin\sdkmanager.bat --sdk_root=D:\Android\sdk ^
  "platform-tools" "platforms;android-34" "build-tools;34.0.0"

:: 4. Gradle（官方源超时，用腾讯镜像）
curl -L -o D:\Android\gradle.zip https://mirrors.cloud.tencent.com/gradle/gradle-8.7-bin.zip
powershell -NoProfile -Command "Expand-Archive D:\Android\gradle.zip D:\Android -Force"
```

---

## 1. 编 Go 二进制（Docker，不需要本机装 Go）

```bash
cd /d D:\a\Reverse-Proxy
docker run --rm -v "D:\a\Reverse-Proxy:/src" -w /src ^
  -e GOFLAGS=-mod=readonly ^
  -e GOPROXY=https://goproxy.cn,direct -e GOSUMDB=off ^
  golang:1.23-alpine sh -c ^
  "GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /src/apk/bin/reverse-proxy-android-arm64 ./cmd/server"
```

产物：`apk/bin/reverse-proxy-android-arm64`，**10.4MB**，ELF `machine=0xb7`（AArch64）。

> ⚠️ 必须用 `-mod=readonly`。用 `-mod=mod` 会改写仓库的 `go.mod`（把 indirect 依赖提成直接依赖）。

---

## 2. 放进 jniLibs（关键一步）

```bash
mkdir -p apk/app/src/main/jniLibs/arm64-v8a
cp apk/bin/reverse-proxy-android-arm64 apk/app/src/main/jniLibs/arm64-v8a/libreverse-proxy.so
```

**为什么改名成 `lib*.so`**：Android 只保证 `nativeLibraryDir` 下的文件可执行。
命名成 `lib*.so` + `useLegacyPackaging = true`，安装后系统会把它们解压到
`/data/app/.../lib/arm64/`，那个路径 `-rwxr-xr-x`，可直接 exec。

---

## 3. 编 APK

```bash
cd /d D:\a\Reverse-Proxy\apk
set JAVA_HOME=D:\spider\jdk-17.0.20.1+1
set ANDROID_HOME=D:\Android\sdk
set ANDROID_SDK_ROOT=D:\Android\sdk
D:\Android\gradle-8.7\bin\gradle.bat assembleDebug --no-daemon
```

产物：`apk/app/build/outputs/apk/debug/app-debug.apk` —— **12.4MB**

构建耗时：首次约 1 分 8 秒（含依赖下载）。

### 踩过的坑

| 问题 | 原因 | 解决 |
|---|---|---|
| `The option 'android.disableAutomaticComponentCreation' is deprecated` | AGP 8 移除了该属性 | 从 `gradle.properties` 删掉 |
| `sdkmanager --licenses` 显示 OK 但没装上包 | 交互式 license 需要逐条输入 y，回车/超时不算接受 | 用 `< yes.txt` 喂 50 个 y |
| `services.gradle.org` 下载超时 | 国内网络 | 用 `mirrors.cloud.tencent.com/gradle/`（12 秒下完 128MB） |
| `maven.google.com` 连不上 | 同上 | `settings.gradle.kts` 里配了阿里云镜像（338ms 响应） |

---

## 4. 真机验证（Pixel 2 / Android 11 / arm64-v8a）

### 4.1 安装

```bash
set ADB=D:\spider\platform-tools\adb.exe
%ADB% install -r apk\app\build\outputs\apk\debug\app-debug.apk
%ADB% shell am start -n com.reverse.proxy/.MainActivity
```

结果：`Success`，进程 `com.reverse.proxy` 正常启动，无崩溃。

### 4.2 确认 .so 已解压且可执行

```bash
%ADB% shell "ls -l /data/app/*/com.reverse.proxy*/lib/arm64/"
```

```text
-rwxr-xr-x 1 system system 10944833 libreverse-proxy.so    ← 可执行位在
```

### 4.3 验证 exec 可用（以 App 身份）

```bash
%ADB% shell "run-as com.reverse.proxy sh -c '<LIB路径> -h'"
```

```text
Usage of .../libreverse-proxy.so:
  -config string
    	path to config json (default "config.json")
```

**意外发现**：这台 Android 11 上 `filesDir` 里的文件也能 exec（`W^X` 未生效）。
但 `nativeLibraryDir` 方案在更高版本更稳，所以代码仍走那条路。

### 4.4 完整启动链路

以 App 身份、工作目录 = `filesDir`、配置 = `files/runtime/config.json`：

```bash
%ADB% shell "run-as com.reverse.proxy sh -c 'cd files && <LIB> -config runtime/config.json > runtime/out.log 2>&1 &'"
```

日志：

```text
loaded 1 account(s) from /data/data/com.reverse.proxy/files/auths/auth.json
[redisstore] upstash 未配置，进入纯内存模式（Noop 降级）
Reverse-Proxy listening on 127.0.0.1:7863 (api_key=true, ...)
```

### 4.5 HTTP 端点

```bash
%ADB% forward tcp:17863 tcp:7863
curl -H "Authorization: Bearer sk-apptest" http://127.0.0.1:17863/status
```

```text
/healthz     -> 200
/status      -> 200
/v1/models   -> 200
```

---

## 5. 验证结论

| 验证项 | 结果 |
|---|---|
| Go 二进制能在 Android 11 上运行 | ✅ |
| `lib*.so` 解压到 nativeLibraryDir 且可执行 | ✅ |
| 以 App 身份在 filesDir 下启动网关 | ✅ |
| 网关能读 App 私有目录的 auth.json | ✅ |
| 监听 127.0.0.1:7863 成功 | ✅ |
| `/healthz` `/status` `/v1/models` 返回 200 | ✅ |
| APK 安装、启动、无崩溃 | ✅ |

**未验证**（需要你解锁手机操作 UI）：
- 点「启动」按钮 → 前台服务拉起进程（UI 交互）
- 悬浮窗显示
- 日志页实时输出

---

## 6. 下一步：你自己验证 UI

手机锁着，我没法点按钮。解锁后：

1. 打开 App「Reverse-Proxy」
2. 「配置」Tab → 把 PC 上 `auths/auth.json` 的内容粘进第二个框 → 点「保存」
3. 回「状态」Tab → 点「启动」
4. 应看到 `● 运行中` + `127.0.0.1:7863` + 账号池摘要
5. 点「自测 /healthz」→ 显示 `200 ✓`

有问题就 `adb logcat -s ProxyProcess:V ProxyService:V` 看日志。

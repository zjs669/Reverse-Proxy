# 10 · 工具链：现状 + Docker 构建 Go

## 1. 你的机器现状（实测）

| 组件 | 状态 | 详情 |
|---|---|---|
| Docker | ✅ 29.9.2 | 可直接用，已有 `golang:1.23-alpine` 镜像 |
| JDK | ✅ 17.0.20.1 | `D:\spider\jdk-17.0.20.1+1` |
| adb | ✅ | `D:\spider\platform-tools\adb.exe` |
| 模拟器 | ✅ 雷电 14 | 已安装但**未启动**，x86_64，Android 9 |
| Go | ❌ 未装 | **但不需要装**——用 Docker |
| Android SDK | ❌ 未装 | 路线 A/C 不需要；路线 B 需要 |
| NDK | ❌ 未装 | 只有路线 B 需要 |

### Docker 镜像源（你的机器已配好）

```text
docker.xuanyuan.me       401（正常，服务在线）2.7s
docker.1ms.run           401（正常）          1.2s
docker.m.daocloud.io     401（正常）          0.2s  ← 最快
```

拉镜像时优先用 `docker.m.daocloud.io/library/<镜像>`。

---

## 2. 用 Docker 构建 Go（不需要本机装 Go）

### 2.1 真机（arm64）—— 路线 A 用这个

```bash
docker run --rm -v "D:\a\Reverse-Proxy:/src" -w /src \
  -e GOFLAGS=-mod=readonly \
  -e GOPROXY=https://goproxy.cn,direct -e GOSUMDB=off \
  golang:1.23-alpine \
  sh -c "GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/reverse-proxy ./cmd/server"
```

产物：`./reverse-proxy`（约 10.4MB ELF，`e_machine=0xb7` = AArch64）

> **用 `-mod=readonly`（已实测）**：`GOFLAGS=-mod=readonly` 会改写 `go.mod`（把 indirect 依赖
> 提成直接依赖），污染工作区。改成 `-e GOFLAGS=-mod=readonly` 同样能编过，且 `go.mod` 不变。
> 下面所有命令都已用 readonly。

### 2.2 模拟器（x86_64）—— 路线 C 用这个

```bash
docker run --rm -v "D:\a\Reverse-Proxy:/src" -w /src \
  -e GOFLAGS=-mod=readonly -e GOPROXY=https://goproxy.cn,direct -e GOSUMDB=off \
  golang:1.23-alpine \
  sh -c "GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/reverse-proxy ./cmd/server"
```

产物约 9.7MB，**完全静态**（无动态链接依赖），可直接在 Android x86_64 上跑。

### 2.3 一次编多个架构

```bash
docker run --rm -v "D:\a\Reverse-Proxy:/src" -w /src \
  -e GOFLAGS=-mod=readonly -e GOPROXY=https://goproxy.cn,direct -e GOSUMDB=off \
  golang:1.23-alpine sh -c '
    set -e
    for pair in "android arm64" "linux amd64" "linux arm64"; do
      set -- $pair
      GOOS=$1 GOARCH=$2 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
        -o /src/apk/bin/reverse-proxy-$1-$2 ./cmd/server
      echo "built $1/$2"
    done
  '
```

---

## 3. 实测过的编译矩阵

| GOOS/GOARCH | CGO=0 | 产物 | 用途 |
|---|---|---|---|
| `android/arm64` | ✅ | 10.4MB ELF | 真机 APK（路线 A） |
| `android/arm` | ❌ | — | 老 32 位 ARM，需 NDK |
| `android/386` | ❌ | — | 需 NDK |
| `android/amd64` | ❌ | — | **模拟器默认架构，需 NDK** |
| `linux/amd64` | ✅ | 9.7MB 静态 | x86_64 模拟器（路线 C） |
| `linux/arm64` | ✅ | ~10MB | 备用 |

失败信息固定为：
```text
android/<arch> requires external (cgo) linking, but cgo is not enabled
```

**这是 Go 的硬性限制**：除 arm64 外，Android 目标的链接器只有 cgo 实现。
所以「完全不装 NDK」这条路，只有 **arm64 真机** 和 **linux 交叉编译** 两种走法。

---

## 4. 如果以后要装 Android SDK / NDK

只有路线 B 需要。装法（用镜像源加速）：

```bash
# 1. 下载命令行工具
curl -O https://dl.google.com/android/repository/commandlinetools-win-11076708_latest.zip

# 2. 解压到 D:\Android\sdk\cmdline-tools\latest
# 3. 装组件（走清华镜像会快很多）
set SDK_TEST_BASE_URL=https://mirrors.tuna.tsinghua.edu.cn/android/repository/
sdkmanager --sdk_root=D:\Android\sdk "platform-tools" "platforms;android-34" "build-tools;34.0.0" "ndk;26.3.11579264"
sdkmanager --sdk_root=D:\Android\sdk --licenses
```

> 国内直连 `dl.google.com` 通常很慢或超时。镜像源：
> - 清华：`https://mirrors.tuna.tsinghua.edu.cn/android/repository/`
> - 腾讯：`https://mirrors.cloud.tencent.com/AndroidSDK/`

---

## 5. Gradle 依赖（如果走路线 B）

Gradle 要拉 AGP / Compose，走 Google Maven。国内建议在
`%USERPROFILE%\.gradle\init.gradle` 里配阿里云镜像：

```groovy
allprojects {
    repositories {
        maven { url 'https://maven.aliyun.com/repository/google' }
        maven { url 'https://maven.aliyun.com/repository/central' }
        maven { url 'https://maven.aliyun.com/repository/gradle-plugin' }
        google()
        mavenCentral()
    }
}
```

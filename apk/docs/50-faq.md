# 50 · FAQ

## 为什么不用 Termux？

Termux 确实能直接跑 Go 二进制，但：

- 要用户自己装 Termux、装包、敲命令 —— **不是 APK**
- Termux 的 `proot` 环境和 App 沙箱不通，做不了悬浮窗、前台服务
- 你明确要的是「APK」，Termux 是另一个 App

不过 Termux 很适合**验证阶段**：想快速确认网关在 Android 上能不能跑，
`pkg install golang && go build` 比我们绕一大圈快得多。但它不能交付成 APK。

---

## 能不能上架 / 分发？

不建议，也没必要：

- 这个网关是「把网页端会话反向代理成 OpenAI 接口」，各家服务条款未必允许
- 需要用户提供自己的账号凭证
- 仓库 README 已声明「仅限个人自用」

自己用就 `adb install` 或直接拷 APK，不用签名发布。
真要 release 签名，`keytool` 生成一个 jks 即可（`D:\spider\jdk-17.0.20.1+1\bin\keytool.exe`）。

---

## 手机上跑会不会很耗电？

看使用模式：

| 场景 | 耗电 |
|---|---|
| 服务开着但没请求 | 很低（Go 的 `net/http` 空闲几乎不占 CPU） |
| 定时任务全开（签到/活跃/旅行） | 明显（按小时打上游） |
| 流式对话中 | 正常（网络 + CPU） |

**建议**：手机上把 `schedule` 里四个 `*_enabled` 全设 `false`，只留
`token_watch_enabled: true`（续期需要，频率低）。

另外国产 ROM 会杀后台，需要：
1. 状态页点「忽略电池优化」
2. 系统设置里把 App 电池设为「无限制」
3. 锁定最近任务列表里的 App

---

## 为什么模拟器这么麻烦？

因为 **Go 只在 `android/arm64` 上支持纯 Go 链接**：

```text
android/arm64   CGO=0  ✅
android/arm     CGO=0  ❌ requires external (cgo) linking
android/386     CGO=0  ❌
android/amd64   CGO=0  ❌   ← 模拟器就是这个
```

模拟器（雷电/MuMu/BlueStacks）都是 **x86_64**，所以：
- 走路线 A（纯 Go exec）→ 模拟器上装不了
- 走路线 B（gomobile）→ 可以，但要 NDK
- 走路线 C（root + linux/amd64 二进制）→ 可以，但不是 APK

**所以：如果只在真机用，完全不用管模拟器，也不用装 NDK。**

---

## Docker 能完全替代本机装 Go 吗？

**能，Go 部分完全可以。** 已实测：

```text
docker run golang:1.23-alpine → go build ./...            ✅
docker run golang:1.23-alpine → go test ./...             ✅
GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build          ✅
```

**不能**替代的部分：
- 路线 A 的 APK 打包（Gradle）→ 需要 JDK，你已有（`D:\spider\jdk-17.0.20.1+1`）
- 路线 B 的 `gomobile bind` → 需要 NDK（可以用 Docker 挂载 NDK 目录跑，但 NDK 本身要下）

> 我试过拉 `eclipse-temurin:17-jdk-alpine`，100 秒没拉完就超时了。
> 大镜像首次拉取确实慢，配好镜像源后会好一些，但仍建议**优先用你本机的 JDK 17**。

---

## 能不能不打包 APK，直接用 adb 起服务？

可以，这就是**路线 C**。适合：

- 先验证网关在 Android 上能不能跑通
- 你自己的手机长期连着 adb
- 不需要悬浮窗/UI

```bash
adb push reverse-proxy-linux-amd64 /data/local/tmp/
adb shell chmod 755 /data/local/tmp/reverse-proxy-linux-amd64
adb shell "/data/local/tmp/reverse-proxy-linux-amd64 -config /data/local/tmp/config.json"
```

但拔了 USB 就没了，不是长期方案。

---

## 配置改了要重新打包吗？

**不用。** 配置和凭证都在 App 私有目录的 JSON 文件里，
在 App 的「配置」Tab 里改完点保存就生效（服务会自动重启）。

只有改了 **Go 代码**才需要重新编译二进制、重新打包 APK。

---

## 账号凭证怎么从 PC 弄到手机？

```bash
# PC 上（在仓库根目录）
./login deepseek 我的号
# 或 docker exec -it reverse-proxy /app/login deepseek 我的号
```

生成的文件在 `auths/auth.json`。打开它，全选复制，
粘贴到手机 App 的「配置」Tab 的 `auths/auth.json` 框里，点保存。

> 也可以 `adb push auths/auth.json /sdcard/`，然后 App 里用「导入文件」选它。
> 导入走 `ACTION_OPEN_DOCUMENT`，不需要存储权限。

---

## 端口 7863 冲突了怎么办？

改 `config.json` 的 `listen`：

```jsonc
{ "listen": "127.0.0.1:17863" }
```

保存后服务自动重启，状态页显示的地址也会跟着变。

---

## 手机上的网关，PC 怎么连？

默认只监听 `127.0.0.1`，PC 连不上。两个办法：

**A. adb 端口转发（推荐，不改配置）**
```bash
adb forward tcp:7863 tcp:7863
curl http://127.0.0.1:7863/healthz
```

**B. 改成对局域网开放（有风险）**
```jsonc
{ "listen": "0.0.0.0:7863" }
```
然后 PC 用手机 IP 访问。

> ⚠️ `0.0.0.0` 意味着同一 WiFi 下谁都能用你的账号池。除非 `api_key` 足够强，否则别开。

---

## 这个 APK 能替代 Docker 部署吗？

能，但不能同时用**同一批账号**：

同一个 `auth.json` 被两个网关同时轮换，会话粘性和冷却状态各自为政，
容易出现「这个网关说 429、那个说正常」。

要么手机跑、要么服务器跑，选一个。

package com.reverse.proxy

import android.content.Context
import android.util.Log
import java.io.BufferedReader
import java.io.File
import java.io.InputStreamReader
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean

/**
 * 网关进程的封装：找可执行文件、启动、读日志、停止。
 *
 * 关键点（Android 10+ 会踩的坑，见 docs/20-exec-approach.md §2）：
 * App 私有目录（filesDir）下的文件**不能 exec**。所以 Go 二进制以 lib*.so 命名
 * 放进 jniLibs，安装后落在 nativeLibraryDir —— 那个路径天然可执行，且不需要 chmod。
 */
object ProxyProcess {

    private const val TAG = "ProxyProcess"

    /** jniLibs 里的文件名。内容其实是 Go 编出来的 ELF 可执行文件。 */
    private const val BIN_NAME = "libreverse-proxy.so"

    private var proc: Process? = null
    private val stopping = AtomicBoolean(false)
    @Volatile private var reader: Thread? = null

    /** 可执行文件的绝对路径；不存在返回 null。 */
    fun binaryPath(ctx: Context): String? {
        val f = File(ctx.applicationInfo.nativeLibraryDir, BIN_NAME)
        return if (f.isFile) f.absolutePath else null
    }

    fun isRunning(): Boolean {
        val p = proc ?: return false
        // Process.isAlive 需要 API 26；minSdk 24 用 exitValue 判断（抛异常=还活着）。
        return try {
            p.exitValue()
            false
        } catch (e: IllegalThreadStateException) {
            true
        }
    }

    /**
     * 启动网关。返回 null 表示成功，否则返回错误信息。
     *
     * 同步等到端口可用或进程退出，这样 UI 能立刻知道成功还是失败，
     * 而不是先显示"运行中"再神秘失败。
     */
    fun start(ctx: Context, configFile: File, listenPort: Int): String? {
        if (isRunning()) return null

        val bin = binaryPath(ctx) ?: return "找不到可执行文件：nativeLibraryDir/$BIN_NAME\n" +
                "（APK 里 jniLibs/arm64-v8a/$BIN_NAME 是否缺失？）"

        stopping.set(false)
        val p = try {
            ProcessBuilder(bin, "-config", configFile.absolutePath)
                // 工作目录设成 filesDir：配置里的相对路径（如果有）才解析得对
                .directory(ctx.filesDir)
                .redirectErrorStream(true)
                .start()
        } catch (t: Throwable) {
            Log.e(TAG, "start failed", t)
            return "启动失败：${t.message}"
        }
        proc = p
        startLogPump(p)

        // 轮询端口：最多等 20 秒。进程提前退出就立刻返回它的错误。
        // 20s 是因为真机冷启动（首次解压 10MB 二进制 + 6 个账号加载）实测要 3-5s，
        // 慢机上 8s 会误报超时；这里是后台线程，等久点不影响 UI。
        val deadline = System.currentTimeMillis() + 20000
        while (System.currentTimeMillis() < deadline) {
            if (!isRunning()) {
                val code = runCatching { p.exitValue() }.getOrDefault(-1)
                proc = null
                return "网关进程已退出（exit=$code），看日志页排查"
            }
            if (portOpen(listenPort)) return null
            Thread.sleep(200)
        }
        // 进程还活着但端口没就绪：不当失败处理——startWatching 会在 2s 内把它纠正成运行中，
        // 而这里若返回错误，UI 会先闪一条"启动失败"再变running，反而误导。
        return if (isRunning()) null else "启动超时：20 秒内 127.0.0.1:$listenPort 未就绪"
    }

    /** 优雅停机：SIGTERM → 等 5s → 强杀。网关收到 SIGTERM 会落盘 state。 */
    fun stop() {
        val p = proc ?: return
        stopping.set(true)
        runCatching { p.destroy() }
        val dead = runCatching { p.waitFor(5, TimeUnit.SECONDS) }.getOrDefault(false)
        if (!dead) runCatching { p.destroyForcibly() }
        proc = null
        reader?.interrupt()
        reader = null
    }

    /** 逐行读 stdout/stderr → LogBus。进程退出时补一条日志。 */
    private fun startLogPump(p: Process) {
        reader = Thread({
            try {
                BufferedReader(InputStreamReader(p.inputStream)).useLines { lines ->
                    for (line in lines) {
                        LogBus.append(line)
                        Log.i(TAG, line)
                    }
                }
            } catch (t: Throwable) {
                if (!stopping.get()) LogBus.append("日志读取中断：${t.message}")
            }
            if (!stopping.get()) {
                val code = runCatching { p.exitValue() }.getOrDefault(-1)
                LogBus.append("网关进程已退出（exit=$code）")
            }
        }, "rp-log").apply { isDaemon = true; start() }
    }

    private fun portOpen(port: Int): Boolean = try {
        java.net.Socket().use { s ->
            s.connect(java.net.InetSocketAddress("127.0.0.1", port), 300)
            true
        }
    } catch (t: Throwable) {
        false
    }
}

/** 极简日志总线：Go 进程 stdout → 内存环形缓冲 → Compose 日志页。 */
object LogBus {
    private const val MAX_LINES = 2000
    private val _lines = kotlinx.coroutines.flow.MutableStateFlow<List<String>>(emptyList())
    val lines: kotlinx.coroutines.flow.StateFlow<List<String>> = _lines

    fun append(line: String) {
        if (line.isBlank()) return
        val cur = _lines.value
        val next = if (cur.size >= MAX_LINES) cur.subList(cur.size - MAX_LINES + 1, cur.size) + line else cur + line
        _lines.value = next
    }

    fun clear() { _lines.value = emptyList() }

    fun text(): String = _lines.value.joinToString("\n")
}
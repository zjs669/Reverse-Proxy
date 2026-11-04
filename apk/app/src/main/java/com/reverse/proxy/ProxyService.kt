package com.reverse.proxy

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.graphics.drawable.Icon
import android.os.Build
import android.os.IBinder
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock

/**
 * 前台服务：持有网关进程的生命周期。
 *
 * 为什么需要前台服务：Android 会在后台杀普通进程。前台服务 + 常驻通知
 * 是唯一能长期跑后台网络服务的正规做法（Android 14 起还必须声明 foregroundServiceType）。
 */
class ProxyService : Service() {

    data class State(
        val running: Boolean = false,
        val addr: String = "",
        val lastError: String = "",
    )

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private var watchJob: Job? = null
    // 启动/停止/重启互斥：用户连点两下「启动」、或停在启动中就点「停止」时，
    // 两个协程并发动同一个子进程会互相打脸（一个刚 start 一个就 stop）。
    private val workLock = Mutex()

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        startForegroundCompat("启动中…")
        // 必须在主线程之外做：启动要轮询端口（最多十几秒）、停止要 waitFor(5s)、
        // 重启是两者相加。放主线程上一次重启就能占满 5s 以上，系统直接弹「应用无响应」。
        val action = intent?.action
        scope.launch {
            workLock.withLock {
                when (action) {
                    ACTION_STOP -> stopProxy()
                    ACTION_RESTART -> {
                        ProxyProcess.stop()
                        _state.value = State()
                        startProxy()
                    }
                    else -> startProxy()
                }
            }
        }
        return START_STICKY
    }

    private fun startProxy() {
        val ctx = applicationContext
        FileStore.ensureLayout(ctx)

        // 没有账号就别起进程：Go 侧 main.go 遇到空账号会 log.Fatalf 退出，
        // 用户只会看到一句没头没尾的 "no accounts loaded / exit=1"。
        // 这里提前拦下，直接告诉他缺什么。
        val accounts = AuthStore.parse(FileStore.readAuth(ctx))
        if (accounts.isEmpty()) {
            val msg = "还没有账号：去「账号」页添加，或用「从文件导入 auth.json」"
            _state.value = State(running = false, addr = "", lastError = msg)
            updateNotification("未启动：没有账号")
            LogBus.append("[启动跳过] $msg")
            return
        }

        // 用户配置里的路径可能是 PC 上的相对路径，改写成 App 私有目录绝对路径
        val runtimeCfg = FileStore.prepareRuntimeConfig(ctx, FileStore.readConfig(ctx))
        val port = FileStore.configuredListen(ctx).substringAfterLast(':').toIntOrNull() ?: 7863

        val err = ProxyProcess.start(ctx, runtimeCfg, port)
        val addr = if (err == null) FileStore.configuredListen(ctx) else ""

        _state.value = State(running = err == null, addr = addr, lastError = err ?: "")
        updateNotification(if (err == null) "运行中 · $addr" else "启动失败")

        if (err != null) {
            LogBus.append("[启动失败] $err")
            // 不 stopSelf：留着通知让用户看到错误，点进 App 能看到日志
            startWatching()
            return
        }
        LogBus.append("[服务] 网关已启动 · $addr")
        startWatching()
        OverlayController.refresh(this)
    }

    private fun stopProxy() {
        watchJob?.cancel()
        ProxyProcess.stop()
        _state.value = State()
        LogBus.append("[服务] 网关已停止")
        OverlayController.detach()
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
    }

    /** 每 2s 检查进程是否还活着；意外退出就更新状态并提示。 */
    private fun startWatching() {
        watchJob?.cancel()
        watchJob = scope.launch {
            while (true) {
                delay(2000)
                val alive = ProxyProcess.isRunning()
                val cur = _state.value
                if (cur.running && !alive) {
                    _state.value = cur.copy(running = false, lastError = "网关进程意外退出")
                    updateNotification("已停止（进程退出）")
                    OverlayController.detach()
                } else if (alive && !cur.running) {
                    _state.value = cur.copy(running = true)
                }
            }
        }
    }

    override fun onDestroy() {
        watchJob?.cancel()
        ProxyProcess.stop()
        _state.value = State()
        scope.cancel()
        OverlayController.detach()
        super.onDestroy()
    }

    // ─── 通知 ─────────────────────────────────────────────────────────────

    private fun startForegroundCompat(text: String) {
        ensureChannel()
        val notif = buildNotification(text)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            startForeground(NOTIF_ID, notif, ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC)
        } else {
            startForeground(NOTIF_ID, notif)
        }
    }

    private fun updateNotification(text: String) {
        runCatching {
            getSystemService(NotificationManager::class.java).notify(NOTIF_ID, buildNotification(text))
        }
    }

    private fun buildNotification(text: String): Notification {
        val open = PendingIntent.getActivity(
            this, 0, Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val stop = PendingIntent.getService(
            this, 1, Intent(this, ProxyService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val b = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O)
            Notification.Builder(this, CHANNEL_ID)
        else
            @Suppress("DEPRECATION") Notification.Builder(this)

        return b
            .setContentTitle("Reverse-Proxy")
            .setContentText(text)
            .setSmallIcon(android.R.drawable.stat_sys_upload)
            .setOngoing(true)
            .setContentIntent(open)
            .addAction(
                Notification.Action.Builder(
                    Icon.createWithResource(this, android.R.drawable.ic_media_pause),
                    "停止",
                    stop,
                ).build()
            )
            .build()
    }

    private fun ensureChannel() {
        val nm = getSystemService(NotificationManager::class.java)
        if (nm.getNotificationChannel(CHANNEL_ID) == null) {
            nm.createNotificationChannel(
                NotificationChannel(CHANNEL_ID, "网关服务", NotificationManager.IMPORTANCE_LOW)
            )
        }
    }

    companion object {
        private const val CHANNEL_ID = "proxy"
        private const val NOTIF_ID = 1
        const val ACTION_STOP = "com.reverse.proxy.STOP"
        const val ACTION_RESTART = "com.reverse.proxy.RESTART"

        private val _state = MutableStateFlow(State())
        val state: StateFlow<State> = _state.asStateFlow()

        fun start(ctx: Context) {
            val i = Intent(ctx, ProxyService::class.java)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) ctx.startForegroundService(i)
            else ctx.startService(i)
        }

        fun stop(ctx: Context) {
            ctx.startService(Intent(ctx, ProxyService::class.java).setAction(ACTION_STOP))
        }

        /** 重载配置：停掉进程再按新的 config.json / auth.json 重启。 */
        fun restart(ctx: Context) {
            ctx.startService(Intent(ctx, ProxyService::class.java).setAction(ACTION_RESTART))
        }
    }
}
package com.reverse.proxy

import android.content.Context
import android.graphics.Color
import android.graphics.PixelFormat
import android.graphics.Typeface
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import android.view.Gravity
import android.view.MotionEvent
import android.view.View
import android.view.WindowManager
import android.widget.LinearLayout
import android.widget.TextView

/**
 * 悬浮窗：收起是一颗胶囊（● 127.0.0.1:7863），点开是卡片（地址 / 最近日志 / 停止）。
 *
 * 用纯 View 实现，不用 Compose —— 悬浮窗里塞 ComposeView 要额外管生命周期，不值。
 */
object OverlayController {

    private val main = Handler(Looper.getMainLooper())
    private var wm: WindowManager? = null
    private var root: LinearLayout? = null
    private var expanded = false

    /** 用户是否显式打开了悬浮窗（关闭后轮询不再自动挂载）。 */
    var enabled = false
        private set

    private lateinit var head: TextView
    private lateinit var body: LinearLayout
    private lateinit var addrText: TextView
    private lateinit var logText: TextView

    fun canDraw(ctx: Context): Boolean =
        Build.VERSION.SDK_INT < Build.VERSION_CODES.M || Settings.canDrawOverlays(ctx)

    /**
     * 显示/刷新；服务停止或用户关闭时自动隐藏。
     *
     * 调用方可能在任意线程（Service 的协程），而 WindowManager 只能在主线程操作，
     * 所以统一切回主线程；用 applicationContext 避免持有 Activity。
     */
    fun refresh(ctx: Context) {
        val app = ctx.applicationContext
        main.post { refreshOnMain(app) }
    }

    private fun refreshOnMain(ctx: Context) {
        val st = ProxyService.state.value
        if (!enabled || !st.running) { detach(); return }
        if (!canDraw(ctx)) return
        if (root == null) attach(ctx)
        update(st)
    }

    private fun attach(ctx: Context) {
        val w = ctx.getSystemService(Context.WINDOW_SERVICE) as WindowManager
        val rootView = LinearLayout(ctx).apply {
            orientation = LinearLayout.VERTICAL
            setBackgroundColor(0xE6202020.toInt())
            setPadding(28, 20, 28, 20)
        }

        head = TextView(ctx).apply {
            setTextColor(Color.WHITE)
            textSize = 14f
            typeface = Typeface.MONOSPACE
        }

        body = LinearLayout(ctx).apply {
            orientation = LinearLayout.VERTICAL
            visibility = View.GONE
        }
        addrText = TextView(ctx).apply {
            setTextColor(0xFF80D8FF.toInt()); textSize = 12f; typeface = Typeface.MONOSPACE
        }
        logText = TextView(ctx).apply {
            setTextColor(0xFFB0BEC5.toInt()); textSize = 11f
            typeface = Typeface.MONOSPACE; maxLines = 8
        }
        val stopBtn = TextView(ctx).apply {
            text = "[ 停止服务 ]"
            setTextColor(0xFFFF8A80.toInt())
            textSize = 12f
            setPadding(0, 16, 0, 0)
            setOnClickListener { ProxyService.stop(ctx) }
        }
        body.addView(addrText)
        body.addView(logText)
        body.addView(stopBtn)

        rootView.addView(head)
        rootView.addView(body)

        val type = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O)
            WindowManager.LayoutParams.TYPE_APPLICATION_OVERLAY
        else
            @Suppress("DEPRECATION") WindowManager.LayoutParams.TYPE_PHONE

        val lp = WindowManager.LayoutParams(
            WindowManager.LayoutParams.WRAP_CONTENT,
            WindowManager.LayoutParams.WRAP_CONTENT,
            type,
            WindowManager.LayoutParams.FLAG_NOT_FOCUSABLE or
                WindowManager.LayoutParams.FLAG_LAYOUT_NO_LIMITS,
            PixelFormat.TRANSLUCENT,
        ).apply {
            gravity = Gravity.TOP or Gravity.START
            x = 24; y = 160
        }

        // 手势：拖动移动；没移动（<8px）当作单击 → 展开/收起。
        // 不能用 setOnClickListener：OnTouchListener 返回 true 时 click 不会派发。
        var downX = 0f; var downY = 0f; var startX = 0; var startY = 0; var moved = false
        rootView.setOnTouchListener { _, e ->
            when (e.action) {
                MotionEvent.ACTION_DOWN -> {
                    downX = e.rawX; downY = e.rawY
                    startX = lp.x; startY = lp.y; moved = false
                }
                MotionEvent.ACTION_MOVE -> {
                    val dx = (e.rawX - downX).toInt()
                    val dy = (e.rawY - downY).toInt()
                    if (kotlin.math.abs(dx) > 8 || kotlin.math.abs(dy) > 8) moved = true
                    if (moved) {
                        lp.x = startX + dx; lp.y = startY + dy
                        runCatching { wm?.updateViewLayout(rootView, lp) }
                    }
                }
                MotionEvent.ACTION_UP -> {
                    if (!moved) {
                        expanded = !expanded
                        body.visibility = if (expanded) View.VISIBLE else View.GONE
                        ProxyService.state.value.takeIf { it.running }?.let { update(it) }
                    }
                }
            }
            true
        }

        runCatching { w.addView(rootView, lp) }
        wm = w; root = rootView
    }

    private fun update(st: ProxyService.State) {
        val addr = st.addr.ifEmpty { "127.0.0.1:7863" }
        head.text = if (expanded) "● Reverse-Proxy    [ 收起 ]" else "● $addr"
        addrText.text = addr
        logText.text = LogBus.lines.value.takeLast(6).joinToString("\n")
    }

    /** 用户切换开关：on=true 尝试挂载，false 移除。 */
    fun toggle(ctx: Context, on: Boolean) {
        enabled = on
        if (!on) { detach(); return }
        if (canDraw(ctx)) refresh(ctx)
    }

    fun detach() {
        main.post {
            root?.let { runCatching { wm?.removeView(it) } }
            root = null; wm = null; expanded = false
        }
    }
}
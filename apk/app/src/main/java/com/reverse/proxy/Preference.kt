package com.reverse.proxy

import android.content.ComponentName
import android.content.Context
import android.content.pm.PackageManager

/** App 级简单开关（SharedPreferences），目前只有「开机自启」。 */
object Preference {
    private const val FILE = "rp"
    private const val KEY_AUTOSTART = "autostart"
    private const val KEY_THEME = "theme_mode"

    /** 0=跟随系统 1=浅色 2=深色 */
    fun theme(ctx: Context): Int =
        ctx.getSharedPreferences(FILE, Context.MODE_PRIVATE).getInt(KEY_THEME, THEME_SYSTEM)

    fun setTheme(ctx: Context, mode: Int) {
        ctx.getSharedPreferences(FILE, Context.MODE_PRIVATE)
            .edit().putInt(KEY_THEME, mode).apply()
    }

    fun autostart(ctx: Context): Boolean =
        ctx.getSharedPreferences(FILE, Context.MODE_PRIVATE).getBoolean(KEY_AUTOSTART, false)

    fun setAutostart(ctx: Context, on: Boolean) {
        ctx.getSharedPreferences(FILE, Context.MODE_PRIVATE)
            .edit().putBoolean(KEY_AUTOSTART, on).apply()
        // BootReceiver 在 manifest 里默认 enabled=false，这里同步启停它，
        // 避免「没开自启却仍被系统拉起」。
        val state = if (on) PackageManager.COMPONENT_ENABLED_STATE_ENABLED
                    else PackageManager.COMPONENT_ENABLED_STATE_DISABLED
        runCatching {
            ctx.packageManager.setComponentEnabledSetting(
                ComponentName(ctx, BootReceiver::class.java),
                state,
                PackageManager.DONT_KILL_APP,
            )
        }
    }
}
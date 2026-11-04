package com.reverse.proxy

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/** 开机自启（manifest 里默认 enabled=false，由 App 内开关控制）。 */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        if (intent.action != Intent.ACTION_BOOT_COMPLETED) return
        if (Preference.autostart(ctx)) ProxyService.start(ctx)
    }
}
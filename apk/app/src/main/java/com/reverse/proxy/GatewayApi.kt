package com.reverse.proxy

import org.json.JSONObject
import java.net.HttpURLConnection
import java.net.URL

/**
 * 回连自己的网关查状态（App 内展示用）。
 *
 * 走真实 HTTP 接口而不是读 Go 内部变量 —— 这样账号过滤规则（区域/供应商/账号级密钥）
 * 只有一份实现，App 显示与外部客户端行为不会分叉。
 */
object GatewayApi {

    /** GET /status，返回原始 JSON；失败返回 null（调用方显示错误即可，不抛）。 */
    fun status(addr: String, apiKey: String, timeoutMs: Int = 3000): JSONObject? = runCatching {
        val conn = open("http://" + localAddr(addr) + "/status", timeoutMs)
        if (apiKey.isNotEmpty()) conn.setRequestProperty("Authorization", "Bearer " + apiKey)
        try {
            if (conn.responseCode != 200) null
            else JSONObject(conn.inputStream.bufferedReader().use { it.readText() })
        } finally {
            conn.disconnect()
        }
    }.getOrNull()

    /** GET /healthz：200 = 服务在跑且至少有一个可用账号；503 = 在跑但没可用账号。 */
    fun healthz(addr: String, timeoutMs: Int = 3000): Boolean = runCatching {
        val conn = open("http://" + localAddr(addr) + "/healthz", timeoutMs)
        try {
            conn.responseCode == 200
        } finally {
            conn.disconnect()
        }
    }.getOrDefault(false)

    /** GET /v1/models，返回模型 id 列表；失败返回空列表。 */
    fun models(addr: String, apiKey: String, timeoutMs: Int = 5000): List<String> = runCatching {
        val conn = open("http://" + localAddr(addr) + "/v1/models", timeoutMs)
        if (apiKey.isNotEmpty()) conn.setRequestProperty("Authorization", "Bearer " + apiKey)
        try {
            if (conn.responseCode != 200) return emptyList()
            val body = conn.inputStream.bufferedReader().use { it.readText() }
            val arr = JSONObject(body).optJSONArray("data") ?: return emptyList()
            val out = ArrayList<String>(arr.length())
            for (i in 0 until arr.length()) {
                arr.optJSONObject(i)?.optString("id", "")?.takeIf { it.isNotEmpty() }?.let { out.add(it) }
            }
            out
        } finally {
            conn.disconnect()
        }
    }.getOrDefault(emptyList())

    /** 一个账号的签到结果。 */
    data class CheckinItem(
        val uid: String,
        val provider: String,
        val nickname: String,
        val state: String,
        val credit: Long,
        val streakDays: Int,
        val msg: String,
    ) {
        /** 与 Web 管理台 checkinText 完全同一套措辞，两处显示不能分叉。 */
        val text: String
            get() = when (state) {
                "ok" -> "签到完成" + if (credit > 0) " +$credit" else ""
                "already" -> "已经签到"
                "unsupported" -> "没有签到入口"
                "pending" -> "接口还在接入中" + if (msg.isNotEmpty()) "：$msg" else ""
                else -> "签到失败" + if (msg.isNotEmpty()) "：$msg" else ""
            }

        /** 按钮/标签短文案。 */
        val badge: String
            get() = when (state) {
                "ok" -> "完成"
                "already" -> "已签到"
                "unsupported" -> "没有签到"
                "pending" -> "接入中"
                else -> "失败"
            }
    }

    /**
     * POST /checkin：对当前密钥可见的账号执行一次签到。
     *
     * 用账号级密钥调就只签那一个号，用主密钥调就签全池 —— 范围由密钥决定，
     * 与 /status、/usage 同一套规则，App 不需要自己传 uid。
     *
     * 失败返回 null（调用方显示错误即可，不抛）。
     */
    fun checkin(addr: String, apiKey: String, timeoutMs: Int = 60000): List<CheckinItem>? = runCatching {
        val conn = open("http://" + localAddr(addr) + "/checkin", timeoutMs).apply {
            requestMethod = "POST"
            doOutput = true
            setRequestProperty("Content-Type", "application/json")
            if (apiKey.isNotEmpty()) setRequestProperty("Authorization", "Bearer " + apiKey)
        }
        try {
            // Go 侧用 http.NewRequestWithContext 解析，带 body 必须显式写长度，
            // 否则上游读到 EOF。空 JSON 对象是最小合法体。
            conn.outputStream.use { it.write("{}".toByteArray()) }
            if (conn.responseCode != 200) return null
            val body = conn.inputStream.bufferedReader().use { it.readText() }
            val arr = JSONObject(body).optJSONArray("accounts") ?: return emptyList()
            val out = ArrayList<CheckinItem>(arr.length())
            for (i in 0 until arr.length()) {
                val o = arr.optJSONObject(i) ?: continue
                out.add(
                    CheckinItem(
                        uid = o.optString("uid", ""),
                        provider = o.optString("provider", ""),
                        nickname = o.optString("nickname", ""),
                        state = o.optString("state", "failed"),
                        credit = o.optLong("credit", 0L),
                        streakDays = o.optInt("streak_days", 0),
                        msg = o.optString("msg", ""),
                    )
                )
            }
            out
        } finally {
            conn.disconnect()
        }
    }.getOrNull()

    private fun open(url: String, timeoutMs: Int): HttpURLConnection =
        (URL(url).openConnection() as HttpURLConnection).apply {
            requestMethod = "GET"
            connectTimeout = timeoutMs
            readTimeout = timeoutMs
        }

    /**
     * 把监听地址转成"本机客户端该连的地址"。
     *
     * 网关可能监听 0.0.0.0 / [::]（用户改成对外），但 App 自己回连一律走 127.0.0.1：
     *   - 0.0.0.0 不是可路由目标，部分 ROM 上直接连接失败；
     *   - network_security_config 只对 127.0.0.1 / localhost 放行明文 HTTP。
     */
    fun localAddr(addr: String): String {
        val a = addr.trim().ifEmpty { "127.0.0.1:7863" }
        val port = a.substringAfterLast(':', "7863")
        val host = a.substringBeforeLast(':', "")
        return if (host.isEmpty() || host == "0.0.0.0" || host == "[::]" || host == "::")
            "127.0.0.1:" + port
        else a
    }

    /**
     * 把 /status 压成一行摘要。
     *
     * localTotal：auth.json 里的账号总数，传 -1 = 用 /status 的 total。
     * 为什么需要覆盖：/status 在 provider 为空时按历史兼容只统计 codebuddy
     * （见 internal/pool/pick.go 的 providerMatch），App 用主密钥查就会把
     * trae/qoder/doubao/deepseek 全漏掉，显示成"账号 2"而实际有 6 个。
     */
    fun summarize(o: JSONObject, localTotal: Int = -1): String {
        val total = if (localTotal >= 0) localTotal else o.optInt("total", 0)
        val healthy = o.optInt("healthy", 0)
        val cooling = o.optInt("cooling", 0)
        val disabled = o.optInt("disabled", 0)
        val inflight = o.optInt("in_flight_full", 0)
        val sticky = o.optInt("sticky_sessions", 0)
        val redis = o.optString("redis_mode", "noop")
        // 总数为本地真实账号数时，健康/冷却只是 /status 列出的那部分（主密钥下仅 codebuddy），
        // 把两者混在一个括号里会读成"6 个号只有 2 个健康"，是假警报。分开写。
        if (localTotal >= 0 && localTotal != total) {
            return "账号 " + localTotal + "（网关可调度 " + total + "：健康 " + healthy +
                " / 冷却 " + cooling + " / 禁用 " + disabled + "）" +
                " · 在途 " + inflight + " · 粘性 " + sticky + " · Redis " + redis
        }
        return "账号 " + total + "（健康 " + healthy + " / 冷却 " + cooling + " / 禁用 " + disabled + "）" +
            " · 在途 " + inflight + " · 粘性 " + sticky + " · Redis " + redis
    }
}
package com.reverse.proxy

import org.json.JSONObject
import java.io.BufferedReader
import java.net.HttpURLConnection
import java.net.URL

/**
 * 三家"凭证不能直接用"的供应商，把用户在网页上拿到的最小凭证换成网关能吃的完整条目。
 *
 * 协议全部照抄 Go 侧，不自己发明端点与头名：
 *   trae      {sourceId}  → POST api.trae.cn/cloudide/api/v3/common/GetUserToken
 *                            （cmd/login/main.go:534 traeMintJWT → upstream/trae.go:268）
 *   qoder     {pat}       → POST openapi.qoder.com.cn/api/v1/jobToken/exchange
 *                            （cmd/login/main.go:809 qoderExchangePAT）
 *   codebuddy 设备流       → POST /v2/plugin/auth/state → 用户浏览器授权
 *                            → GET /v2/plugin/auth/token?state=... → GET /v2/plugin/login/account
 *                            （cmd/login/main.go:257 loginCodeBuddy）
 *
 * 为什么必须在这里换：网关 internal/auth/auth.go:162 要求条目里 accessToken 非空，
 * 空字符串的条目会被静默跳过 —— 用户会看到"加成功了"但网关报 no accounts loaded。
 */
object ProvidersNet {

    /** CodeBuddy 与 CLI 一致的 UA，端点对 UA 敏感。 */
    private const val UA = "CLI/2.63.2 CodeBuddy/2.63.2"

    data class Done(val entry: JSONObject)

    // ─── Trae：用登录会话（sourceId）铸一枚 Cloud-IDE-JWT ──────────────────

    /**
     * sourceId 就是请求头 X-Cloudide-Session 的值（JWT 载荷里的 data.source_id）。
     *
     * 关键点（upstream/trae.go:250-267 实机验证过）：source_id 必须放
     * X-Cloudide-Session；放 X-Cloudide-Token 会被判 401/20101。
     * 会话长期有效，所以这一条路能让 trae 账号免于 8 小时一过期。
     */
    fun traeFromSourceId(sourceId: String, nickname: String): Result<JSONObject> = runCatching {
        val sid = sourceId.trim()
        require(sid.isNotEmpty()) { "sourceId 不能为空" }

        val conn = open("https://api.trae.cn/cloudide/api/v3/common/GetUserToken")
        conn.requestMethod = "POST"
        conn.doOutput = true
        conn.setRequestProperty("Content-Type", "application/json")
        conn.setRequestProperty("Accept", "application/json")
        conn.setRequestProperty("X-Cloudide-Session", sid)

        val body = post(conn, "{}")
        val o = JSONObject(body)
        val result = o.optJSONObject("Result")
        val token = result?.optString("Token", "").orEmpty()
        if (token.isEmpty()) {
            val err = o.optJSONObject("ResponseMetadata")
                ?.optJSONObject("Error")
            val code = err?.optString("Code", "").orEmpty()
            val msg = err?.optString("Message", "").orEmpty()
            throw IllegalStateException(
                "换令牌失败：code=" + code.ifEmpty { "?" } + " " + msg.ifEmpty { body.take(120) } +
                    "（会话可能已失效，去浏览器 F12 复制 Authorization 里的 Cloud-IDE-JWT，直接粘 accessToken）"
            )
        }

        val expiresAt = parseRfc3339(result.optString("ExpiredAt", "")).takeIf { it > 0 }
            ?: jwtExp(token)

        val auth = JSONObject()
        auth.put("accessToken", token)
        auth.put("expiresAt", expiresAt)
        val account = JSONObject()
        account.put("uid", jwtDataField(token, "id") ?: "")
        account.put("nickname", nickname)
        val extra = JSONObject()
        // sourceId 是长期凭证，必须存下来：JWT 8 小时后过期，靠它才能自动续
        extra.put("sourceId", sid)
        jwtSourceId(token)?.takeIf { it.isNotEmpty() }?.let { extra.put("sourceId", it) }

        JSONObject().apply {
            put("provider", "trae")
            put("auth", auth)
            put("account", account)
            put("extra", extra)
        }
    }

    // ─── Qoder：PAT 换 jobToken ────────────────────────────────────────────

    /**
     * PAT（pt- 开头）长期有效，换来的 jobToken（jt-）只有 24h。
     * 所以 pat 必须留在 extra 里，网关临期用它自己再换（upstream/qoder.go 路线 1）。
     */
    fun qoderFromPat(pat: String, nickname: String): Result<JSONObject> = runCatching {
        val p = pat.trim()
        require(p.isNotEmpty()) { "PAT 不能为空" }
        require(p.startsWith("pt-")) { "PAT 应该以 pt- 开头，你贴的看起来不是 PAT" }

        val conn = open("https://openapi.qoder.com.cn/api/v1/jobToken/exchange")
        conn.requestMethod = "POST"
        conn.doOutput = true
        conn.setRequestProperty("Content-Type", "application/json")
        conn.setRequestProperty("Accept", "application/json")
        conn.setRequestProperty("User-Agent", "reverse-proxy")
        conn.setRequestProperty("Cosy-Version", "1.0.1")
        conn.setRequestProperty("Cosy-ClientType", "5")

        val body = post(conn, JSONObject().put("personal_token", p).toString())
        val o = JSONObject(body)
        val jt = o.optString("token", "").ifEmpty { o.optString("job_token", "") }
        if (jt.isEmpty()) throw IllegalStateException("PAT 兑换失败：上游没返回 token（" + body.take(120) + "）")
        val rt = o.optString("refresh_token", "")
        val expIn = o.optLong("expires_in", 0L)
        // 上游给的是毫秒（main.go:846），退回 24h
        val expiresAt = if (expIn > 0) System.currentTimeMillis() / 1000 + expIn / 1000
        else System.currentTimeMillis() / 1000 + 86400

        val uid = qoderUserinfo(jt).ifEmpty { randomUid() }

        val auth = JSONObject()
        auth.put("accessToken", jt)
        if (rt.isNotEmpty()) auth.put("refreshToken", rt)
        auth.put("expiresAt", expiresAt)
        val account = JSONObject()
        account.put("uid", uid)
        account.put("nickname", nickname)
        val extra = JSONObject()
        extra.put("pat", p)

        JSONObject().apply {
            put("provider", "qoder")
            put("auth", auth)
            put("account", account)
            put("extra", extra)
        }
    }

    /** userinfo 返回形态不固定，按 main.go:851 的解析链依次试。 */
    private fun qoderUserinfo(jt: String): String = runCatching {
        val conn = open("https://openapi.qoder.com.cn/api/v1/userinfo")
        conn.setRequestProperty("Authorization", "Bearer " + jt)
        conn.setRequestProperty("Accept", "application/json")
        val body = read(conn)
        val o = JSONObject(body)
        val data = o.optJSONObject("data")
        sequenceOf(
            o.optString("uid", ""),
            data?.optString("user_id", "").orEmpty(),
            data?.optString("userId", "").orEmpty(),
            data?.optString("uid", "").orEmpty(),
        ).firstOrNull { it.isNotBlank() }?.trim().orEmpty()
    }.getOrDefault("")

    // ─── CodeBuddy：OAuth 设备流 ───────────────────────────────────────────

    /** 区域端点（cmd/login/main.go:59 的 regions 表）。 */
    data class Region(val base: String, val origin: String)

    fun region(intl: Boolean) = if (intl) Region("https://www.codebuddy.ai", "https://www.codebuddy.ai")
    else Region("https://copilot.tencent.com", "https://www.codebuddy.cn")

    /** 第一步：拿授权链接 + state。用户要在浏览器打开这个链接完成登录。 */
    fun codebuddyAuthUrl(intl: Boolean): Result<Pair<String, String>> = runCatching {
        val r = region(intl)
        val conn = open(r.base + "/v2/plugin/auth/state?platform=CLI")
        conn.requestMethod = "POST"
        conn.doOutput = true
        headers(conn, r)
        val body = post(conn, "{}")
        val data = envelope(body)
        val state = data.optString("state", "")
        val url = data.optString("authUrl", "")
        if (state.isEmpty() || url.isEmpty()) throw IllegalStateException("上游没返回 state 或 authUrl（" + body.take(120) + "）")
        state to url
    }

    /**
     * 第二步：轮询换 token，再拿 uid，组装条目。
     *
     * 每次调用只问一次上游（不在这里 sleep），轮询交给 UI 的协程 ——
     * 这样界面不会卡住，用户也能随时取消。
     */
    fun codebuddyPoll(intl: Boolean, state: String, nickname: String): Result<JSONObject?> = runCatching {
        val r = region(intl)
        val conn = open(r.base + "/v2/plugin/auth/token?state=" + state)
        headers(conn, r)
        val body = read(conn)
        val data = runCatching { envelope(body) }.getOrNull()
        val token = data?.optString("accessToken", "").orEmpty()
        // 还没登录完：上游还不给 token，返回 null 让调用方继续轮询
        if (token.isEmpty()) return@runCatching null

        val refresh = data!!.optString("refreshToken", "")
        val expiresIn = data.optLong("expiresIn", 0L)
        val domain = data.optString("domain", "").ifEmpty { r.origin }
        val expiresAt = if (expiresIn > 0) System.currentTimeMillis() / 1000 + expiresIn
        else jwtExp(token)

        // 拿 uid：必须带 token 调 login/account，拿不到 uid 这个号就没法用
        val uid = codebuddyUid(r, state, token)
            ?: throw IllegalStateException("拿不到 uid，登录态可能已失效，请重试授权")

        val auth = JSONObject()
        auth.put("accessToken", token)
        auth.put("refreshToken", refresh)
        auth.put("expiresAt", expiresAt)
        auth.put("domain", domain)
        val account = JSONObject()
        account.put("uid", uid)
        account.put("enterpriseId", codebuddyEnterpriseId ?: "")
        account.put("nickname", nickname)

        JSONObject().apply {
            put("provider", "codebuddy")
            put("auth", auth)
            put("account", account)
        }
    }

    /** login/account 顺带返回的 enterpriseId，供上面复用。 */
    private var codebuddyEnterpriseId: String? = null

    private fun codebuddyUid(r: Region, state: String, token: String): String? = runCatching {
        val conn = open(r.base + "/v2/plugin/login/account?state=" + state)
        headers(conn, r)
        conn.setRequestProperty("Authorization", "Bearer " + token)
        val data = envelope(read(conn))
        codebuddyEnterpriseId = data.optString("enterpriseId", "")
        data.optString("uid", "").trim().ifEmpty { null }
    }.getOrNull()

    // ─── 续期（手动触发） ────────────────────────────────────────────────

    /** 续期结果：只含要覆盖回 auth.json 的字段。 */
    data class Renewed(val accessToken: String, val refreshToken: String, val expiresAt: Long)

    /**
     * trae：用 extra.sourceId 换一枚新 Cloud-IDE-JWT。
     *
     * 与网关 token-watch 同一条路（internal/scheduler/token.go:129 把 trae 判为
     * selfHealable —— GetUserToken 认 X-Cloudide-Session，不认 refreshToken）。
     * 手上那枚 JWT 就算已过期也没关系，会话还在就能换。
     */
    fun renewTrae(entry: JSONObject): Result<Renewed> = runCatching {
        val extra = entry.optJSONObject("extra")
        val auth = entry.optJSONObject("auth") ?: entry
        val sid = extra?.optString("sourceId", "").orEmpty().trim()
            .ifEmpty { jwtSourceId(auth.optString("accessToken", entry.optString("accessToken", ""))) ?: "" }
        if (sid.isEmpty()) {
            throw IllegalStateException(
                "没有 sourceId，换不了新令牌。去浏览器 F12 找一个请求的 X-Cloudide-Session 头，" +
                    "重新添加这个账号（只填 sourceId 就行）。"
            )
        }

        val conn = open("https://api.trae.cn/cloudide/api/v3/common/GetUserToken")
        conn.requestMethod = "POST"
        conn.doOutput = true
        conn.setRequestProperty("Content-Type", "application/json")
        conn.setRequestProperty("Accept", "application/json")
        // 头名必须是 X-Cloudide-Session：放 X-Cloudide-Token 会被判 401/20101
        conn.setRequestProperty("X-Cloudide-Session", sid)
        val old = auth.optString("accessToken", "")
        if (old.isNotEmpty()) conn.setRequestProperty("X-Cloudide-Token", old)

        val o = JSONObject(post(conn, "{}"))
        val result = o.optJSONObject("Result")
        val token = result?.optString("Token", "").orEmpty()
        if (token.isEmpty()) {
            val err = o.optJSONObject("ResponseMetadata")?.optJSONObject("Error")
            throw IllegalStateException(
                "上游没给新令牌：code=" + (err?.optString("Code", "") ?: "?") + " " +
                    (err?.optString("Message", "") ?: "") + "（会话可能已失效，需要重新登录）"
            )
        }
        val exp = parseRfc3339(result.optString("ExpiredAt", "")).takeIf { it > 0 } ?: jwtExp(token)
        Renewed(token, "", exp)
    }

    /**
     * qoder：优先用 extra.pat 换新 jobToken；PAT 失败再退回 drt- 轮换 dt-。
     * 与 internal/upstream/qoder.go:159 的两条路线一致。
     */
    fun renewQoder(entry: JSONObject): Result<Renewed> = runCatching {
        val extra = entry.optJSONObject("extra")
        val auth = entry.optJSONObject("auth") ?: entry
        val pat = extra?.optString("pat", "").orEmpty().trim()
        val drt = auth.optString("refreshToken", "").ifEmpty { entry.optString("refreshToken", "") }.trim()

        if (pat.isNotEmpty()) {
            val patErr = runCatching {
                val conn = open("https://openapi.qoder.com.cn/api/v1/jobToken/exchange")
                conn.requestMethod = "POST"
                conn.doOutput = true
                conn.setRequestProperty("Content-Type", "application/json")
                conn.setRequestProperty("Accept", "application/json")
                conn.setRequestProperty("User-Agent", "reverse-proxy")
                conn.setRequestProperty("Cosy-Version", "1.0.1")
                conn.setRequestProperty("Cosy-ClientType", "5")
                val o = JSONObject(post(conn, JSONObject().put("personal_token", pat).toString()))
                val jt = o.optString("token", "").ifEmpty { o.optString("job_token", "") }
                if (jt.isEmpty()) throw IllegalStateException("PAT 兑换没返回 token")
                val expIn = o.optLong("expires_in", 0L)
                val exp = if (expIn > 0) System.currentTimeMillis() / 1000 + expIn / 1000
                else System.currentTimeMillis() / 1000 + 86400
                Renewed(jt, o.optString("refresh_token", ""), exp)
            }
            patErr.getOrNull()?.let { return@runCatching it }
            // PAT 挂了但还有 drt- 就继续往下试；都没有就把 PAT 的错抛出去
            if (drt.isEmpty()) {
                throw IllegalStateException("PAT 兑换失败：" + patErr.exceptionOrNull()?.message + "（也没有 drt- 可退）")
            }
        }

        if (drt.isEmpty()) {
            throw IllegalStateException("这个号既没有 PAT 也没有 refreshToken，只能重新用 PAT 添加一次。")
        }
        val conn = open("https://openapi.qoder.com.cn/api/v1/deviceToken/refresh")
        conn.requestMethod = "POST"
        conn.doOutput = true
        conn.setRequestProperty("Content-Type", "application/json")
        conn.setRequestProperty("Accept", "application/json")
        val dt = auth.optString("accessToken", entry.optString("accessToken", ""))
        if (dt.isNotEmpty()) conn.setRequestProperty("Authorization", "Bearer " + dt)
        val o = JSONObject(post(conn, JSONObject().put("refresh_token", drt).toString()))
        val newDt = o.optString("token", "").ifEmpty { o.optString("device_token", "") }
        val newRt = o.optString("refresh_token", "")
        if (newDt.isEmpty() || newRt.isEmpty()) throw IllegalStateException("设备令牌刷新返回不完整")
        val expIn = o.optLong("expires_in", 0L)
        val exp = if (expIn > 0) System.currentTimeMillis() / 1000 + expIn / 1000
        else parseRfc3339(o.optString("expires_at", "")).takeIf { it > 0 }
            ?: System.currentTimeMillis() / 1000 + 86400
        Renewed(newDt, newRt, exp)
    }

    // ─── 公共 ──────────────────────────────────────────────────────────────

    private fun headers(conn: HttpURLConnection, r: Region) {
        conn.setRequestProperty("Content-Type", "application/json")
        conn.setRequestProperty("Accept", "application/json, text/plain, */*")
        conn.setRequestProperty("X-Requested-With", "XMLHttpRequest")
        conn.setRequestProperty("Origin", r.origin)
        conn.setRequestProperty("Referer", r.origin + "/")
        conn.setRequestProperty("User-Agent", UA)
    }

    /** 解 {code,msg,data} 信封；code!=0 视为失败（main.go:963-970）。 */
    private fun envelope(body: String): JSONObject {
        val o = JSONObject(body)
        val code = o.optInt("code", 0)
        if (code != 0) {
            throw IllegalStateException("code=" + code + " " + o.optString("msg", ""))
        }
        return o.optJSONObject("data") ?: JSONObject()
    }

    private fun open(url: String): HttpURLConnection =
        (URL(url).openConnection() as HttpURLConnection).apply {
            connectTimeout = 15000
            readTimeout = 20000
        }

    private fun post(conn: HttpURLConnection, payload: String): String {
        conn.outputStream.use { it.write(payload.toByteArray()) }
        return read(conn)
    }

    private fun read(conn: HttpURLConnection): String {
        val code = conn.responseCode
        val stream = if (code in 200..299) conn.inputStream else conn.errorStream
        val text = stream?.let { s ->
            BufferedReader(s.reader()).use { br -> br.readText() }
        }.orEmpty()
        if (code !in 200..299) throw IllegalStateException("上游 HTTP " + code + "：" + text.take(200))
        return text
    }

    /** RFC3339 → Unix 秒；解析不了返回 0。 */
    private fun parseRfc3339(s: String): Long = runCatching {
        if (s.isBlank()) return 0
        java.time.OffsetDateTime.parse(s).toEpochSecond()
    }.getOrDefault(0)

    private fun randomUid(): String {
        val b = ByteArray(8)
        java.security.SecureRandom().nextBytes(b)
        return b.joinToString("") { String.format("%02x", it) }
    }

    /** JWT 载荷解码（只解不验签，本地取值足够）。 */
    private fun payload(token: String): JSONObject? = runCatching {
        val parts = token.split(".")
        if (parts.size < 2) return null
        val raw = android.util.Base64.decode(
            parts[1],
            android.util.Base64.URL_SAFE or android.util.Base64.NO_PADDING or android.util.Base64.NO_WRAP,
        ).toString(Charsets.UTF_8)
        JSONObject(raw)
    }.getOrNull()

    /** trae JWT 的 data.<key>（id = X-Uid，source_id = X-Cloudide-Session）。 */
    private fun jwtDataField(token: String, key: String): String? =
        payload(token)?.optJSONObject("data")?.optString(key, "")?.trim()?.ifEmpty { null }

    private fun jwtSourceId(token: String): String? = jwtDataField(token, "source_id")

    /** 供 UI 判断该号能否续期（trae 可从现有 JWT 里取到 source_id 时也算能续）。 */
    fun jwtSourceIdOf(token: String): String? = jwtSourceId(token)

    /** JWT 的 exp；取不到返回 0。 */
    private fun jwtExp(token: String): Long =
        payload(token)?.optLong("exp", 0L) ?: 0L
}

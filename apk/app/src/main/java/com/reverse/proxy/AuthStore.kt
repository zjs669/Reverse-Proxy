package com.reverse.proxy

import org.json.JSONObject

/** auth.json 里的一个账号（列表展示用）。 */
data class Account(
    val key: String,
    val provider: String,
    val providerName: String,
    val uid: String,
    val nickname: String,
    val expiresAt: Long,
    val renewable: Boolean,
    /** 该账号专属的网关密钥（config.json 的 account_keys[标签][uid]）；空 = 还没生成。 */
    val apiKey: String = "",
) {
    /** 剩余秒数；无期限返回 -1。 */
    val secondsLeft: Long
        get() = if (expiresAt <= 0) -1 else expiresAt - System.currentTimeMillis() / 1000

    /**
     * 剩余时间的中文说法。
     *
     * 不能只按天取整：trae 的 JWT 只有 8 小时、qoder 的 jobToken 只有 24 小时，
     * 整数除法会全变成"剩0天"，看着像坏了。按剩余量级换单位。
     */
    val remainText: String
        get() {
            val s = secondsLeft
            if (s < 0) return "无期限"
            if (s <= 0) return "已过期"
            return when {
                s >= 86400 -> "剩" + (s / 86400) + "天"
                s >= 3600 -> "剩" + (s / 3600) + "小时"
                s >= 60 -> "剩" + (s / 60) + "分钟"
                else -> "即将过期"
            }
        }

    val expired: Boolean
        get() = expiresAt > 0 && expiresAt < System.currentTimeMillis() / 1000

    /** 临期（不足 1 天）——用来决定标签是黄还是绿。 */
    val expiring: Boolean
        get() = expiresAt > 0 && secondsLeft in 1..86400

    /**
     * 卡片第一行显示的账号标识。
     *
     * 优先用户填的备注，其次 auth.json 里的账号标签（deepseek_1 / qoder_1 这种，
     * 与配置文件里的键一致，排查时能直接对上），最后才退到 uid——
     * uid 是 16 位十六进制，用户看不出是哪个号。
     */
    val label: String
        get() = nickname.ifEmpty { key.ifEmpty { uid } }
}

object AuthStore {

    /** 模板占位符：出现任何一个都说明用户没填真值。 */
    private val PLACEHOLDERS = listOf(
        "<Cloud-IDE-JWT 原文>", "mb5+...", "pt-...", "\"...\"", "<token>", "<uid>",
    )

    /**
     * 解析 auth.json。
     *
     * keys 传 config.json 的 account_keys，用来把"该账号的专属密钥"挂上来。
     *
     * 两种条目形态都要吃（与 internal/auth/auth.go 的 Parse 一致）：
     *   - 嵌套形 {"auth":{...},"account":{uid,nickname}}  ← codebuddy OAuth
     *   - 扁平形 {"accessToken":...,"uid":...,"nickname":...} ← trae/qoder/doubao/deepseek
     * 扁平形的 uid 在**顶层**，只读 account.uid 会让这些号 uid 为空，
     * 而账号级密钥正是按 uid 挂的 —— 漏了这条，专属密钥永远配不上号。
     */
    fun parse(raw: String, keys: JSONObject = JSONObject()): List<Account> {
        val root = runCatching { JSONObject(raw.trim().ifEmpty { "{}" }) }.getOrNull() ?: return emptyList()
        val out = mutableListOf<Account>()
        val it = root.keys()
        while (it.hasNext()) {
            val key = it.next()
            val entry = root.optJSONObject(key) ?: continue
            val providerId = Providers.detectProvider(entry)
            val auth = entry.optJSONObject("auth")
            val account = entry.optJSONObject("account")
            val extra = entry.optJSONObject("extra")

            // uid / nickname：嵌套形优先取 account 子对象，扁平形回落顶层。
            val uid = account?.optString("uid", "")?.trim().orEmpty()
                .ifEmpty { entry.optString("uid", "").trim() }
            val nickname = account?.optString("nickname", "")?.trim().orEmpty()
                .ifEmpty { entry.optString("nickname", "").trim() }

            out += Account(
                key = key,
                provider = providerId,
                providerName = Providers.byId(providerId)?.name ?: providerId,
                uid = uid,
                nickname = nickname,
                expiresAt = (auth?.optLong("expiresAt", 0L) ?: 0L)
                    .takeIf { it > 0 } ?: entry.optLong("expiresAt", 0L),
                renewable = !auth?.optString("refreshToken", "").isNullOrEmpty() ||
                    !entry.optString("refreshToken", "").isNullOrEmpty() ||
                    !extra?.optString("pat", "").isNullOrEmpty(),
                apiKey = if (uid.isEmpty()) "" else
                    keys.optJSONObject(key)?.optString(uid, "").orEmpty(),
            )
        }
        return out.sortedBy { it.providerName }
    }

    /**
     * 把用户粘贴的最简凭证追加进 auth.json。
     *
     * 输入可以是：{accessToken:...} / {pat:...} / {sessionid:...}，
     * 也可以是完整条目（带 auth/account 子对象）。
     */
    fun addAccount(existingRaw: String, pasted: String, providerId: String, nickname: String): Result<String> {
        val trimmed = pasted.trim()
        if (trimmed.isEmpty()) return Result.failure(IllegalArgumentException("请先粘贴凭证"))

        val holder = PLACEHOLDERS.firstOrNull { trimmed.contains(it) }
        if (holder != null) {
            return Result.failure(
                IllegalArgumentException("凭证里还有占位符 " + holder + "，请换成真实值")
            )
        }

        val pastedObj = runCatching { JSONObject(trimmed) }.getOrElse {
            return Result.failure(IllegalArgumentException("不是合法 JSON：" + it.message))
        }
        val root = runCatching { JSONObject(existingRaw.trim().ifEmpty { "{}" }) }.getOrElse {
            return Result.failure(IllegalArgumentException("现有 auth.json 不是合法对象"))
        }

        val entry = if (pastedObj.has("auth") || pastedObj.has("account")) {
            pastedObj
        } else {
            wrap(pastedObj, providerId, nickname)
        }

        // 至少要有 accessToken 或 extra.pat，否则网关用不了
        val hasToken = !entry.optJSONObject("auth")?.optString("accessToken", "").isNullOrEmpty()
        val hasPat = !entry.optJSONObject("extra")?.optString("pat", "").isNullOrEmpty()
        if (!hasToken && !hasPat) {
            return Result.failure(IllegalArgumentException("缺少 accessToken（或 qoder 的 pat）"))
        }

        val prefix = if (providerId == "codebuddy_intl") "codebuddyai" else providerId
        var key = prefix + "_1"
        var n = 1
        while (root.has(key)) { n++; key = prefix + "_" + n }
        root.put(key, entry)
        return Result.success(root.toString(2))
    }

    /**
     * 让 account_keys 与 auth.json 对齐，返回**新的** account_keys（没变化返回 null）。
     *
     * 规则（与 gen_account_keys.py 一致）：
     *   - 每个账号一把独立密钥，键为 {账号标签: {uid: secret}}
     *   - 已存在的密钥原样保留，不覆盖（用户可能已经把它填进客户端了）
     *   - 新账号/换 uid 的账号自动生成
     *   - 已从 auth.json 删掉的账号，其密钥一并清掉，避免残留旧 key 仍能通过鉴权
     *
     * 没有 uid 的账号跳过：网关按 uid 反查，空 uid 挂密钥等于挂一把永远匹配不上的锁。
     */
    fun ensureKeys(raw: String, existing: JSONObject): JSONObject? {
        val accounts = parse(raw, existing)
        val next = JSONObject()
        var changed = false

        for (a in accounts) {
            if (a.uid.isEmpty()) continue
            val prev = existing.optJSONObject(a.key)?.optString(a.uid, "").orEmpty()
            val secret = if (prev.isNotEmpty()) prev else FileStore.newAccountKey()
            if (prev.isEmpty()) changed = true
            val one = JSONObject()
            one.put(a.uid, secret)
            next.put(a.key, one)
        }
        // 条目数/账号集合变化也算改动
        if (!changed && existing.length() != next.length()) changed = true
        if (!changed) {
            val ek = existing.keys()
            while (ek.hasNext()) {
                val k = ek.next()
                if (next.optJSONObject(k)?.keys()?.hasNext() != true) { changed = true; break }
            }
        }
        return if (changed) next else null
    }

    /**
     * 把续期结果写回 auth.json 的指定账号。
     *
     * 兼容两种形态：嵌套形写 auth.*，扁平形写顶层（trae/qoder 常见）。
     * 只覆盖 accessToken/refreshToken/expiresAt 三个字段，其他（pat、sourceId、
     * uid、domain）原样保留 —— 那些才是下次还能续期的依据。
     */
    fun applyRenewed(raw: String, key: String, r: ProvidersNet.Renewed): Result<String> {
        val root = runCatching { JSONObject(raw.trim().ifEmpty { "{}" }) }.getOrElse {
            return Result.failure(IllegalArgumentException("auth.json 不是合法对象"))
        }
        val entry = root.optJSONObject(key)
            ?: return Result.failure(IllegalArgumentException("找不到账号 " + key))
        val target = entry.optJSONObject("auth") ?: entry
        target.put("accessToken", r.accessToken)
        if (r.refreshToken.isNotEmpty()) target.put("refreshToken", r.refreshToken)
        if (r.expiresAt > 0) target.put("expiresAt", r.expiresAt)
        return Result.success(root.toString(2))
    }

    /**
     * 该账号能不能手动续期（决定按钮是否可点）。
     *
     * trae 靠 extra.sourceId（或 JWT 里的 source_id）换新；qoder 靠 extra.pat
     * 或 refreshToken。其余家没有自助续期能力，不给按钮。
     */
    fun canRenew(acc: Account, entry: JSONObject?): Boolean {
        if (entry == null) return false
        val auth = entry.optJSONObject("auth") ?: entry
        val extra = entry.optJSONObject("extra")
        return when (acc.provider.removeSuffix("_intl")) {
            "trae" -> {
                val explicit = extra?.optString("sourceId", "").orEmpty().trim()
                val fromJwt = ProvidersNet.jwtSourceIdOf(auth.optString("accessToken", "")) ?: ""
                explicit.isNotEmpty() || fromJwt.isNotEmpty()
            }
            "qoder" -> {
                val pat = extra?.optString("pat", "").orEmpty().trim()
                val rt = auth.optString("refreshToken", "").trim()
                pat.isNotEmpty() || rt.isNotEmpty()
            }
            else -> false
        }
    }

    fun removeAccount(existingRaw: String, key: String): Result<String> {
        val root = runCatching { JSONObject(existingRaw.trim().ifEmpty { "{}" }) }.getOrElse {
            return Result.failure(IllegalArgumentException("auth.json 不是合法对象"))
        }
        root.remove(key)
        return Result.success(root.toString(2))
    }

    /**
     * 给缺 uid 的账号补一个。
     *
     * 规则照抄 cmd/login/main.go，不自己发明：
     *   - deepseek：凭证里的 id，否则固定 "deepseek-1"（main.go:748-753）
     *   - doubao：固定 "doubao-work-1"（main.go:689-691）
     *   - qoder：16 位随机 hex（main.go:606 的 genUUID 去横线前 16 位）
     *   - trae：从 JWT 载荷的 data.id 解，解不到就用随机
     *   - codebuddy：必须自带（OAuth 一定带 account.uid），没有就用随机兜底
     *
     * 随机兜底为什么安全：uid 只作账号标识与展示，上游鉴权走 accessToken。
     * qoder 侧同样有这条兜底（main.go:492-495 注释）。
     */
    fun defaultUid(providerId: String, raw: JSONObject): String {
        val given = raw.optString("uid", "").trim()
        if (given.isNotEmpty()) return given

        return when (providerId.removeSuffix("_intl")) {
            "deepseek" -> raw.optString("id", "").trim().ifEmpty { "deepseek-1" }
            "doubao" -> "doubao-work-1"
            "trae" -> {
                val tok = raw.optString("accessToken", "")
                jwtPayloadField(tok, "id") ?: randomUid()
            }
            else -> randomUid()
        }
    }

    /** 16 位随机 hex，对齐 cmd/login 的 genUUID 去横线取前 16 位。 */
    private fun randomUid(): String {
        val b = ByteArray(8)
        java.security.SecureRandom().nextBytes(b)
        val sb = StringBuilder(16)
        for (x in b) sb.append(String.format("%02x", x))
        return sb.toString()
    }

    /**
     * 从 JWT 的载荷里取 data.<field>。
     *
     * 只做 base64url 解码 + 正则取字段，不引 JSON 库——载荷一定是个对象，
     * 且这里只认 "id" 这一个字符串字段，用不着完整解析。
     */
    private fun jwtPayloadField(token: String, field: String): String? = runCatching {
        val parts = token.split(".")
        if (parts.size < 2) return null
        val payload = android.util.Base64.decode(
            parts[1], android.util.Base64.URL_SAFE or android.util.Base64.NO_PADDING or android.util.Base64.NO_WRAP
        ).toString(Charsets.UTF_8)
        // data.id 是数字串，也可能是字符串；两种都接
        Regex("\"$field\"\\s*:\\s*\"?([A-Za-z0-9_-]+)\"?").find(payload)
            ?.groupValues?.get(1)
    }.getOrNull()

    /**
     * 回填 auth.json 里空 uid 的账号，返回**新内容**（无需改动返回 null）。
     *
     * 为什么要单独一步：旧版本 App 添加的 deepseek/doubao 账号 uid 被写成了空串，
     * 光改 wrap() 救不回已经在文件里的条目。这里按 defaultUid 补上，
     * 嵌套形写 account.uid，扁平形写顶层 uid。
     *
     * 补完保证文件内 uid 互不重复（网关按 uid 建索引，重复同样会互相覆盖）。
     */
    fun repairUids(raw: String): String? {
        val root = runCatching { JSONObject(raw.trim().ifEmpty { "{}" }) }.getOrNull() ?: return null
        val seen = HashSet<String>()
        // 先把已有的非空 uid 占位，避免新补的撞上它们
        val it0 = root.keys()
        while (it0.hasNext()) {
            val e = root.optJSONObject(it0.next()) ?: continue
            val u = (e.optJSONObject("account")?.optString("uid", "")?.trim().orEmpty())
                .ifEmpty { e.optString("uid", "").trim() }
            if (u.isNotEmpty()) seen.add(u)
        }

        var changed = false
        val it = root.keys()
        while (it.hasNext()) {
            val key = it.next()
            val entry = root.optJSONObject(key) ?: continue
            val account = entry.optJSONObject("account")
            val cur = (account?.optString("uid", "")?.trim().orEmpty())
                .ifEmpty { entry.optString("uid", "").trim() }
            if (cur.isNotEmpty()) continue

            val providerId = Providers.detectProvider(entry)
            var uid = defaultUid(providerId, entry)
            while (!seen.add(uid)) uid = randomUid()

            // 嵌套形有条目就写子对象，扁平形写顶层
            if (account != null) account.put("uid", uid) else entry.put("uid", uid)
            changed = true
        }
        return if (changed) root.toString(2) else null
    }

    /** 把最简凭证包装成网关认识的完整条目。 */
    private fun wrap(raw: JSONObject, providerId: String, nickname: String): JSONObject {
        val entry = JSONObject()
        val auth = JSONObject()
        val account = JSONObject()
        val extra = JSONObject()
        val now = System.currentTimeMillis() / 1000

        when (providerId) {
            "deepseek" -> {
                auth.put("accessToken", raw.optString("accessToken", raw.optString("token", "")))
                auth.put("expiresAt", now + 30L * 86400)
            }
            "qoder" -> {
                val pat = raw.optString("pat", "")
                if (pat.isNotEmpty()) {
                    // PAT 路线：网关用 extra.pat 自己换 jobToken
                    extra.put("pat", pat)
                    auth.put("accessToken", raw.optString("accessToken", ""))
                } else {
                    auth.put("accessToken", raw.optString("accessToken", ""))
                    auth.put("refreshToken", raw.optString("refreshToken", ""))
                }
                auth.put("expiresAt", now + 86400)
            }
            "trae" -> {
                auth.put("accessToken", raw.optString("accessToken", ""))
                auth.put("expiresAt", now + 8L * 3600)
            }
            "doubao" -> {
                // sessionid 是 Cookie 里的核心登录态，网关当 accessToken 用
                val sid = raw.optString("sessionid", raw.optString("accessToken", ""))
                auth.put("accessToken", sid)
                auth.put("expiresAt", now + 30L * 86400)
                raw.optString("ttwid", "").takeIf { it.isNotEmpty() }?.let { extra.put("ttwid", it) }
            }
            else -> {
                // codebuddy：需要完整 OAuth 结构，原样透传
                val src = raw.optJSONObject("auth") ?: raw
                src.keys().forEach { auth.put(it, src.get(it)) }
            }
        }

        // uid 不能留空：网关拿它当账号池主键（internal/pool/pool.go 的 byUID），
        // 空 uid 会让多个号塌成同一个键、互相覆盖，账号级密钥也就无从挂起。
        account.put("uid", defaultUid(providerId, raw))
        account.put("nickname", nickname)

        entry.put("provider", providerId.removeSuffix("_intl"))
        entry.put("auth", auth)
        entry.put("account", account)
        if (extra.length() > 0) entry.put("extra", extra)
        return entry
    }
}
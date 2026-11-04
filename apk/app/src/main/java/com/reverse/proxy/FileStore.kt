package com.reverse.proxy

import android.content.Context
import java.io.File
import java.util.UUID

/**
 * App 私有目录里的落点。全部给 Go 侧传**绝对路径**——
 * Android 上进程工作目录不是 App 目录，配置里的相对路径会解析错。
 *
 *   files/config.json          网关配置
 *   files/auths/auth.json      账号凭证（顶层 map）
 *   files/data/state.json      账号池运行态（网关自己写）
 *   files/runtime/config.json  实际喂给 Go 的配置（路径已改写成绝对路径）
 */
object FileStore {

    fun configFile(ctx: Context) = File(ctx.filesDir, "config.json")
    fun authFile(ctx: Context) = File(ctx.filesDir, "auths/auth.json")
    fun dataDir(ctx: Context) = File(ctx.filesDir, "data")
    fun stateFile(ctx: Context) = File(dataDir(ctx), "state.json")
    fun runtimeDir(ctx: Context) = File(ctx.filesDir, "runtime")

    fun ensureLayout(ctx: Context) {
        configFile(ctx).parentFile?.mkdirs()
        authFile(ctx).parentFile?.mkdirs()
        dataDir(ctx).mkdirs()
        runtimeDir(ctx).mkdirs()
    }

    fun readConfig(ctx: Context): String = readOrInit(configFile(ctx)) { defaultConfig(ctx) }

    /** 初始是空 map：一个账号都没有也是合法状态。 */
    fun readAuth(ctx: Context): String = readOrInit(authFile(ctx)) { "{}" }

    fun writeConfig(ctx: Context, text: String) = write(configFile(ctx), text)
    fun writeAuth(ctx: Context, text: String) = write(authFile(ctx), text)

    /**
     * 把用户配置里的相对路径 / 缺省路径改写成 App 私有目录的绝对路径，
     * 写到 runtime/config.json，再把这份路径交给 Go。
     *
     * 这样用户在 App 里粘贴 PC 上的配置（auth_file 写成 "./auths/auth.json"）也能直接跑。
     */
    fun prepareRuntimeConfig(ctx: Context, rawConfig: String): File {
        val out = File(runtimeDir(ctx), "config.json")
        out.parentFile?.mkdirs()

        // 用 JSONObject 改键；解析失败就把原文原样写过去，让 Go 报出具体错误。
        val patched = runCatching {
            val o = org.json.JSONObject(rawConfig.trim().ifEmpty { "{}" })
            o.put("auth_file", authFile(ctx).absolutePath)
            o.put("state_file", stateFile(ctx).absolutePath)
            val ds = o.optJSONObject("deepseek") ?: org.json.JSONObject()
            ds.put("session_file", File(dataDir(ctx), "deepseek_sessions.json").absolutePath)
            o.put("deepseek", ds)
            o.toString(2)
        }.getOrDefault(rawConfig)
        out.writeText(patched)
        return out
    }

    /** 内置默认配置：只监听回环、定时任务全关（手机上跑签到没意义还耗电）。 */
    fun defaultConfig(ctx: Context): String {
        // 先算成局部变量：嵌套模板 + 嵌套字符串字面量在 raw string 里容易踩解析歧义。
        val authPath = authFile(ctx).absolutePath.replace('\\', '/')
        val statePath = stateFile(ctx).absolutePath.replace('\\', '/')
        val dsPath = File(dataDir(ctx), "deepseek_sessions.json").absolutePath.replace('\\', '/')
        val key = randomKey()
        return """
            {
              "listen": "127.0.0.1:7863",
              "api_key": "$key",
              "auth_file": "$authPath",
              "state_file": "$statePath",
              "deepseek": { "session_file": "$dsPath" },
              "schedule": {
                "checkin_enabled": false,
                "travel_enabled": false,
                "activity_enabled": false,
                "keepalive_enabled": false,
                "token_watch_enabled": true
              },
              "server": { "max_body_mb": 8 },
              "pool": { "max_in_flight": 3 },
              "session_sticky": { "enabled": true }
            }
        """.trimIndent()
    }

    /** 配置里的 listen；展示给用户时把 ":7863" / "0.0.0.0" 归一成 127.0.0.1。 */
    fun configuredListen(ctx: Context): String {
        val raw = runCatching {
            Regex("\"listen\"\\s*:\\s*\"([^\"]*)\"").find(readConfig(ctx))?.groupValues?.get(1)
        }.getOrNull().orEmpty()
        val port = when {
            raw.isEmpty() -> "7863"
            raw.contains(":") -> raw.substringAfterLast(':')
            else -> raw
        }
        return "127.0.0.1:" + port
    }

    fun apiKey(ctx: Context): String = runCatching {
        Regex("\"api_key\"\\s*:\\s*\"([^\"]*)\"").find(readConfig(ctx))?.groupValues?.get(1) ?: ""
    }.getOrDefault("")

    // ─── 账号级密钥（config.json 的 account_keys） ───────────────────────
    //
    // 结构对齐网关（cmd/server/config.go）：{账号标签: {uid: secret}}。
    // 一把 key 锁一个 uid —— handler.withAuth 拿它反查出 uid，请求只走那个号、
    // 不参与轮换。所以两个账号不可能共用同一把 key（config.go 的 normalize
    // 遇到重复会直接报错 "账户 key 必须唯一"）。

    /** 读出 account_keys；结构不对返回空表（网关按"无账号级密钥"处理，不影响启动）。 */
    fun readAccountKeys(ctx: Context): org.json.JSONObject = runCatching {
        org.json.JSONObject(readConfig(ctx).trim().ifEmpty { "{}" })
            .optJSONObject("account_keys") ?: org.json.JSONObject()
    }.getOrDefault(org.json.JSONObject())

    /**
     * 把整张 account_keys 写回 config.json。
     *
     * 只替换 account_keys 这一个键，其余配置原样保留 —— 用户可能手改过
     * listen / models / schedule，不能被 App 顺手覆盖掉。
     */
    fun writeAccountKeys(ctx: Context, keys: org.json.JSONObject): Boolean = runCatching {
        val root = org.json.JSONObject(readConfig(ctx).trim().ifEmpty { "{}" })
        root.put("account_keys", keys)
        writeConfig(ctx, root.toString(2))
        true
    }.getOrDefault(false)

    /**
     * 生成一把账号级密钥。
     *
     * 与 gen_account_keys.py 的 secrets.token_hex(32) 同口径：32 字节随机 → 64 位小写 hex，
     * 这样 PC 侧生成的 key 与 App 侧生成的肉眼一致、可互相替换。
     */
    fun newAccountKey(): String {
        val b = ByteArray(32)
        java.security.SecureRandom().nextBytes(b)
        val sb = StringBuilder(64)
        for (x in b) sb.append(String.format("%02x", x))
        return sb.toString()
    }

    private fun randomKey(): String =
        "sk-" + UUID.randomUUID().toString().replace("-", "") + UUID.randomUUID().toString().take(8)

    private inline fun readOrInit(f: File, init: () -> String): String {
        if (!f.exists()) {
            f.parentFile?.mkdirs()
            val t = init()
            if (t.isNotEmpty()) f.writeText(t)
            return t
        }
        return runCatching { f.readText() }.getOrDefault("")
    }

    private fun write(f: File, text: String) {
        f.parentFile?.mkdirs()
        f.writeText(text)
    }
}
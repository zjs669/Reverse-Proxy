package com.reverse.proxy

/**
 * 各供应商的最简凭证格式与获取方式。
 *
 * 这里的格式直接对应 cmd/login/main.go 里 readPastedJSON 能接受的输入，
 * 不是编的 —— 用户照着粘贴，网关一定能解析。
 */
data class ProviderGuide(
    val id: String,
    val name: String,
    val tagline: String,
    val url: String?,
    /** 最简凭证 JSON（直接可粘）。 */
    val sample: String,
    /** 去哪拿，2-3 步。 */
    val steps: List<String>,
    val expiry: String?,
)

object Providers {

    val all: List<ProviderGuide> = listOf(
        ProviderGuide(
            id = "deepseek",
            name = "DeepSeek",
            tagline = "网页版账号，token 以 mb5+ 开头",
            url = "https://chat.deepseek.com",
            sample = "{ \"accessToken\": \"mb5+...\" }",
            steps = listOf(
                "浏览器登录 chat.deepseek.com",
                "F12 → Application → Local Storage → userToken",
                "复制 value 字段（mb5+ 开头那串）",
            ),
            expiry = "约 30 天，过期需重新复制",
        ),
        ProviderGuide(
            id = "qoder",
            name = "Qoder",
            tagline = "只给 PAT，App 去换 jobToken",
            url = "https://qoder.com.cn/account/integrations",
            sample = "{ \"pat\": \"pt-…\" }",
            steps = listOf(
                "打开上面的网址，创建 Personal Access Token",
                "复制 pt- 开头的串，粘到下面",
                "uid 与机器指纹会自动补，不用手填",
            ),
            expiry = "PAT 长期有效，jobToken 自动续",
        ),
        ProviderGuide(
            id = "trae",
            name = "Trae",
            // 只需要 sourceId：拿到它 App 会去上游换 JWT，用户不用掐 8 小时
            tagline = "只给登录会话 sourceId，JWT 自动换",
            url = "https://www.trae.cn",
            sample = "{ \"sourceId\": \"<X-Cloudide-Session 的值>\" }",
            steps = listOf(
                "浏览器登录 trae.cn，F12 → Network",
                "随便点一个请求，找请求头 X-Cloudide-Session",
                "复制它的值粘过来（长期有效，不用管 JWT 过期）",
            ),
            expiry = "会话长期有效；换来的 JWT 8h，网关自动续",
        ),
        ProviderGuide(
            id = "doubao",
            name = "豆包",
            tagline = "用账号 Cookie 里的 sessionid",
            url = "https://www.doubao.com",
            sample = "{ \"sessionid\": \"...\" }",
            steps = listOf(
                "浏览器登录豆包，F12 → Application → Cookies",
                "找 sessionid 的值",
                "或从任意请求的 Cookie 头里复制 sessionid=xxx 的 xxx",
            ),
            expiry = "约 30 天",
        ),
        ProviderGuide(
            id = "codebuddy",
            name = "CodeBuddy / WorkBuddy",
            // 设备流：点下面的按钮现场生成授权链接，不用去 PC 跑 ./login
            tagline = "点按钮生成授权链接，浏览器登录即完成",
            url = "https://www.codebuddy.cn",
            sample = "",
            steps = listOf(
                "点下面的「生成授权链接」",
                "在浏览器打开它，用要接入的账号登录并授权",
                "回到这里等几秒，会自动收号",
            ),
            expiry = "按 OAuth 返回，自动续期",
        ),
        ProviderGuide(
            id = "codebuddy_intl",
            name = "CodeBuddy 国际版",
            tagline = "codebuddy.ai，账号与国内版独立",
            url = "https://www.codebuddy.ai",
            sample = "",
            steps = listOf(
                "点「生成授权链接」（走 codebuddy.ai 端点）",
                "浏览器登录授权后自动收号",
            ),
            expiry = "按 OAuth 返回，自动续期",
        ),
    )

    fun byId(id: String): ProviderGuide? = all.firstOrNull { it.id == id }

    /** 从 auth.json 条目推断 provider（与 internal/auth 的 ProviderOrDefault 一致）。 */
    fun detectProvider(entry: org.json.JSONObject): String {
        val explicit = entry.optString("provider", "").trim()
        val domain = entry.optJSONObject("auth")?.optString("domain", "") ?: ""
        if (explicit == "codebuddy" || explicit.isEmpty()) {
            return if (domain.contains("codebuddy.ai")) "codebuddy_intl" else "codebuddy"
        }
        return explicit
    }
}
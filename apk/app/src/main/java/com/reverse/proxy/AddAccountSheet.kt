package com.reverse.proxy

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.foundation.layout.size
import androidx.compose.ui.platform.LocalContext

/**
 * 添加账号：选供应商 → 看怎么拿 → 粘凭证。
 *
 * 各家凭证形态差别很大（DeepSeek 是 mb5+ token，Qoder 是 pt- PAT，
 * 豆包是 sessionid，CodeBuddy 走 OAuth），所以必须先选后填。
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AddAccountSheet(
    onDismiss: () -> Unit,
    /** 返回 null = 成功；返回字符串 = 错误信息。 */
    onAdd: (providerId: String, pasted: String, nickname: String) -> String?,
) {
    var step by remember { mutableIntStateOf(0) }
    var guide by remember { mutableStateOf<ProviderGuide?>(null) }
    var pasted by remember { mutableStateOf("") }
    var nickname by remember { mutableStateOf("") }
    var err by remember { mutableStateOf("") }

    // 非空 = 这家要走"App 替用户换 token"，不能直接粘凭证
    //   src  → trae：用户只给 sourceId
    //   pat  → qoder：用户只给 PAT
    //   oauth→ codebuddy：点按钮生成授权链接，轮询收号
    var mode by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var authUrl by remember { mutableStateOf("") }
    var state by remember { mutableStateOf("") }

    val ctx = androidx.compose.ui.platform.LocalContext.current
    // 网络动作统一投到组合作用域，随弹窗销毁一起取消，不能用 GlobalScope 泄漏
    val scope = rememberCoroutineScope()

    fun resetFor(p: ProviderGuide) {
        mode = when (p.id) {
            "trae" -> "src"
            "qoder" -> "pat"
            "codebuddy", "codebuddy_intl" -> "oauth"
            else -> ""
        }
        pasted = if (mode.isEmpty()) p.sample else ""
        authUrl = ""
        state = ""
        busy = false
        err = ""
    }

    AlertDialog(
        onDismissRequest = onDismiss,
        confirmButton = {},
        text = {
            Column(
                Modifier.fillMaxWidth().heightIn(max = 520.dp).verticalScroll(rememberScrollState()),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                if (step == 0) {
                    Text("选择供应商", fontSize = 16.sp)
                    Providers.all.forEach { p ->
                        // 用 clickable 而不是 Card(onClick)：后者在 verticalScroll 容器里
                        // 会被滚动手势拦截，点了没反应。
                        Column(
                            Modifier
                                .fillMaxWidth()
                                .clickable {
                                    guide = p
                                    nickname = ""
                                    err = ""
                                    resetFor(p)
                                    step = 1
                                }
                                .background(cardColor(), RoundedCornerShape(8.dp))
                                .padding(12.dp),
                            verticalArrangement = Arrangement.spacedBy(2.dp),
                        ) {
                            Text(p.name, fontSize = 14.sp)
                            Text(p.tagline, fontSize = 11.sp, color = dimColor())
                        }
                    }
                    TextButton(onClick = onDismiss, modifier = Modifier.fillMaxWidth()) { Text("取消") }
                } else {
                    val p = guide!!
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        TextButton(onClick = { step = 0 }) { Text("←", fontSize = 16.sp) }
                        Text(p.name, fontSize = 15.sp)
                    }
                    Text(p.tagline, fontSize = 11.sp, color = dimColor())

                    Text("怎么拿", fontSize = 12.sp)
                    p.url?.let {
                        Box(
                            Modifier.fillMaxWidth()
                                .background(cardColor(), RoundedCornerShape(6.dp))
                                .padding(8.dp),
                        ) {
                            Text(it, fontSize = 11.sp, fontFamily = FontFamily.Monospace,
                                color = MaterialTheme.colorScheme.primary)
                        }
                    }
                    p.steps.forEachIndexed { i, s ->
                        Row {
                            Text((i + 1).toString() + ".", fontSize = 11.sp,
                                color = dimColor(), modifier = Modifier.width(16.dp))
                            Text(s, fontSize = 11.sp)
                        }
                    }
                    p.expiry?.let { Text("有效期：" + it, fontSize = 11.sp, color = dimColor()) }

                    // 备注：三家都放最上面，轮询动画/报错都在它下面
                    OutlinedTextField(
                        value = nickname,
                        onValueChange = { nickname = it },
                        modifier = Modifier.fillMaxWidth(),
                        placeholder = { Text("备注（可选）", fontSize = 12.sp) },
                        singleLine = true,
                        textStyle = MonoSm,
                    )

                    when (mode) {
                        // ── trae：只要 sourceId ──
                        "src" -> {
                            Text("粘 sourceId", fontSize = 12.sp)
                            OutlinedTextField(
                                value = pasted,
                                onValueChange = { pasted = it; err = "" },
                                modifier = Modifier.fillMaxWidth(),
                                placeholder = { Text("X-Cloudide-Session 的值", fontSize = 11.sp) },
                                textStyle = MonoSm,
                                singleLine = true,
                                isError = err.isNotEmpty(),
                            )
                        }
                        // ── qoder：只要 PAT ──
                        "pat" -> {
                            Text("粘 PAT", fontSize = 12.sp)
                            OutlinedTextField(
                                value = pasted,
                                onValueChange = { pasted = it; err = "" },
                                modifier = Modifier.fillMaxWidth(),
                                placeholder = { Text("pt- 开头", fontSize = 11.sp) },
                                textStyle = MonoSm,
                                singleLine = true,
                                isError = err.isNotEmpty(),
                            )
                        }
                        // ── codebuddy：设备流，不粘任何东西 ──
                        "oauth" -> {
                            if (authUrl.isEmpty()) {
                                Text(
                                    "点下面的按钮，App 会向上游要一条一次性授权链接。",
                                    fontSize = 11.sp, color = dimColor(),
                                )
                            } else {
                                Text("1. 在浏览器打开这条链接并完成登录：", fontSize = 11.sp)
                                Box(
                                    Modifier.fillMaxWidth()
                                        .background(cardColor(), RoundedCornerShape(6.dp))
                                        .padding(8.dp),
                                ) {
                                    Text(authUrl, fontSize = 10.sp, fontFamily = FontFamily.Monospace)
                                }
                                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                                    OutlinedButton(
                                        onClick = {
                                            val i = android.content.Intent(
                                                android.content.Intent.ACTION_VIEW,
                                                android.net.Uri.parse(authUrl),
                                            )
                                            runCatching { ctx.startActivity(i) }
                                        },
                                        modifier = Modifier.weight(1f),
                                    ) { Text("在浏览器打开", fontSize = 12.sp) }
                                    OutlinedButton(
                                        onClick = {
                                            val cm = ctx.getSystemService(android.content.ClipboardManager::class.java)
                                            cm.setPrimaryClip(
                                                android.content.ClipData.newPlainText("auth", authUrl)
                                            )
                                        },
                                        modifier = Modifier.weight(1f),
                                    ) { Text("复制链接", fontSize = 12.sp) }
                                }
                                Text(
                                    "2. 登录完回到这里，App 每 3 秒自动问一次，收到就结束。",
                                    fontSize = 11.sp, color = dimColor(),
                                )
                            }
                        }
                        // ── 其他家：保持原样，直接粘 JSON ──
                        else -> {
                            Text("粘到这里", fontSize = 12.sp)
                            OutlinedTextField(
                                value = pasted,
                                onValueChange = { pasted = it; err = "" },
                                modifier = Modifier.fillMaxWidth().height(120.dp),
                                textStyle = MonoSm,
                                isError = err.isNotEmpty(),
                            )
                        }
                    }

                    if (busy) {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            CircularProgressIndicator(
                                modifier = Modifier.size(14.dp), strokeWidth = 2.dp,
                            )
                            Spacer(Modifier.width(8.dp))
                            Text(
                                if (mode == "oauth") "等待浏览器授权…" else "正在向上游换凭证…",
                                fontSize = 11.sp, color = dimColor(),
                            )
                        }
                    }
                    if (err.isNotEmpty()) Text(err, fontSize = 11.sp, color = badColor())

                    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        OutlinedButton(
                            onClick = onDismiss,
                            modifier = Modifier.weight(1f),
                        ) { Text("取消") }

                        Button(
                            enabled = !busy,
                            onClick = {
                                fun submit(entry: org.json.JSONObject) {
                                    // 换好的条目已经是完整形态，直接交给上层写入
                                    onAdd(p.id, entry.toString(), nickname.trim())?.let { err = it }
                                }

                                when (mode) {
                                    "src" -> {
                                        busy = true; err = ""
                                        // 网络不能放主线程
                                        scope.launch(Dispatchers.IO) {
                                            val r = ProvidersNet.traeFromSourceId(pasted, nickname.trim())
                                            kotlinx.coroutines.withContext(kotlinx.coroutines.Dispatchers.Main) {
                                                busy = false
                                                r.fold(
                                                    onSuccess = { submit(it) },
                                                    onFailure = { err = it.message ?: "换令牌失败" },
                                                )
                                            }
                                        }
                                    }
                                    "pat" -> {
                                        busy = true; err = ""
                                        scope.launch(Dispatchers.IO) {
                                            val r = ProvidersNet.qoderFromPat(pasted, nickname.trim())
                                            withContext(Dispatchers.Main) {
                                                busy = false
                                                r.fold(
                                                    onSuccess = { submit(it) },
                                                    onFailure = { err = it.message ?: "PAT 兑换失败" },
                                                )
                                            }
                                        }
                                    }
                                    "oauth" -> {
                                        busy = true; err = ""
                                        val intl = p.id == "codebuddy_intl"
                                        scope.launch(Dispatchers.IO) {
                                            val start = ProvidersNet.codebuddyAuthUrl(intl)
                                            withContext(Dispatchers.Main) {
                                                start.fold(
                                                    onSuccess = { (st, url) ->
                                                        state = st
                                                        authUrl = url
                                                    },
                                                    onFailure = {
                                                        busy = false
                                                        err = it.message ?: "生成授权链接失败"
                                                    },
                                                )
                                            }
                                            if (state.isEmpty()) return@launch

                                            // 轮询：最多 30 次 × 3s = 90s
                                            for (i in 1..30) {
                                                kotlinx.coroutines.delay(3000)
                                                val got = ProvidersNet.codebuddyPoll(intl, state, nickname.trim())
                                                if (got.isSuccess) {
                                                    val entry = got.getOrNull()
                                                    if (entry != null) {
                                                        kotlinx.coroutines.withContext(kotlinx.coroutines.Dispatchers.Main) {
                                                            busy = false
                                                            submit(entry)
                                                        }
                                                        return@launch
                                                    }
                                                } else {
                                                    kotlinx.coroutines.withContext(kotlinx.coroutines.Dispatchers.Main) {
                                                        busy = false
                                                        err = got.exceptionOrNull()?.message ?: "收号失败"
                                                    }
                                                    return@launch
                                                }
                                            }
                                            kotlinx.coroutines.withContext(kotlinx.coroutines.Dispatchers.Main) {
                                                busy = false
                                                err = "等了 90 秒还没收到授权。确认浏览器里登录完成了吗？可以再点一次重试。"
                                            }
                                        }
                                    }
                                    else -> onAdd(p.id, pasted, nickname.trim())?.let { err = it }
                                }
                            },
                            modifier = Modifier.weight(1f),
                        ) {
                            Text(
                                when (mode) {
                                    "src" -> "换令牌并保存"
                                    "pat" -> "兑换并保存"
                                    "oauth" -> if (authUrl.isEmpty()) "生成授权链接" else "重新生成"
                                    else -> "保存"
                                }
                            )
                        }
                    }
                }
            }
        },
    )
}
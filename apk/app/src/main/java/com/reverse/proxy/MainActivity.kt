package com.reverse.proxy

import android.Manifest
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.provider.Settings
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONObject

class MainActivity : ComponentActivity() {

    private var onImported: ((String) -> Unit)? = null

    private val pickFile = registerForActivityResult(ActivityResultContracts.OpenDocument()) { uri: Uri? ->
        if (uri == null) return@registerForActivityResult
        val t = runCatching {
            contentResolver.openInputStream(uri)?.bufferedReader()?.use { it.readText() }
        }.getOrNull() ?: return@registerForActivityResult
        onImported?.invoke(t)
    }

    override fun onCreate(s: Bundle?) {
        super.onCreate(s)
        FileStore.ensureLayout(this)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 1)
        }
        setContent {
            val modeState = remember { mutableIntStateOf(Preference.theme(this)) }
            AppTheme(modeState.intValue) {
                App(modeState.intValue) { picked ->
                    modeState.intValue = picked
                    Preference.setTheme(this, picked)
                }
            }
        }
    }

    @OptIn(ExperimentalMaterial3Api::class)
    @Composable
    private fun App(themeMode: Int, onThemeChange: (Int) -> Unit) {
        val ctx = LocalContext.current
        val st by ProxyService.state.collectAsStateWithLifecycle()
        var tab by remember { mutableIntStateOf(0) }

        Scaffold(
            topBar = {
                TopAppBar(
                    title = {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Dot(st.running)
                            Spacer(Modifier.width(8.dp))
                            Text(
                                if (st.running) st.addr else "未运行",
                                fontFamily = FontFamily.Monospace, fontSize = 13.sp,
                            )
                        }
                    },
                    actions = {
                        if (st.running) {
                            TextButton(onClick = { ProxyService.stop(ctx) }) { Text("停止") }
                        } else {
                            TextButton(onClick = { ProxyService.start(ctx) }) { Text("启动") }
                        }
                        TextButton(onClick = {
                            if (OverlayController.canDraw(ctx)) {
                                OverlayController.toggle(ctx, !OverlayController.enabled)
                            } else {
                                runCatching {
                                    ctx.startActivity(
                                        Intent(
                                            Settings.ACTION_MANAGE_OVERLAY_PERMISSION,
                                            Uri.parse("package:" + ctx.packageName),
                                        )
                                    )
                                }
                            }
                        }) { Text("悬浮窗", fontSize = 12.sp) }
                    },
                )
            },
            bottomBar = {
                NavigationBar {
                    NavigationBarItem(tab == 0, { tab = 0 }, icon = {}, label = { Text("主页") })
                    NavigationBarItem(tab == 1, { tab = 1 }, icon = {}, label = { Text("账号") })
                    NavigationBarItem(tab == 2, { tab = 2 }, icon = {}, label = { Text("日志") })
                }
            },
        ) { pad ->
            Box(Modifier.padding(pad).fillMaxSize()) {
                when (tab) {
                    0 -> HomeTab(st, themeMode, onThemeChange)
                    1 -> AccountTab { cb -> onImported = cb; pickFile.launch(arrayOf("*/*")) }
                    else -> LogTab()
                }
            }
        }
    }

    // ─── 主页 ──────────────────────────────────────────────────────────────

    @OptIn(ExperimentalMaterial3Api::class)
    @Composable
    private fun HomeTab(st: ProxyService.State, themeMode: Int, onThemeChange: (Int) -> Unit) {
        val ctx = LocalContext.current
        val key = FileStore.apiKey(ctx)
        val addr = st.addr.ifEmpty { FileStore.configuredListen(ctx) }
        val base = "http://" + addr + "/v1"
        var info by remember { mutableStateOf("") }
        val scope = rememberCoroutineScope()
        // 账号数只用于文案，不参与鉴权判断；服务没跑也要正确（用户可能在加号后才启动）
        val accountCount by produceState(0) {
            value = withContext(Dispatchers.IO) {
                AuthStore.parse(FileStore.readAuth(ctx)).size
            }
        }

        // LaunchedEffect 跑在主线程；GatewayApi 是阻塞 HTTP（3s 超时），
        // 直接调会把主线程按住 → 系统弹「应用无响应」。必须切到 IO。
        LaunchedEffect(st.running, st.addr) {
            if (!st.running) { info = ""; return@LaunchedEffect }
            while (true) {
                val addrNow = st.addr
                val o = withContext(Dispatchers.IO) { GatewayApi.status(addrNow, FileStore.apiKey(ctx)) }
                val local = withContext(Dispatchers.IO) {
                    AuthStore.parse(FileStore.readAuth(ctx)).size
                }
                info = o?.let { GatewayApi.summarize(it, local) } ?: ""
                delay(3000)
            }
        }

        Column(
            Modifier.padding(14.dp).fillMaxSize().verticalScroll(rememberScrollState()),
            verticalArrangement = Arrangement.spacedBy(10.dp),
        ) {
            if (st.lastError.isNotEmpty()) {
                Text(st.lastError, color = badColor(), fontSize = 12.sp)
            }
            if (info.isNotEmpty()) {
                Text(info, fontSize = 11.sp, color = dimColor())
            }

            Card(colors = CardDefaults.cardColors(containerColor = cardColor())) {
                Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    CopyRow("地址", addr) { copy(ctx, addr) }
                    CopyRow("Base URL", base) { copy(ctx, base) }
                    CopyRow("总密钥", key) { copy(ctx, key) }
                    Text(
                        if (accountCount > 1)
                            "两种密钥，别混用：\n" +
                                "· 总密钥 = 上面这把，可调度全部 " + accountCount +
                                " 个号，按池轮换（客户端什么都不用改，随便换号用它）\n" +
                                "· 专属密钥 = 每个账号各一把、互不通用，只能调那一个号。" +
                                "要固定用某个号，去「账号」页展开那张卡复制。"
                        else
                            "总密钥可调度全部账号。账号之间密钥互不通用：专属密钥在「账号」页展开卡片复制。",
                        fontSize = 10.sp, color = dimColor(),
                    )
                }
            }

            OutlinedButton(
                onClick = {
                    // 同上：healthz 是阻塞 HTTP，不能在点击回调（主线程）里直接调。
                    scope.launch {
                        val ok = withContext(Dispatchers.IO) { GatewayApi.healthz(st.addr) }
                        info = if (ok) "自测通过" else "自测失败"
                    }
                },
                enabled = st.running,
                modifier = Modifier.fillMaxWidth(),
            ) { Text("自测") }

            Card(colors = CardDefaults.cardColors(containerColor = cardColor())) {
                Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                    Text("客户端配置", fontSize = 13.sp)
                    Code("base_url = " + base)
                    Code("api_key  = " + key)
                }
            }

            Row(verticalAlignment = Alignment.CenterVertically) {
                var auto by remember { mutableStateOf(Preference.autostart(ctx)) }
                Switch(checked = auto, onCheckedChange = { auto = it; Preference.setAutostart(ctx, it) })
                Spacer(Modifier.width(8.dp))
                Text("开机自启", fontSize = 13.sp)
            }
            TextButton(onClick = {
                runCatching {
                    ctx.startActivity(
                        Intent(
                            Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
                            Uri.parse("package:" + ctx.packageName),
                        )
                    )
                }
            }) { Text("忽略电池优化", fontSize = 12.sp) }

            // 主题：跟随系统 / 浅色 / 深色
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text("主题", fontSize = 13.sp, modifier = Modifier.width(52.dp))
                SingleChoiceSegmentedButtonRow {
                    listOf("跟随系统", "浅色", "深色").forEachIndexed { i, label ->
                        SegmentedButton(
                            selected = themeMode == i,
                            onClick = { onThemeChange(i) },
                            shape = SegmentedButtonDefaults.itemShape(index = i, count = 3),
                        ) { Text(label, fontSize = 11.sp) }
                    }
                }
            }
        }
    }

    // ─── 账号 ──────────────────────────────────────────────────────────────

    @Composable
    private fun AccountTab(pick: ((String) -> Unit) -> Unit) {
        val ctx = LocalContext.current
        var raw by remember { mutableStateOf(FileStore.readAuth(ctx)) }
        var keys by remember { mutableStateOf(FileStore.readAccountKeys(ctx)) }
        var adding by remember { mutableStateOf(false) }
        val list = remember(raw, keys) { AuthStore.parse(raw, keys) }
        val st by ProxyService.state.collectAsStateWithLifecycle()

        // 账号一变就做两件事，顺序不能反：
        //   1) 回填空 uid（网关按 uid 建索引、账号级密钥也按 uid 挂）
        //   2) 再对齐 account_keys
        // 只以 raw 为 key，避免 keys 变化又触发自己造成循环。
        LaunchedEffect(raw) {
            var touched = false
            val fixed = withContext(Dispatchers.IO) {
                AuthStore.repairUids(raw)?.also { FileStore.writeAuth(ctx, it) }
            }
            if (fixed != null) {
                raw = fixed
                touched = true
            }

            val next = withContext(Dispatchers.IO) {
                AuthStore.ensureKeys(raw, FileStore.readAccountKeys(ctx))
                    ?.takeIf { FileStore.writeAccountKeys(ctx, it) }
            }
            if (next != null) {
                keys = next
                touched = true
            }
            // 网关只在启动时读 auth.json / account_keys，改了就重载
            if (touched && ProxyService.state.value.running) ProxyService.restart(ctx)
        }

        if (adding) {
            AddAccountSheet(
                onDismiss = { adding = false },
                onAdd = { pid, pasted, nick ->
                    AuthStore.addAccount(raw, pasted, pid, nick).fold(
                        onSuccess = {
                            FileStore.writeAuth(ctx, it)
                            raw = it
                            adding = false
                            if (st.running) ProxyService.restart(ctx)
                            null
                        },
                        onFailure = { it.message ?: "添加失败" },
                    )
                },
            )
        }

        Column(
            Modifier.padding(14.dp).fillMaxSize().verticalScroll(rememberScrollState()),
            verticalArrangement = Arrangement.spacedBy(10.dp),
        ) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(list.size.toString() + " 个账号", fontSize = 13.sp, color = dimColor())
                Spacer(Modifier.weight(1f))
                Button(onClick = { adding = true }, contentPadding = PaddingValues(horizontal = 14.dp)) {
                    Text("添加")
                }
            }

            if (list.isEmpty()) {
                Card(colors = CardDefaults.cardColors(containerColor = cardColor())) {
                    Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                        Text("还没有账号", fontSize = 14.sp)
                        Text(
                            "点「添加」，选一个供应商，页面会告诉你去哪拿凭证。",
                            fontSize = 12.sp, color = dimColor(),
                        )
                    }
                }
            } else {
                list.forEach { acc ->
                    AccCard(acc, st.running, onDelete = {
                        AuthStore.removeAccount(raw, acc.key).getOrNull()?.let {
                            FileStore.writeAuth(ctx, it)
                            raw = it
                            if (st.running) ProxyService.restart(ctx)
                        }
                    }, onRefresh = { raw = it })
                }
            }

            Spacer(Modifier.height(6.dp))
            OutlinedButton(
                onClick = { pick { t -> raw = t; FileStore.writeAuth(ctx, t) } },
                modifier = Modifier.fillMaxWidth(),
            ) { Text("从文件导入 auth.json", fontSize = 12.sp) }
        }
    }

    @OptIn(ExperimentalMaterial3Api::class)
    @Composable
    private fun AccCard(acc: Account, running: Boolean, onDelete: () -> Unit, onRefresh: (String) -> Unit) {
        val ctx = LocalContext.current
        var open by remember { mutableStateOf(false) }
        // null = 还没拉过（不主动拉）；空表 = 拉过但没有；非空 = 拉到模型
        var models by remember { mutableStateOf<List<String>?>(null) }
        var revealed by remember { mutableStateOf(false) }
        var loading by remember { mutableStateOf(false) }
        var showModels by remember { mutableStateOf(true) }
        var renewing by remember { mutableStateOf(false) }
        var renewMsg by remember { mutableStateOf("") }
        val scope = rememberCoroutineScope()

        Card(
            onClick = { open = !open },
            colors = CardDefaults.cardColors(containerColor = cardColor()),
            modifier = Modifier.fillMaxWidth(),
        ) {
            Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text(acc.providerName, fontSize = 14.sp)
                    Spacer(Modifier.width(8.dp))
                    Expiry(acc)
                }
                // 第二行：账号标签（= auth.json 的键，如 qoder_1），方便直接对照配置文件；
                // 用户填了备注就把它跟在后面。以前这里优先显示 uid，一串十六进制看不出是哪个号。
                Text(
                    acc.key + if (acc.nickname.isNotEmpty() && acc.nickname != acc.key) "  ·  " + acc.nickname else "",
                    fontSize = 11.sp, fontFamily = FontFamily.Monospace, color = dimColor(),
                )

                if (open) {
                    HorizontalDivider(Modifier.padding(vertical = 4.dp))

                    // ── 该账号专属密钥 ──
                    // 一把 key 锁一个 uid：填进客户端后请求只走这个号，不参与池内轮换。
                    if (acc.apiKey.isEmpty()) {
                        Text(
                            if (acc.uid.isEmpty())
                                "这个账号缺 uid，配不了专属密钥（网关按 uid 反查）。请重新导入带 uid 的凭证。"
                            else "专属密钥生成中…",
                            fontSize = 11.sp, color = badColor(),
                        )
                    } else {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Text("专属密钥", fontSize = 12.sp)
                            Spacer(Modifier.weight(1f))
                            TextButton(onClick = { revealed = !revealed }) {
                                Text(if (revealed) "隐藏" else "显示", fontSize = 11.sp)
                            }
                            TextButton(onClick = { copy(ctx, acc.apiKey) }) {
                                Text("复制", fontSize = 11.sp)
                            }
                        }
                        Text(
                            if (revealed) acc.apiKey
                            else "•".repeat(32) + "（" + acc.apiKey.take(6) + "…" + acc.apiKey.takeLast(4) + "）",
                            fontSize = 10.sp, fontFamily = FontFamily.Monospace, color = dimColor(),
                        )

                    }

                    // ── 手动续期（只 trae / qoder 有自助续期能力）──
                    // 网关自己每 10 分钟巡检一次，但用户看不到也等不及；
                    // 这里给一个"现在就换"的按钮，走的是和 token-watch 完全相同的协议。
                    if (acc.provider.removeSuffix("_intl") == "trae" || acc.provider.removeSuffix("_intl") == "qoder") {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Text("续期", fontSize = 12.sp)
                            Spacer(Modifier.weight(1f))
                            if (renewing) {
                                CircularProgressIndicator(
                                    modifier = Modifier.size(14.dp), strokeWidth = 2.dp,
                                )
                                Spacer(Modifier.width(8.dp))
                            }
                            TextButton(
                                enabled = !renewing,
                                onClick = {
                                    renewing = true; renewMsg = ""
                                    scope.launch {
                                        val msg = withContext(Dispatchers.IO) {
                                            // 现读磁盘：AccCard 拿不到父级 raw，而且续期前可能刚改过
                                            val diskRaw = FileStore.readAuth(ctx)
                                            val entry = runCatching {
                                                org.json.JSONObject(diskRaw).optJSONObject(acc.key)
                                            }.getOrNull()
                                            if (entry == null) return@withContext "读不到该账号条目"
                                            val r = if (acc.provider.removeSuffix("_intl") == "trae")
                                                ProvidersNet.renewTrae(entry)
                                            else ProvidersNet.renewQoder(entry)
                                            r.fold(
                                                onSuccess = { rn ->
                                                    AuthStore.applyRenewed(diskRaw, acc.key, rn).fold(
                                                        onSuccess = {
                                                            FileStore.writeAuth(ctx, it)
                                                            onRefresh(it)
                                                            if (ProxyService.state.value.running) ProxyService.restart(ctx)
                                                            "已续期，新凭证有效期至 " +
                                                                java.text.SimpleDateFormat("MM-dd HH:mm", java.util.Locale.getDefault())
                                                                    .format(java.util.Date(rn.expiresAt * 1000))
                                                        },
                                                        onFailure = { "写回失败：" + it.message },
                                                    )
                                                },
                                                onFailure = { "续期失败：" + (it.message ?: "未知错误") },
                                            )
                                        }
                                        renewMsg = msg
                                        renewing = false
                                    }
                                },
                            ) {
                                Text(if (renewing) "续期中…" else "立即续期", fontSize = 11.sp)
                            }
                        }
                        if (renewMsg.isNotEmpty()) {
                            Text(
                                renewMsg,
                                fontSize = 10.sp,
                                color = if (renewMsg.startsWith("已续期")) goodColor() else badColor(),
                            )
                        }
                    }

                    HorizontalDivider(Modifier.padding(vertical = 4.dp))

                    // ── 模型列表：点按钮才拉，不点不发请求 ──
                    // 用该账号自己的专属密钥去问：网关按密钥反查 uid，只返回这个号的模型。
                    // 用总密钥会拿到全池模型，对"这号能用什么"没有参考价值。
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Text("模型", fontSize = 12.sp)
                        Spacer(Modifier.weight(1f))
                        if (loading) {
                            CircularProgressIndicator(
                                modifier = Modifier.size(14.dp), strokeWidth = 2.dp,
                            )
                            Spacer(Modifier.width(8.dp))
                        }
                        if (models != null) {
                            TextButton(onClick = { showModels = !showModels }) {
                                Text(if (showModels) "收起" else "展开", fontSize = 11.sp)
                            }
                        }
                        TextButton(
                            enabled = !loading && acc.apiKey.isNotEmpty(),
                            onClick = {
                                loading = true
                                scope.launch {
                                    val a = ProxyService.state.value.addr
                                    val k = acc.apiKey
                                    val got = withContext(Dispatchers.IO) { GatewayApi.models(a, k) }
                                    models = got
                                    showModels = true
                                    loading = false
                                }
                            },
                        ) {
                            Text(if (models == null) "获取" else "重新获取", fontSize = 11.sp)
                        }
                    }
                    when {
                        models == null ->
                            Text(
                                if (acc.apiKey.isEmpty()) "先生成专属密钥才能单独查这个号的模型"
                                else "点「获取」拉取（不点不发请求）",
                                fontSize = 10.sp, color = dimColor(),
                            )
                        models!!.isEmpty() ->
                            Text(
                                if (!ProxyService.state.value.running) "服务未运行，先启动网关"
                                else "这个号没有可用模型",
                                fontSize = 10.sp, color = dimColor(),
                            )
                        showModels -> {
                            Text(models!!.size.toString() + " 个模型", fontSize = 10.sp, color = dimColor())
                            models!!.forEach { m ->
                                Text(m, fontSize = 11.sp, fontFamily = FontFamily.Monospace)
                            }
                        }
                        else -> Text(models!!.size.toString() + " 个模型（已收起）", fontSize = 10.sp, color = dimColor())
                    }
                    TextButton(onClick = onDelete) {
                        Text("删除", color = badColor(), fontSize = 12.sp)
                    }
                }
            }
        }
    }

    // ─── 日志 ──────────────────────────────────────────────────────────────

    @Composable
    private fun LogTab() {
        val lines by LogBus.lines.collectAsStateWithLifecycle()
        val ctx = LocalContext.current
        val scroll = rememberScrollState()
        var follow by remember { mutableStateOf(true) }

        LaunchedEffect(lines.size, follow) {
            if (follow && lines.isNotEmpty()) scroll.scrollTo(scroll.maxValue)
        }

        Column(Modifier.padding(10.dp).fillMaxSize()) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(lines.size.toString() + " 行", fontSize = 11.sp, color = dimColor())
                Spacer(Modifier.weight(1f))
                TextButton(onClick = { LogBus.clear() }) { Text("清空", fontSize = 12.sp) }
                TextButton(onClick = { copy(ctx, LogBus.text()) }) { Text("复制", fontSize = 12.sp) }
                Checkbox(follow, { follow = it })
                Text("跟随", fontSize = 11.sp)
            }
            Column(Modifier.weight(1f).verticalScroll(scroll)) {
                Text(
                    lines.joinToString("\n"),
                    fontFamily = FontFamily.Monospace, fontSize = 10.sp, color = dimColor(),
                )
            }
        }
    }

    private fun copy(ctx: Context, text: String) {
        runCatching {
            ctx.getSystemService(ClipboardManager::class.java)
                .setPrimaryClip(ClipData.newPlainText("rp", text))
        }
    }
}
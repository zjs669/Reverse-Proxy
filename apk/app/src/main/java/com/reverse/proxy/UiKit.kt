package com.reverse.proxy

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.*
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp

/** 运行状态圆点。 */
@Composable
fun Dot(running: Boolean) {
    Box(
        Modifier.size(8.dp)
            .background(if (running) goodColor() else dimColor(), CircleShape)
    )
}

/** 标签 + 值 + 复制。 */
@Composable
fun CopyRow(label: String, value: String, onCopy: () -> Unit) {
    Row(verticalAlignment = Alignment.CenterVertically) {
        Text(label, fontSize = 11.sp, color = dimColor(), modifier = Modifier.width(66.dp))
        Text(
            value, fontSize = 12.sp, fontFamily = FontFamily.Monospace,
            modifier = Modifier.weight(1f), maxLines = 1,
        )
        TextButton(onClick = onCopy, contentPadding = PaddingValues(horizontal = 8.dp)) {
            Text("复制", fontSize = 11.sp)
        }
    }
}

/** 等宽代码块。 */
@Composable
fun Code(text: String) {
    Box(
        Modifier.fillMaxWidth()
            .background(cardColor(), RoundedCornerShape(6.dp))
            .padding(horizontal = 10.dp, vertical = 8.dp),
    ) { Text(text, fontFamily = FontFamily.Monospace, fontSize = 11.sp) }
}

/** 有效期小标签。 */
@Composable
fun Expiry(acc: Account) {
    val warn = Color(0xFFE8A33D)
    val (t, c) = when {
        acc.expiresAt <= 0 -> "无期限" to dimColor()
        acc.expired -> "已过期" to badColor()
        acc.expiring -> acc.remainText to warn
        else -> acc.remainText to goodColor()
    }
    Box(
        Modifier.background(c.copy(alpha = 0.16f), RoundedCornerShape(4.dp))
            .padding(horizontal = 6.dp, vertical = 2.dp),
    ) { Text(t, fontSize = 10.sp, color = c) }
}
package com.reverse.proxy

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Typography
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.sp

// ── 主题模式 ────────────────────────────────────────────────────────────────
const val THEME_SYSTEM = 0
const val THEME_LIGHT = 1
const val THEME_DARK = 2

// ── 深色 ────────────────────────────────────────────────────────────────────
private val DarkBg = Color(0xFF0F1113)
private val DarkCard = Color(0xFF1A1D21)
private val DarkCardHigh = Color(0xFF24282D)
private val DarkFg = Color(0xFFE8EAED)
private val DarkDim = Color(0xFF8E9297)

// ── 浅色 ────────────────────────────────────────────────────────────────────
private val LightBg = Color(0xFFF7F8FA)
private val LightCard = Color(0xFFFFFFFF)
private val LightCardHigh = Color(0xFFEEF1F5)
private val LightFg = Color(0xFF1A1D21)
private val LightDim = Color(0xFF6B7076)

// ── 共用强调色 ──────────────────────────────────────────────────────────────
private val Accent = Color(0xFF0288D1)
private val AccentDark = Color(0xFF5AC8FA)
private val Good = Color(0xFF2E7D32)
private val GoodDark = Color(0xFF66BB6A)
private val Bad = Color(0xFFC62828)
private val BadDark = Color(0xFFEF5350)

private val DarkScheme = darkColorScheme(
    primary = AccentDark,
    onPrimary = Color(0xFF00212E),
    secondary = GoodDark,
    background = DarkBg,
    onBackground = DarkFg,
    surface = DarkCard,
    onSurface = DarkFg,
    surfaceVariant = DarkCardHigh,
    onSurfaceVariant = DarkDim,
    error = BadDark,
)

private val LightScheme = lightColorScheme(
    primary = Accent,
    onPrimary = Color.White,
    secondary = Good,
    background = LightBg,
    onBackground = LightFg,
    surface = LightCard,
    onSurface = LightFg,
    surfaceVariant = LightCardHigh,
    onSurfaceVariant = LightDim,
    error = Bad,
)

/** 当前是否深色（由 AppTheme 写入，供 UiKit 取色）。 */
var isDarkTheme: Boolean = true
    private set

val Mono = TextStyle(fontFamily = FontFamily.Monospace, fontSize = 12.sp)
val MonoSm = TextStyle(fontFamily = FontFamily.Monospace, fontSize = 11.sp)

/** 卡片底色：跟随明暗。 */
@Composable
fun cardColor(): Color = if (isDarkTheme) DarkCardHigh else LightCardHigh

@Composable
fun dimColor(): Color = MaterialTheme.colorScheme.onSurfaceVariant

@Composable
fun goodColor(): Color = if (isDarkTheme) GoodDark else Good

@Composable
fun badColor(): Color = MaterialTheme.colorScheme.error

@Composable
fun AppTheme(mode: Int, content: @Composable () -> Unit) {
    val dark = when (mode) {
        THEME_LIGHT -> false
        THEME_DARK -> true
        else -> isSystemInDarkTheme()
    }
    isDarkTheme = dark
    MaterialTheme(
        colorScheme = if (dark) DarkScheme else LightScheme,
        typography = Typography(),
        content = content,
    )
}
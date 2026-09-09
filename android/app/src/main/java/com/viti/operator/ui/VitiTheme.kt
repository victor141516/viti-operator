package com.viti.operator.ui

import android.os.Build
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.dynamicDarkColorScheme
import androidx.compose.material3.dynamicLightColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext

private val LightColors = lightColorScheme(
    primary = Color(0xFF4058C9),
    onPrimary = Color.White,
    primaryContainer = Color(0xFFDDE1FF),
    onPrimaryContainer = Color(0xFF0A1B61),
    secondary = Color(0xFF595D72),
    tertiary = Color(0xFF76546D),
    surface = Color(0xFFFAF8FF),
    surfaceContainer = Color(0xFFEEECF4),
)

private val DarkColors = darkColorScheme(
    primary = Color(0xFFBAC3FF),
    onPrimary = Color(0xFF082678),
    primaryContainer = Color(0xFF273F9F),
    onPrimaryContainer = Color(0xFFDDE1FF),
)

@Composable
fun VitiTheme(content: @Composable () -> Unit) {
    val dark = isSystemInDarkTheme()
    val context = LocalContext.current
    val colors = when {
        Build.VERSION.SDK_INT >= Build.VERSION_CODES.S && dark -> dynamicDarkColorScheme(context)
        Build.VERSION.SDK_INT >= Build.VERSION_CODES.S -> dynamicLightColorScheme(context)
        dark -> DarkColors
        else -> LightColors
    }
    MaterialTheme(colorScheme = colors, typography = MaterialTheme.typography, content = content)
}

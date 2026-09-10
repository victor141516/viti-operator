package com.viti.operator.data

import android.content.Context
import android.os.Build
import android.provider.Settings
import android.telephony.TelephonyManager

fun Context.vitiClientIdentity(): ClientIdentity {
    val stableId = Settings.Secure.getString(contentResolver, Settings.Secure.ANDROID_ID)
        ?.takeIf(String::isNotBlank)
        ?: Build.FINGERPRINT.hashCode().toUInt().toString(16)
    val configuredName = runCatching {
        Settings.Global.getString(contentResolver, "device_name")?.trim()
    }.getOrNull().orEmpty()
    val manufacturer = Build.MANUFACTURER.trim().replaceFirstChar { it.uppercase() }
    val brand = Build.BRAND.trim().replaceFirstChar { it.uppercase() }
    val model = Build.MODEL.trim()
    val commercialBrand = brand.takeUnless {
        it.isBlank() || it.equals("Android", ignoreCase = true) || it.equals("Generic", ignoreCase = true)
    } ?: manufacturer
    val hardwareName = when {
        model.isBlank() -> commercialBrand
        commercialBrand.isBlank() || model.startsWith(commercialBrand, ignoreCase = true) -> model
        else -> "$commercialBrand $model"
    }
    val name = configuredName
        .takeIf { it.isNotBlank() }
        ?.takeUnless { it.equals("Android", ignoreCase = true) }
        .orEmpty()
        .ifBlank { hardwareName }
        .take(80)
        .ifBlank { "Android phone" }
    val telephony = getSystemService(TelephonyManager::class.java)
    val hasSIM = telephony?.simState?.let {
        it != TelephonyManager.SIM_STATE_ABSENT && it != TelephonyManager.SIM_STATE_UNKNOWN
    } == true
    val cellular = CellularTelemetryMonitor.get(this).snapshot()
    return ClientIdentity("android-$stableId", name, hasSIM, cellular)
}

internal fun ClientIdentity.withCurrentCellularObservation(): ClientIdentity =
    copy(cellular = CellularTelemetryMonitor.currentSnapshot() ?: cellular)

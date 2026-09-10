package com.viti.operator.data

import android.content.Context
import android.telephony.SubscriptionManager
import android.telephony.TelephonyManager
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.json.JSONObject
import java.io.ByteArrayOutputStream
import java.net.HttpURLConnection
import java.net.URL
import java.time.Duration
import java.time.Instant
import java.util.Locale
import java.util.TimeZone

data class SessionCountryDetection(
    val sessionGeneration: String,
    val country: String = "",
    val source: String = "",
    val reason: String = "",
)

internal data class SessionCountrySignals(
    val servingCountry: String = "",
    val ipCountry: String = "",
    val timezoneCountry: String = "",
    val localeCountry: String = "",
)

internal fun canonicalCountry(value: String): String = value.trim().uppercase(Locale.ROOT)
    .takeIf { it.length == 2 && it.all(Char::isLetter) }
    .orEmpty()

internal fun resolveSessionCountry(sessionGeneration: String, signals: SessionCountrySignals): SessionCountryDetection {
    val serving = canonicalCountry(signals.servingCountry)
    if (serving.isNotEmpty()) return SessionCountryDetection(sessionGeneration, serving, "serving-network")
    val ip = canonicalCountry(signals.ipCountry)
    val timezone = canonicalCountry(signals.timezoneCountry)
    val locale = canonicalCountry(signals.localeCountry)
    if (ip.isEmpty()) return SessionCountryDetection(sessionGeneration, reason = "ip-unavailable")
    if (timezone.isNotEmpty()) {
        if (timezone != ip) return SessionCountryDetection(sessionGeneration, reason = "ip-timezone-conflict")
        return SessionCountryDetection(sessionGeneration, ip, "ip+timezone")
    }
    if (locale == ip) return SessionCountryDetection(sessionGeneration, ip, "ip+locale")
    return SessionCountryDetection(sessionGeneration, reason = if (locale.isEmpty()) "insufficient-evidence" else "ip-locale-conflict")
}

internal fun isCanonicalE164(value: String): Boolean = value.trim().matches(Regex("^\\+[1-9][0-9]{7,14}$"))

class SessionCountryDetector(
    context: Context,
    private val ipCountryLookup: suspend () -> String = ::lookupIPCountry,
) {
    private val appContext = context.applicationContext

    suspend fun detect(sessionGeneration: String): SessionCountryDetection = withContext(Dispatchers.IO) {
        val serving = currentServingCountry()
        if (serving.isNotEmpty()) {
            return@withContext resolveSessionCountry(sessionGeneration, SessionCountrySignals(servingCountry = serving))
        }
        val timezone = runCatching {
            android.icu.util.TimeZone.getRegion(TimeZone.getDefault().id)
        }.getOrDefault("").takeUnless { it == "001" }.orEmpty()
        resolveSessionCountry(
            sessionGeneration,
            SessionCountrySignals(
                ipCountry = runCatching { ipCountryLookup() }.getOrDefault(""),
                timezoneCountry = timezone,
                localeCountry = Locale.getDefault().country,
            ),
        )
    }

    private fun currentServingCountry(): String {
        val observation = CellularTelemetryMonitor.get(appContext).snapshot() ?: return ""
        if (observation.service.registration != "in_service") return ""
        val observedAt = runCatching { Instant.parse(observation.service.measuredAt) }.getOrNull() ?: return ""
        if (Duration.between(observedAt, Instant.now()).abs() > Duration.ofMinutes(2)) return ""
        val subscriptionId = simAuthSubscriptionId()
        if (subscriptionId == SubscriptionManager.INVALID_SUBSCRIPTION_ID) return ""
        val manager = appContext.getSystemService(TelephonyManager::class.java) ?: return ""
        return runCatching { manager.createForSubscriptionId(subscriptionId).networkCountryIso }.getOrDefault("")
    }
}

private suspend fun lookupIPCountry(): String = withContext(Dispatchers.IO) {
    val connection = URL("https://api.country.is/").openConnection() as HttpURLConnection
    try {
        connection.requestMethod = "GET"
        connection.connectTimeout = 3_500
        connection.readTimeout = 3_500
        connection.instanceFollowRedirects = false
        connection.useCaches = false
        connection.setRequestProperty("Accept", "application/json")
        connection.setRequestProperty("Cache-Control", "no-store")
        if (connection.responseCode !in 200..299) return@withContext ""
        val declared = connection.contentLengthLong
        if (declared > COUNTRY_RESPONSE_LIMIT) return@withContext ""
        val bytes = connection.inputStream.use { input ->
            val output = ByteArrayOutputStream(COUNTRY_RESPONSE_LIMIT + 1)
            val buffer = ByteArray(256)
            while (output.size() <= COUNTRY_RESPONSE_LIMIT) {
                val remaining = COUNTRY_RESPONSE_LIMIT + 1 - output.size()
                val count = input.read(buffer, 0, minOf(buffer.size, remaining))
                if (count <= 0) break
                output.write(buffer, 0, count)
            }
            output.toByteArray()
        }
        if (bytes.size > COUNTRY_RESPONSE_LIMIT) return@withContext ""
        canonicalCountry(JSONObject(bytes.toString(Charsets.UTF_8)).optString("country"))
    } catch (_: Exception) {
        ""
    } finally {
        connection.disconnect()
    }
}

private const val COUNTRY_RESPONSE_LIMIT = 1024

package com.viti.operator.voice

import com.viti.operator.data.VoiceCallQuality
import com.viti.operator.data.VoiceMediaRoute
import org.json.JSONObject

data class VoiceEngineEvent(
    val type: String,
    val peer: String = "",
    val cause: String = "",
    val traceId: String = "",
    val participantId: String = "",
    val legToken: String = "",
    val connectionId: String = "",
    val quality: VoiceCallQuality? = null,
    val registrationExpiresAtMs: Long = 0L,
)

class VoiceCommandException(message: String) : IllegalStateException(message)

internal object VoiceQualityJson {
    fun parse(body: JSONObject?): VoiceCallQuality? = body?.let {
        val route = it.optJSONObject("route")
        parseValues(mapOf(
            "sequence" to it.optLong("sequence", 0), "status" to it.optString("status"),
            "sampledAtMs" to it.optLong("sampledAtMs", 0), "inboundLossPercent" to it.optNullableDouble("inboundLossPercent"),
            "outboundLossPercent" to it.optNullableDouble("outboundLossPercent"), "inboundJitterMs" to it.optNullableDouble("inboundJitterMs"),
            "roundTripTimeMs" to it.optNullableDouble("roundTripTimeMs"), "roundTripScope" to it.optString("roundTripScope"),
            "inboundWindowMs" to it.optNullableDouble("inboundWindowMs"), "outboundWindowMs" to it.optNullableDouble("outboundWindowMs")),
            route?.let { value -> mapOf("mode" to value.optString("mode"), "protocol" to value.optString("protocol", "unknown"),
                "localCandidateType" to value.optString("localCandidateType", "unknown"), "remoteCandidateType" to value.optString("remoteCandidateType", "unknown")) })
    }
    private fun JSONObject.optNullableDouble(name: String): Double? = if (has(name) && !isNull(name)) optDouble(name).takeIf(Double::isFinite) else null
    fun parseValues(values: Map<String, Any?>, routeValues: Map<String, Any?>? = null): VoiceCallQuality? {
        val sequence = (values["sequence"] as? Number)?.toLong() ?: 0
        val status = values["status"] as? String ?: ""
        if (sequence <= 0 || status !in setOf("connecting", "collecting", "available", "reconnecting", "unavailable", "unsupported")) return null
        val route = routeValues?.let {
            val mode = it["mode"] as? String ?: ""
            if (mode !in setOf("direct", "turn-relay")) null else VoiceMediaRoute(mode, it["protocol"] as? String ?: "unknown",
                it["localCandidateType"] as? String ?: "unknown", it["remoteCandidateType"] as? String ?: "unknown")
        }
        fun number(name: String) = (values[name] as? Number)?.toDouble()?.takeIf(Double::isFinite)
        return VoiceCallQuality(sequence, status, (values["sampledAtMs"] as? Number)?.toLong() ?: 0,
            number("inboundLossPercent"), number("outboundLossPercent"), number("inboundJitterMs"), number("roundTripTimeMs"),
            values["roundTripScope"] as? String ?: "", number("inboundWindowMs"), number("outboundWindowMs"), route)
    }
}

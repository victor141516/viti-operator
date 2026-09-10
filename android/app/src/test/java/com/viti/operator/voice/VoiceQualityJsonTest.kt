package com.viti.operator.voice

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class VoiceQualityJsonTest {
    @Test
    fun parsesOnlyWhitelistedTypedQualityEvidence() {
        val quality = VoiceQualityJson.parseValues(
            mapOf(
                "sequence" to 7, "status" to "available", "sampledAtMs" to 1234,
                "inboundLossPercent" to 1.25, "outboundLossPercent" to 2.5,
                "inboundJitterMs" to 8, "roundTripTimeMs" to 42,
                "roundTripScope" to "media-peer", "inboundWindowMs" to 1000, "outboundWindowMs" to 900,
            ),
            mapOf(
                "mode" to "turn-relay", "protocol" to "udp", "localCandidateType" to "relay",
                "remoteCandidateType" to "srflx", "address" to "must-not-cross-the-bridge",
            ),
        )

        assertEquals(7L, quality?.sequence)
        assertEquals(1.25, quality?.inboundLossPercent ?: -1.0, 0.0)
        assertEquals("turn-relay", quality?.route?.mode)
        assertEquals("udp", quality?.route?.protocol)
        assertEquals(1000.0, quality?.inboundWindowMs ?: -1.0, 0.0)
        assertEquals(900.0, quality?.outboundWindowMs ?: -1.0, 0.0)
    }

    @Test
    fun rejectsMalformedSequenceStatusAndRouteWithoutInventingValues() {
        assertNull(VoiceQualityJson.parseValues(emptyMap()))
        assertNull(VoiceQualityJson.parseValues(mapOf("sequence" to 1, "status" to "great")))
        val partial = VoiceQualityJson.parseValues(
            mapOf("sequence" to 1, "status" to "collecting", "inboundLossPercent" to null),
            mapOf("mode" to "configured-turn"),
        )
        assertNull(partial?.inboundLossPercent)
        assertNull(partial?.route)
    }
}

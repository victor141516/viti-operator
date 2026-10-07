package com.viti.operator.voice

import org.junit.Assert.*
import org.junit.Test

class NativeVoiceQualityTest {
    private fun inbound(id: String = "audio", at: Double, received: Long, lost: Long) = NativeRtcStat(id, "inbound-rtp", at,
        mapOf("kind" to "audio", "packetsReceived" to received, "packetsLost" to lost, "jitter" to 0.005))
    @Test fun intervalLossDoesNotReuseLifetimeOrIdleCounters() {
        val quality = NativeVoiceQuality()
        assertNull(quality.sample(listOf(inbound(at = 1000.0, received = 900, lost = 100))).inboundLossPercent)
        val active = quality.sample(listOf(inbound(at = 3000.0, received = 990, lost = 110)))
        assertEquals(10.0, active.inboundLossPercent!!, 0.001)
        assertEquals(2000.0, active.inboundWindowMs!!, 0.001)
        assertEquals(5.0, active.inboundJitterMs!!, 0.001)
        assertNull(quality.sample(listOf(inbound(at = 5000.0, received = 990, lost = 110))).inboundLossPercent)
        assertNull(quality.sample(listOf(inbound(at = 6000.0, received = 1, lost = 0))).inboundLossPercent)
        assertNull(quality.sample(listOf(inbound(id = "replaced", at = 7000.0, received = 200, lost = 10))).inboundLossPercent)
        assertEquals("unavailable", quality.unavailable().status)
        assertNull(quality.sample(listOf(inbound(id = "replaced", at = 8000.0, received = 300, lost = 10))).inboundLossPercent)
    }
    @Test fun selectedRelayRouteAndRttComeFromTheActualTransportPair() {
        val sample = NativeVoiceQuality().sample(listOf(
            NativeRtcStat("transport", "transport", 1000.0, mapOf("selectedCandidatePairId" to "pair")),
            NativeRtcStat("pair", "candidate-pair", 1000.0, mapOf("localCandidateId" to "local", "remoteCandidateId" to "remote", "currentRoundTripTime" to 0.1)),
            NativeRtcStat("local", "local-candidate", 1000.0, mapOf("candidateType" to "relay", "protocol" to "udp")),
            NativeRtcStat("remote", "remote-candidate", 1000.0, mapOf("candidateType" to "host"))))
        assertEquals("turn-relay", sample.route!!.mode)
        assertEquals(100.0, sample.roundTripTimeMs!!, 0.001)
        assertEquals("ice-path", sample.roundTripScope)
        assertNull(sample.outboundLossPercent)
    }
}

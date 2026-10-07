package com.viti.operator.voice

import org.junit.Assert.assertEquals
import org.junit.Test

class VoiceSignalingProbeTest {
    @Test fun nativeTicksAndJavaScriptAcknowledgementsAreIndependent() {
        var now = 100L
        val probe = VoiceSignalingProbe { now }
        assertEquals(-1L, probe.jsStatusAgeMs())
        probe.tick()
        probe.observe("signaling-status", "registered")
        now += 30_000
        probe.tick()
        assertEquals(2L, probe.tickCount)
        assertEquals(30_000L, probe.jsStatusAgeMs())
        assertEquals(30_000L, probe.uptimeMs())
        probe.observe("registered")
        assertEquals(0L, probe.eventAgeMs())
        assertEquals(30_000L, probe.jsStatusAgeMs())
    }

    @Test fun unrelatedCallEventsAndRawCausesCannotEnterTheProbe() {
        val probe = VoiceSignalingProbe { 100L }
        probe.observe("incoming", "private number")
        assertEquals("none", probe.event)
        probe.observe("signaling-status", "private error")
        assertEquals("none", probe.jsStatus)
        probe.observe("disconnected", "private error")
        assertEquals("disconnected", probe.event)
    }

    @Test fun leaseRemainingNeverReportsNegativeValues() {
        var now = 10_000L
        val lease = VoiceRegistrationLease { now }
        assertEquals(0L, lease.remainingMs())
        lease.confirmed(20_000)
        assertEquals(10_000L, lease.remainingMs())
        now = 30_000
        assertEquals(0L, lease.remainingMs())
    }
}

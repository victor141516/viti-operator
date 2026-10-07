package com.viti.operator.voice

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class VoiceRegistrationLeaseTest {
    @Test
    fun delayedBackgroundWebViewChecksDoNotInvalidateTheServerLeaseAfterTwoMinutes() {
        var now = 1_000L
        val lease = VoiceRegistrationLease { now }
        lease.confirmed(now + 600_000)
        repeat(5) {
            now += 30_000
            assertTrue(lease.isValid())
            assertFalse(lease.hasExpired())
        }
        now = 601_000
        assertFalse(lease.isValid())
        assertTrue(lease.hasExpired())
    }

    @Test
    fun anActualDisconnectInvalidatesTheLeaseImmediatelyAndReconnectionRearmsIt() {
        val lease = VoiceRegistrationLease { 1_000 }
        lease.confirmed(601_000)
        lease.lost()
        assertFalse(lease.isValid())
        lease.confirmed(301_000)
        assertTrue(lease.isValid())
    }

    @Test
    fun absentAndAlreadyExpiredEvidenceIsNeverTreatedAsConnected() {
        val lease = VoiceRegistrationLease { 10_000 }
        assertFalse(lease.isValid())
        lease.confirmed(9_999)
        assertFalse(lease.isValid())
    }
}

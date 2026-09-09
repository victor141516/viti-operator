package com.viti.operator.voice

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class VoicePlatformPolicyTest {
    @Test
    fun coreTelecomRequiresBothApi26AndTheTelecomFeature() {
        assertFalse(supportsCoreTelecom(25, hasTelecomFeature = true))
        assertFalse(supportsCoreTelecom(26, hasTelecomFeature = false))
        assertTrue(supportsCoreTelecom(26, hasTelecomFeature = true))
    }

    @Test
    fun callActionIdentityChangesWithTheImmutableLegToken() {
        val old = callActionData(VoiceCallService.ACTION_HANGUP, "old-token")
        val replacement = callActionData(VoiceCallService.ACTION_HANGUP, "replacement-token")

        assertNotEquals(old, replacement)
        assertTrue(old.endsWith(":old-token"))
        assertTrue(replacement.endsWith(":replacement-token"))
    }

    @Test
    fun legacyRouteDoesNotClaimBluetoothBeforeScoConnects() {
        val wired = VoiceAudioEndpoint("wired", "Wired", VoiceAudioEndpointType.WIRED_HEADSET)
        val bluetooth = VoiceAudioEndpoint("bt", "Bluetooth", VoiceAudioEndpointType.BLUETOOTH)
        val earpiece = VoiceAudioEndpoint("earpiece", "Earpiece", VoiceAudioEndpointType.EARPIECE)
        val available = listOf(earpiece, wired, bluetooth)

        assertEquals(wired, selectLegacyCurrentEndpoint(available, speakerphoneOn = false, bluetoothScoConnected = false))
        assertEquals(bluetooth, selectLegacyCurrentEndpoint(available, speakerphoneOn = false, bluetoothScoConnected = true))
    }

    @Test
    fun legacyRouteDoesNotClaimEarpieceWhileWiredAudioStillWins() {
        val earpiece = VoiceAudioEndpoint("earpiece", "Earpiece", VoiceAudioEndpointType.EARPIECE)
        val wired = VoiceAudioEndpoint("wired", "Wired", VoiceAudioEndpointType.WIRED_HEADSET)

        assertEquals(
            wired,
            selectLegacyCurrentEndpoint(listOf(earpiece, wired), speakerphoneOn = false, bluetoothScoConnected = false),
        )
    }
}

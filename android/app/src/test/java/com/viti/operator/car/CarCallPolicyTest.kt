package com.viti.operator.car

import com.viti.operator.data.VoiceUiState
import com.viti.operator.voice.VoiceCallSnapshot
import org.junit.Assert.assertEquals
import org.junit.Test

class CarCallPolicyTest {
    @Test
    fun successfulBindCannotHideAFailedDurableServiceStart() {
        val state = readyState().copy(serviceDurable = false)

        assertEquals(
            "Call service could not start durably. Open Viti on your phone when safe.",
            carCallBlockReason(state, servicePresent = true, microphoneGranted = true),
        )
    }

    @Test
    fun bluetoothPermissionIsNotRequiredForUsbSpeakerOrEarpieceCalls() {
        assertEquals(
            "",
            carCallBlockReason(readyState(), servicePresent = true, microphoneGranted = true),
        )
    }

    @Test
    fun microphonePermissionStillBlocksMediaCapture() {
        assertEquals(
            "Complete microphone permission in Viti on your phone when safe.",
            carCallBlockReason(readyState(), servicePresent = true, microphoneGranted = false),
        )
    }

    private fun readyState() = CarCallState(
        coreTelecomAvailable = true,
        serviceConnected = true,
        serviceDurable = true,
        loadingDirectory = false,
        call = VoiceCallSnapshot(voice = VoiceUiState(state = "registered")),
    )
}

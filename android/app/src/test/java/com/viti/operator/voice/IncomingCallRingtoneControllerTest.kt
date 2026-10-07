package com.viti.operator.voice

import org.junit.Assert.assertEquals
import org.junit.Test
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest

@OptIn(ExperimentalCoroutinesApi::class)
class IncomingCallRingtoneControllerTest {
    private val ringing = VoiceCallSnapshot(legToken = "leg-1", direction = VoiceCallDirection.INCOMING, phase = VoiceCallPhase.RINGING)

    @Test
    fun serviceStartsRingingWithoutAnyActivityAndRepeatedUpdatesDoNotRestartSound() {
        val actions = mutableListOf<String>()
        val controller = IncomingCallRingtoneController({ actions += "start" }, { actions += "stop" })
        controller.update(ringing)
        controller.update(ringing.copy(platformReady = true))
        controller.update(ringing)
        assertEquals(listOf("start"), actions)
    }

    @Test
    fun answerRejectRemoteEndAndOutgoingCallsStopRinging() {
        for (next in listOf(
            ringing.copy(phase = VoiceCallPhase.ACTIVE), ringing.copy(phase = VoiceCallPhase.ENDING),
            VoiceCallSnapshot(), ringing.copy(direction = VoiceCallDirection.OUTGOING),
        )) {
            val actions = mutableListOf<String>()
            val controller = IncomingCallRingtoneController({ actions += "start" }, { actions += "stop" })
            controller.update(ringing)
            controller.update(next)
            assertEquals(listOf("start", "stop"), actions)
        }
    }

    @Test
    fun aNewLegReplacesTheOldRingtoneAndServiceShutdownStopsSound() {
        val actions = mutableListOf<String>()
        val controller = IncomingCallRingtoneController({ actions += "start" }, { actions += "stop" })
        controller.update(ringing)
        controller.update(ringing.copy(legToken = "leg-2"))
        controller.close()
        assertEquals(listOf("start", "stop", "start", "stop"), actions)
    }

    @Test
    fun answerDuringNotificationHandoffCannotStartAStaleRingtone() = runTest {
        val actions = mutableListOf<String>()
        val controller = IncomingCallRingtoneController({ actions += "start" }, { actions += "stop" }, this, 600)
        controller.update(ringing)
        runCurrent()
        advanceTimeBy(300)
        controller.update(ringing.copy(phase = VoiceCallPhase.ACTIVE))
        advanceTimeBy(600)
        runCurrent()
        assertEquals(listOf("stop"), actions)
        controller.close()
    }

    @Test
    fun notificationHandoffRingsOnlyTheCurrentLegWithoutAnActivity() = runTest {
        val actions = mutableListOf<String>()
        val controller = IncomingCallRingtoneController({ actions += "start" }, { actions += "stop" }, this, 600)
        controller.update(ringing)
        runCurrent()
        advanceTimeBy(300)
        controller.update(ringing.copy(legToken = "leg-2"))
        runCurrent()
        advanceTimeBy(600)
        runCurrent()
        assertEquals(listOf("stop", "start"), actions)
        controller.close()
    }
}

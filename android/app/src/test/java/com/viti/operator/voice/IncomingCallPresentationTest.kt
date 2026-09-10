package com.viti.operator.voice

import android.app.NotificationManager
import org.junit.Assert.assertFalse
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class IncomingCallPresentationTest {
    @Test
    fun androidBefore14DoesNotExposeARevocableFullScreenGate() {
        assertTrue(fullScreenIntentAllowed(33, platformAllowed = false))
    }

    @Test
    fun android14AndLaterUsesTheObservedPlatformDecision() {
        assertFalse(fullScreenIntentAllowed(34, platformAllowed = false))
        assertTrue(fullScreenIntentAllowed(34, platformAllowed = true))
        assertTrue(fullScreenIntentAllowed(36, platformAllowed = true))
    }

    @Test
    fun incomingSurfaceRequiresTheExactStillRingingLeg() {
        val ringing = VoiceCallSnapshot(
            legToken = "leg-7",
            direction = VoiceCallDirection.INCOMING,
            phase = VoiceCallPhase.RINGING,
        )

        assertTrue(shouldShowIncomingCall(ringing, "leg-7"))
        assertFalse(shouldShowIncomingCall(ringing, "older-leg"))
        assertFalse(shouldShowIncomingCall(ringing.copy(phase = VoiceCallPhase.ACTIVE), "leg-7"))
        assertFalse(shouldShowIncomingCall(ringing.copy(direction = VoiceCallDirection.OUTGOING), "leg-7"))
        assertFalse(shouldShowIncomingCall(ringing.copy(legToken = ""), ""))
    }

    @Test
    fun disabledOrReducedCallChannelIsReportedWithoutClaimingProminence() {
        val disabled = IncomingCallPresentationStatus(callChannelImportance = NotificationManager.IMPORTANCE_NONE)
        val missing = IncomingCallPresentationStatus(callChannelImportance = NotificationManager.IMPORTANCE_UNSPECIFIED)
        val reduced = IncomingCallPresentationStatus(callChannelImportance = NotificationManager.IMPORTANCE_DEFAULT)
        val high = IncomingCallPresentationStatus(callChannelImportance = NotificationManager.IMPORTANCE_HIGH)

        assertFalse(disabled.callChannelEnabled)
        assertFalse(disabled.prominentCallChannel)
        assertFalse(missing.callChannelEnabled)
        assertFalse(missing.prominentCallChannel)
        assertTrue(reduced.callChannelEnabled)
        assertFalse(reduced.prominentCallChannel)
        assertTrue(high.callChannelEnabled)
        assertTrue(high.prominentCallChannel)
    }

    @Test
    fun recreationPreservesPendingPermissionOnlyForTheSameLeg() {
        val beforeRecreation = IncomingCallActivityMemory(
            expectedToken = "leg-7",
            fallbackPeer = "Ada",
            permissionRequestToken = "leg-7",
            notice = "Waiting for microphone permission",
        )

        val restored = memoryForIncomingIntent(
            current = beforeRecreation,
            token = "leg-7",
            fallbackPeer = "",
            requestAnswer = false,
        )
        assertEquals("leg-7", restored.permissionRequestToken)
        assertEquals("Ada", restored.fallbackPeer)

        val reusedForAnotherCall = memoryForIncomingIntent(
            current = beforeRecreation,
            token = "leg-8",
            fallbackPeer = "Grace",
            requestAnswer = false,
        )
        assertNull(reusedForAnotherCall.permissionRequestToken)
        assertNull(reusedForAnotherCall.autoAnswerToken)
        assertFalse(reusedForAnotherCall.actionSubmitted)
        assertEquals("", reusedForAnotherCall.notice)
    }

    @Test
    fun repeatedAnswerIntentDoesNotReplaceAnActivePermissionRequest() {
        val waiting = IncomingCallActivityMemory(
            expectedToken = "leg-7",
            permissionRequestToken = "leg-7",
        )

        val repeated = memoryForIncomingIntent(
            current = waiting,
            token = "leg-7",
            fallbackPeer = "Ada",
            requestAnswer = true,
        )

        assertEquals("leg-7", repeated.permissionRequestToken)
        assertNull(repeated.autoAnswerToken)
    }
}

package com.viti.operator.voice

import android.app.NotificationManager
import android.os.Build

/**
 * Observable Android capabilities that affect how an incoming call can be
 * presented. These are evidence, not a promise that a particular OEM will
 * choose a full-screen surface for the next notification.
 */
data class IncomingCallPresentationStatus(
    val notificationsEnabled: Boolean = false,
    val callChannelImportance: Int? = null,
    val fullScreenIntentAllowed: Boolean = false,
    val fullScreenSettingAvailable: Boolean = false,
    val backgroundCallsUnrestricted: Boolean = false,
    val callSoundEnabled: Boolean = false,
) {
    val callChannelEnabled: Boolean
        get() = callChannelImportance == null || callChannelImportance > NotificationManager.IMPORTANCE_NONE

    val prominentCallChannel: Boolean
        get() = callChannelImportance == null || callChannelImportance >= NotificationManager.IMPORTANCE_HIGH
}

internal data class IncomingCallActivityMemory(
    val expectedToken: String = "",
    val fallbackPeer: String = "",
    val permissionRequestToken: String? = null,
    val autoAnswerToken: String? = null,
    val actionSubmitted: Boolean = false,
    val notice: String = "",
)

/**
 * Reuses volatile presentation state only for the exact same service leg.
 * A new token must never inherit an answer intent or submitted action from a
 * previous call, even when Android reuses the single-task Activity.
 */
internal fun memoryForIncomingIntent(
    current: IncomingCallActivityMemory,
    token: String,
    fallbackPeer: String,
    requestAnswer: Boolean,
): IncomingCallActivityMemory {
    val base = if (current.expectedToken == token) {
        current.copy(fallbackPeer = fallbackPeer.ifBlank { current.fallbackPeer })
    } else {
        IncomingCallActivityMemory(expectedToken = token, fallbackPeer = fallbackPeer)
    }
    return if (
        requestAnswer &&
        token.isNotBlank() &&
        base.permissionRequestToken != token &&
        !base.actionSubmitted
    ) {
        base.copy(autoAnswerToken = token)
    } else {
        base
    }
}

internal fun fullScreenIntentAllowed(sdkInt: Int, platformAllowed: Boolean): Boolean =
    sdkInt < Build.VERSION_CODES.UPSIDE_DOWN_CAKE || platformAllowed

internal fun shouldShowIncomingCall(
    snapshot: VoiceCallSnapshot,
    expectedToken: String,
): Boolean =
    expectedToken.isNotBlank() &&
        snapshot.legToken == expectedToken &&
        snapshot.direction == VoiceCallDirection.INCOMING &&
        snapshot.phase == VoiceCallPhase.RINGING

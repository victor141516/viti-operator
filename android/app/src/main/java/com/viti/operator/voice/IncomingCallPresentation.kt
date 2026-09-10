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
) {
    val callChannelEnabled: Boolean
        get() = callChannelImportance == null || callChannelImportance > NotificationManager.IMPORTANCE_NONE

    val prominentCallChannel: Boolean
        get() = callChannelImportance == null || callChannelImportance >= NotificationManager.IMPORTANCE_HIGH
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

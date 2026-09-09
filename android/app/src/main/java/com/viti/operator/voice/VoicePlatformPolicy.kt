package com.viti.operator.voice

/** Pure platform-policy helpers kept independent of Android framework objects. */
internal fun supportsCoreTelecom(sdkInt: Int, hasTelecomFeature: Boolean): Boolean =
    sdkInt >= 26 && hasTelecomFeature

/**
 * PendingIntent identity includes this opaque URI, so an action retained by a
 * wearable or notification can never be silently retargeted to a newer leg.
 */
internal fun callActionData(action: String, legToken: String): String =
    "viti-call:${action.substringAfterLast('.')}:${legToken.length}:$legToken"

/** A failed incoming reject must remain a reject when retried from CallStyle. */
internal fun shouldRejectFromNotification(snapshot: VoiceCallSnapshot): Boolean =
    snapshot.phase == VoiceCallPhase.ENDING && snapshot.endingRejected

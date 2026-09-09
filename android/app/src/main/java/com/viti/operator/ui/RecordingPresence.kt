package com.viti.operator.ui

import com.viti.operator.data.RecordingTrack

enum class RecordingPresencePhase { BEFORE, ACTIVE, AFTER }

data class RecordingPresence(
    val phase: RecordingPresencePhase,
    val boundaryMs: Long,
) {
    val active: Boolean get() = phase == RecordingPresencePhase.ACTIVE
}

fun recordingTrackPresence(track: RecordingTrack, positionMs: Long): RecordingPresence {
    val offsetMs = track.offsetMs.coerceAtLeast(0)
    val currentMs = positionMs.coerceAtLeast(0)
    val endMs = if (track.durationMs > 0) offsetMs + track.durationMs else null
    return when {
        currentMs < offsetMs -> RecordingPresence(RecordingPresencePhase.BEFORE, offsetMs)
        endMs != null && currentMs >= endMs -> RecordingPresence(RecordingPresencePhase.AFTER, endMs)
        else -> RecordingPresence(RecordingPresencePhase.ACTIVE, currentMs)
    }
}

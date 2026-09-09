package com.viti.operator.ui

import com.viti.operator.data.RecordingTrack
import org.junit.Assert.assertEquals
import org.junit.Test

class RecordingPresenceTest {
    private val lateShortTrack = RecordingTrack(
        id = "client-one",
        role = "client",
        participantId = "participant-one",
        clientId = "client-one",
        label = "Phone",
        contentType = "audio/wav",
        createdAt = "2026-09-09T12:00:05Z",
        offsetMs = 5_000,
        durationMs = 2_000,
    )

    @Test
    fun `late participant is inactive before its real offset`() {
        assertEquals(RecordingPresencePhase.BEFORE, recordingTrackPresence(lateShortTrack, 4_999).phase)
        assertEquals(RecordingPresencePhase.ACTIVE, recordingTrackPresence(lateShortTrack, 5_000).phase)
    }

    @Test
    fun `short participant is inactive at its real end`() {
        assertEquals(RecordingPresencePhase.ACTIVE, recordingTrackPresence(lateShortTrack, 6_999).phase)
        assertEquals(RecordingPresencePhase.AFTER, recordingTrackPresence(lateShortTrack, 7_000).phase)
    }
}

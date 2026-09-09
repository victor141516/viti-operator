package com.viti.operator

import com.viti.operator.data.Message
import org.junit.Assert.assertEquals
import org.junit.Test

class IncomingMessageTrackerTest {
    private val first = incomingMessage("first", "2026-09-08T10:00:00Z")
    private val second = incomingMessage("second", "2026-09-08T10:01:00Z")

    @Test
    fun initialHistoryBecomesBaselineWithoutNotification() {
        val tracker = IncomingMessageTracker()

        assertEquals(emptyList<Message>(), tracker.update("server-a", listOf(first)))
    }

    @Test
    fun onlyNewIncomingMessagesAreReportedOnce() {
        val tracker = IncomingMessageTracker()
        tracker.update("server-a", listOf(first))

        assertEquals(listOf(second), tracker.update("server-a", listOf(second, first)))
        assertEquals(emptyList<Message>(), tracker.update("server-a", listOf(second, first)))
    }

    @Test
    fun outboundMessagesNeverTriggerAndChangingServerCreatesANewBaseline() {
        val tracker = IncomingMessageTracker()
        val outbound = first.copy(id = "outbound", direction = "outgoing")
        tracker.update("server-a", listOf(first))

        assertEquals(emptyList<Message>(), tracker.update("server-a", listOf(outbound, first)))
        assertEquals(emptyList<Message>(), tracker.update("server-b", listOf(first, second)))
    }

    private fun incomingMessage(id: String, createdAt: String) = Message(
        id = id,
        direction = "incoming",
        from = "+34600000000",
        to = "",
        text = "Message $id",
        state = "received",
        createdAt = createdAt,
    )
}

package com.viti.operator.data

import org.junit.Assert.*
import org.junit.Test

class HistoryLedgerTest {
    private fun patch(mode: String = "delta", base: String = "history-fixture:0", cursor: String = "history-fixture:1",
                      messages: Map<String, String> = emptyMap(), calls: Map<String, String> = emptyMap(),
                      deleted: Set<String> = emptySet()) = HistoryPatch(mode, base, cursor, messages, calls, deleted)

    @Test fun fullThenDeltaUpdatesOlderRowsAndDeletes() {
        val full = applyHistoryPatch(HistoryCheckpoint(), patch("full", cursor = "history-fixture:0", messages = mapOf("old" to "pending", "gone" to "text")))
        val delta = applyHistoryPatch(full, patch(messages = mapOf("old" to "delivered"), calls = mapOf("call" to "ended"), deleted = setOf("gone")))
        assertEquals(mapOf("old" to "delivered"), delta.messages)
        assertEquals(mapOf("call" to "ended"), delta.calls)
        assertEquals("pending", full.messages["old"])
    }
    @Test fun invalidBaseAndRestartCannotAdvanceCheckpoint() {
        val current = HistoryCheckpoint("history-fixture:1", mapOf("old" to "text"))
        for (invalid in listOf(patch(base = "history-fixture:0", cursor = "history-fixture:2"), patch(base = current.cursor, cursor = "history-other:2"))) {
            assertThrows(IllegalArgumentException::class.java) { applyHistoryPatch(current, invalid) }
        }
        assertEquals("text", current.messages["old"])
        val reset = applyHistoryPatch(current, patch("full", cursor = "history-other:0"))
        assertTrue(reset.messages.isEmpty())
    }
    @Test fun duplicateCheckAndUnchangedResponseAreNoOps() {
        val current = HistoryCheckpoint("history-fixture:1", mapOf("one" to "text"))
        assertSame(current, applyHistoryPatch(current, patch()))
        assertSame(current, applyHistoryPatch(current, patch(base = current.cursor)))
    }
    @Test fun invalidCursorAndConflictingTombstoneRejected() {
        assertThrows(IllegalArgumentException::class.java) { applyHistoryPatch(HistoryCheckpoint(), patch("full", cursor = "invalid")) }
        assertThrows(IllegalArgumentException::class.java) { applyHistoryPatch(HistoryCheckpoint(), patch("full", messages = mapOf("one" to "text"), deleted = setOf("one"))) }
    }
}

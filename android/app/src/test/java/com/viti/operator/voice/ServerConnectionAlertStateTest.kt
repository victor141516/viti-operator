package com.viti.operator.voice

import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class ServerConnectionAlertStateTest {
    @Test
    fun silenceKeepsWarningVisibleAndStopsFurtherRingingDuringTheSameOutage() {
        val state = ServerConnectionAlertState()
        state.show()
        assertTrue(state.shouldRing)

        assertTrue(state.silence(state.outageToken))
        assertTrue(state.visible)
        assertFalse(state.shouldRing)

        // Poll updates and a changed failure reason belong to the same outage.
        repeat(3) { state.show() }
        assertTrue(state.visible)
        assertFalse(state.shouldRing)
    }

    @Test
    fun recoveryRearmsSoundForTheNextOutage() {
        val state = ServerConnectionAlertState()
        state.show()
        val previousToken = state.outageToken
        state.silence(previousToken)
        state.dismiss()
        assertFalse(state.visible)
        assertFalse(state.shouldRing)
        assertFalse(state.silenced)

        state.show()
        assertTrue(state.shouldRing)
        assertNotEquals(previousToken, state.outageToken)
        assertFalse(state.silence(previousToken))
        assertTrue(state.shouldRing)
    }

    @Test
    fun staleOrDuplicateActionsDoNotChangeTheAlertLifecycle() {
        val state = ServerConnectionAlertState()
        assertFalse(state.silence(null))
        assertFalse(state.silence("stale"))
        state.show()
        assertFalse(state.silence("stale"))
        assertTrue(state.shouldRing)
        val token = state.outageToken
        assertTrue(state.silence(token))
        assertFalse(state.silence(token))
        state.dismiss()
        assertFalse(state.silence(token))
        assertFalse(state.visible)
    }
}

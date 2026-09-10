package com.viti.operator

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class DialRouteTest {
    @Test
    fun onlyPrivateEngineExtensionsAreExplicitlyAllowlisted() {
        assertTrue(isInternalCallTarget("*43"))
        assertTrue(isInternalCallTarget(" *44 "))
        assertFalse(isInternalCallTarget("*45"))
        assertFalse(isInternalCallTarget("112"))
        assertFalse(isInternalCallTarget("+34612345678"))
    }
}

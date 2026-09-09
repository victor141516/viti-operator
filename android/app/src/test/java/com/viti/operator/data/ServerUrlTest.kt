package com.viti.operator.data

import org.junit.Assert.assertEquals
import org.junit.Test

class ServerUrlTest {
    @Test
    fun addsSchemeAndPrivatePortToBareTailscaleIp() {
        assertEquals("http://100.64.0.10:18788", normalizeServerUrl("100.64.0.10"))
    }

    @Test
    fun addsSchemeAndPrivatePortToBareHostname() {
        assertEquals("http://irsouate:18788", normalizeServerUrl("irsouate"))
    }

    @Test
    fun preservesExplicitPortAndHttpsDefault() {
        assertEquals("http://100.64.0.10:18787", normalizeServerUrl("http://100.64.0.10:18787"))
        assertEquals("https://irsouate.example", normalizeServerUrl("https://irsouate.example"))
    }
}

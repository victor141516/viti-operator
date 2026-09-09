package com.viti.operator

import com.viti.operator.data.ServerSettings
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class ContactRefreshPolicyTest {
    private val server = ServerSettings("http://server.example", "key")

    @Test
    fun unchangedServerVersionReusesCachedContacts() {
        assertFalse(shouldReloadContacts(server, 7, server, 7))
    }

    @Test
    fun changedVersionOrServerReloadsContacts() {
        assertTrue(shouldReloadContacts(server, 7, server, 8))
        assertTrue(shouldReloadContacts(server, 7, ServerSettings("http://other.example", "key"), 7))
        assertTrue(shouldReloadContacts(null, null, server, 0))
    }
}

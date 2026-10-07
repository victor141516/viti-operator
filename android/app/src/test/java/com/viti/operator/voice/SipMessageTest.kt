package com.viti.operator.voice

import org.junit.Assert.*
import org.junit.Test

class SipMessageTest {
    private fun request(body: String = "") = SipMessage("MESSAGE sip:test@example.invalid SIP/2.0", listOf(
        "v" to "SIP/2.0/WS native.invalid;branch=z9hG4bKtest", "f" to "<sip:test@example.invalid>;tag=test",
        "t" to "<sip:peer@example.invalid>", "i" to "test-call", "CSeq" to "1 MESSAGE"), body)

    @Test fun codecUsesUtf8LengthsCompactHeadersAndFoldedValues() {
        val message = request("café")
        val raw = message.encode().replace("CSeq: 1 MESSAGE", "CSeq: 1\r\n REGISTER")
        val parsed = SipMessage.parse(raw)!!
        assertEquals("test-call", parsed.callId)
        assertEquals("REGISTER", parsed.header("CSeq").substringAfter(' '))
        assertEquals("5", parsed.header("Content-Length"))
        assertEquals("café", parsed.body)
    }
    @Test fun malformedLengthsAndHeaderInjectionAreRejected() {
        assertNull(SipMessage.parse(request("data").encode().replace("Content-Length: 4", "Content-Length: 3")))
        assertNull(SipMessage.parse(request().encode().replace("Content-Length: 0", "Content-Length: 0\r\nContent-Length: 1")))
        try { request().replace("From", "caller\r\nInjected: value").encode(); fail("Header injection accepted") } catch (_: IllegalArgumentException) {}
    }
    @Test fun contactsAreSplitWithoutBreakingQuotedNamesOrUriParameters() {
        val contacts = SipMessage.commaValues("\"Caller, One\" <sip:a@local;transport=ws>;expires=20, <sip:b@local>;expires=300")
        assertEquals(2, contacts.size)
        assertEquals("a", SipMessage.user(contacts[0]))
        assertEquals("20", SipMessage.parameter(contacts[0], "expires"))
    }
    @Test fun rfcDigestVectorAndNonceCountsAreCorrect() {
        val digest = SipDigest("0a4f113b")
        val challenge = "Digest realm=\"testrealm@host.com\", qop=\"auth,auth-int\", nonce=\"dcd98b7102dd2f0e8b11d0f600bfb0c093\", opaque=\"5ccc069c403ebaf9f0171e9517f40e41\""
        val value = digest.authorization(challenge, "Mufasa", "Circle Of Life", "GET", "/dir/index.html")
        assertTrue(value.contains("response=\"6629fae49393a05397450978507c4ef1\""))
        assertTrue(value.contains("nc=00000001"))
        assertTrue(digest.authorization(challenge, "Mufasa", "Circle Of Life", "GET", "/dir/index.html").contains("nc=00000002"))
    }
    @Test fun unsupportedDigestModesFailClosed() {
        for (challenge in listOf("Basic test", "Digest realm=\"x\",nonce=\"n\",algorithm=unknown", "Digest realm=\"x\",nonce=\"n\",qop=\"auth-int\"")) {
            try { SipDigest().authorization(challenge, "u", "p", "REGISTER", "sip:example.invalid"); fail("Unsupported mode accepted") } catch (_: Exception) {}
        }
    }
}

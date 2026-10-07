package com.viti.operator.voice

import com.viti.operator.data.VoiceProvisioning
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.runTest
import kotlinx.coroutines.test.runCurrent
import org.junit.Assert.*
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class NativeSipEngineTest {
    private class FakeMedia : NativeVoiceMedia {
        var closed = false
        var accepts = 0
        var muted = false
        var inactive = false
        var digit = ' '
        override suspend fun offer() = "local-offer"
        override suspend fun answer(offer: String) = "local-answer"
        override suspend fun acceptAnswer(answer: String, provisional: Boolean) { accepts++ }
        override fun setAudioState(microphoneMuted: Boolean, inactive: Boolean) { muted = microphoneMuted; this.inactive = inactive }
        override fun dtmf(tone: Char) { digit = tone }
        override fun close() { closed = true }
    }
    private class Harness(scope: CoroutineScope) {
        var now = 1000L
        var socket = 0L
        val sent = mutableListOf<SipMessage>()
        val events = mutableListOf<VoiceEngineEvent>()
        val media = mutableListOf<FakeMedia>()
        val engine = NativeSipEngine(scope, "test-client", "Local test", { action, id, data ->
            if (action == "connect") socket = id
            if (action == "send") SipMessage.parse(data)?.let(sent::add)
            true
        }, { _, _ -> FakeMedia().also(media::add) }, events::add, { now })
        fun ready(expires: Int = 600) {
            engine.start(VoiceProvisioning(true, "ws://example.invalid/sipws", "example.invalid", "test", "local-password"))
            engine.socketEvent(NativeSipSocketEvent(socket, "open"))
            registered(expires)
        }
        fun registered(expires: Int = 600) {
            val req = sent.last { it.method == "REGISTER" }
            receive(SipMessage.response(req, 200, extra = listOf("Contact" to req.header("Contact").replace("expires=600", "expires=$expires"))))
        }
        fun receive(message: SipMessage) = engine.socketEvent(NativeSipSocketEvent(socket, "message", message.encode()))
        fun incoming(id: String = "test-incoming") = SipMessage("INVITE sip:test@example.invalid SIP/2.0", listOf(
            "Via" to "SIP/2.0/WS server.invalid;branch=z9hG4bK$id", "From" to "\"Caller\" <sip:caller@example.invalid>;tag=remote-tag",
            "To" to "<sip:test@example.invalid>", "Call-ID" to id, "CSeq" to "42 INVITE", "Contact" to "<sip:caller@server.invalid>",
            "X-Viti-Trace-ID" to "test-trace", "X-Viti-Participant-ID" to "test-participant", "X-Viti-Connection-ID" to "test-connection",
            "Content-Type" to "application/sdp"), "remote-offer")
        fun inDialog(invite: SipMessage, method: String, seq: Int = 43, wrongTag: Boolean = false): SipMessage {
            val reply = sent.last { it.status in listOf(180, 200) && it.callId == invite.callId }
            return invite.copy(start = "$method sip:test@example.invalid SIP/2.0", body = "")
                .replace("Via", "SIP/2.0/WS server.invalid;branch=z9hG4bK$method$seq")
                .replace("CSeq", "$seq $method").replace("To", if (wrongTag) "<sip:test@example.invalid>;tag=wrong" else reply.header("To"))
        }
        fun accepted() {
            val invite = sent.last { it.method == "INVITE" && it.status == 0 }
            receive(SipMessage.response(invite, 200, "server-tag", "remote-answer", listOf("Contact" to "<sip:server@server.invalid>")))
        }
    }

    @Test fun registrationKeepsRenewingBeyondTenMinutesWithoutAnyUiOrJavascript() = runTest {
        val h = Harness(this); h.ready()
        repeat(15) {
            h.now += 60_000; h.engine.maintain(); h.registered()
            assertTrue(h.engine.registrationExpiresAtMs > h.now)
        }
        assertEquals(16, h.sent.count { it.method == "REGISTER" })
        assertEquals(16, h.events.count { it.type == "registered" })
    }
    @Test fun challengeRetriesUseNewCseqAndOnlyOurContactConfirmsRegistration() = runTest {
        val h = Harness(this)
        h.engine.start(VoiceProvisioning(true, "ws://example.invalid", "example.invalid", "test", "password"))
        h.engine.socketEvent(NativeSipSocketEvent(h.socket, "open"))
        val first = h.sent.last()
        h.receive(SipMessage.response(first, 401, extra = listOf("WWW-Authenticate" to "Digest realm=\"local\",nonce=\"nonce\",algorithm=MD5,qop=\"auth\"")))
        val retry = h.sent.last()
        assertEquals(first.cseq + 1, retry.cseq)
        assertTrue(retry.header("Authorization").startsWith("Digest "))
        h.receive(SipMessage.response(retry, 200, extra = listOf("Contact" to "<sip:other@local>;expires=600")))
        assertEquals(0, h.events.count { it.type == "registered" })
        assertEquals("registration-failed", h.events.last().type)
    }
    @Test fun incomingAnswerRemoteHangupAndRetransmitsAreTokenScoped() = runTest {
        val h = Harness(this); h.ready()
        val invite = h.incoming(); h.receive(invite); h.receive(invite)
        val incoming = h.events.last()
        assertEquals("incoming", incoming.type)
        assertEquals("test-trace", incoming.traceId)
        assertEquals("test-connection", incoming.connectionId)
        assertEquals(1, h.events.count { it.type == "incoming" })
        h.engine.answer(incoming.legToken)
        runCurrent()
        assertEquals("active", h.events.last().type)
        h.receive(h.inDialog(invite, "ACK", 42))
        h.receive(h.inDialog(invite, "BYE", wrongTag = true))
        assertEquals(481, h.sent.last().status)
        assertEquals("active", h.events.last().type)
        val bye = h.inDialog(invite, "BYE")
        h.receive(bye); h.receive(bye)
        assertEquals(1, h.events.count { it.type == "ended" })
        assertTrue(h.media.single().closed)
    }
    @Test fun cancelBeforeAnswerEndsOnlyItsOwnLegAndAcknowledgesBothTransactions() = runTest {
        val h = Harness(this); h.ready(); val invite = h.incoming(); h.receive(invite)
        h.receive(invite.copy(start = "CANCEL ${invite.uri} SIP/2.0", body = "").replace("CSeq", "42 CANCEL"))
        assertEquals("ended", h.events.last().type)
        assertTrue(h.sent.any { it.status == 200 && it.method == "CANCEL" })
        assertTrue(h.sent.any { it.status == 487 && it.method == "INVITE" })
        assertTrue(h.media.isEmpty())
    }
    @Test fun lateSuccessfulAnswerAfterLocalHangupIsAckedAndReleasedNotResurrected() = runTest {
        val h = Harness(this); h.ready(); h.engine.call("leg", "+10000000000", "trace"); runCurrent()
        h.engine.end("leg")
        h.accepted(); runCurrent()
        assertEquals(0, h.events.count { it.type == "active" })
        assertTrue(h.sent.any { it.method == "ACK" && it.status == 0 })
        assertTrue(h.sent.any { it.method == "BYE" && it.status == 0 })
        assertEquals(1, h.events.count { it.type == "ended" })
    }
    @Test fun lossOfTransportPreservesLogicalIdentityForRejoinInsteadOfHangingUp() = runTest {
        val h = Harness(this); h.ready(); h.engine.call("leg", "+10000000000", "trace"); runCurrent(); h.accepted(); runCurrent()
        val first = h.events.last { it.type == "active" }
        h.engine.socketEvent(NativeSipSocketEvent(h.socket, "closed", error = true))
        assertEquals("interrupted", h.events.last().type)
        assertFalse(h.sent.any { it.method == "BYE" && it.status == 0 })
        h.now += 3000; h.engine.maintain(); h.engine.socketEvent(NativeSipSocketEvent(h.socket, "open")); h.registered()
        h.engine.call("leg", "*44", "trace", true); runCurrent()
        val rejoin = h.sent.last { it.method == "INVITE" }
        assertEquals("trace", rejoin.header("X-Viti-Trace-ID"))
        assertEquals("test-client", rejoin.header("X-Viti-Client-ID"))
        assertNotEquals(first.connectionId, rejoin.header("X-Viti-Connection-ID"))
    }
    @Test fun staleCommandsCannotChangeNewCallAndMuteStatesCompose() = runTest {
        val h = Harness(this); h.ready(); h.engine.call("leg", "+10000000000", "trace"); runCurrent(); h.accepted(); runCurrent()
        try { h.engine.end("old"); fail("Stale hangup accepted") } catch (_: VoiceCommandException) {}
        h.engine.mute("leg", true); h.engine.telecomMute("leg", true); h.engine.mute("leg", false)
        assertTrue(h.media.single().muted)
        h.engine.telecomMute("leg", false)
        assertFalse(h.media.single().muted)
        h.engine.inactive("leg", true)
        assertTrue(h.media.single().muted); assertTrue(h.media.single().inactive)
        h.engine.inactive("leg", false); h.engine.dtmf("leg", '5')
        assertEquals('5', h.media.single().digit)
    }
    @Test fun unansweredRegistrationExpiresAndTimedOutTransactionDoesNotWedgeRenewal() = runTest {
        val h = Harness(this); h.ready(20)
        h.now += 11_000; h.engine.maintain()
        h.now += 40_000; h.engine.maintain()
        assertEquals(0L, h.engine.registrationExpiresAtMs)
        assertTrue(h.events.any { it.type == "registration-failed" })
        h.now += 4000; h.engine.maintain(); h.registered()
        assertEquals("registered", h.events.last().type)
    }
    @Test fun secureTransportAndMinimumLeaseAreHonoredWithoutUnboundedChallengeLoops() = runTest {
        val h = Harness(this)
        h.engine.start(VoiceProvisioning(true, "wss://example.invalid", "example.invalid", "test", "password"))
        h.engine.socketEvent(NativeSipSocketEvent(h.socket, "open"))
        assertTrue(h.sent.last().header("Via").startsWith("SIP/2.0/WSS "))
        repeat(3) {
            val register = h.sent.last { it.method == "REGISTER" }
            h.receive(SipMessage.response(register, 423, extra = listOf("Min-Expires" to "900")))
        }
        assertEquals(3, h.sent.count { it.method == "REGISTER" })
        assertEquals("900", h.sent.last { it.method == "REGISTER" }.header("Expires"))
        assertEquals("registration-failed", h.events.last().type)
    }
}

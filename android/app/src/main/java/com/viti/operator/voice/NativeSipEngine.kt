package com.viti.operator.voice

import com.viti.operator.data.VoiceProvisioning
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.launch
import java.util.UUID

/** Main-scope SIP actor for one trusted Asterisk/WebSocket registration and call. */
internal class NativeSipEngine(
    private val scope: CoroutineScope,
    private val clientId: String,
    private val clientName: String,
    private val transport: (String, Long, String) -> Boolean,
    private val mediaFactory: (VoiceProvisioning, (NativeMediaEvent) -> Unit) -> NativeVoiceMedia,
    private val onEvent: (VoiceEngineEvent) -> Unit,
    private val now: () -> Long = System::currentTimeMillis,
) {
    private data class Transaction(val request: SipMessage, val deadline: Long, val attempts: Int = 0, val session: Session? = null)
    private class Session(val token: String, val peer: String, val trace: String, val participant: String,
        val connection: String, val callId: String, val localTag: String, val incoming: Boolean, val recovery: Boolean = false) {
        var invite: SipMessage? = null
        var media: NativeVoiceMedia? = null
        var operation: Job? = null
        var localSeq = 1L
        var remoteSeq = -1L
        var from = ""
        var to = ""
        var target = ""
        var routes = emptyList<String>()
        var active = false
        var announcedActive = false
        var ending = false
        var cancelled = false
        var terminal = false
        var muted = false
        var telecomMuted = false
        var inactive = false
        var reply: SipMessage? = null
        var replyDeadline = 0L
        var replyNext = 0L
        var replyInterval = 1000L
        var retireAt = Long.MAX_VALUE
    }
    private var provisioning = VoiceProvisioning()
    val enabled get() = provisioning.enabled
    private var socketId = 0L
    private var connected = false
    private var connectingAt: Long? = null
    private var nextConnectAt = 0L
    private var registeredUntil = 0L
    val registrationExpiresAtMs get() = registeredUntil
    private var nextRegisterAt = 0L
    private var registerCallId = SipMessage.token()
    private var registerSeq = 0L
    private var registerExpires = 600L
    private val registerTag = SipMessage.token()
    private val instanceId = UUID.nameUUIDFromBytes(clientId.toByteArray(Charsets.UTF_8)).toString()
    private val contactUser = "native" + instanceId.replace("-", "").take(20)
    private val digest = SipDigest()
    private var challenge: Pair<String, String>? = null
    private val transactions = mutableMapOf<String, Transaction>()
    private val sessions = mutableMapOf<String, Session>()
    private val serverReplies = linkedMapOf<String, Pair<Long, SipMessage>>()
    private var current: Session? = null
    private var destroyed = false

    fun start(value: VoiceProvisioning) {
        if (destroyed || value == provisioning) return
        stopConnection()
        provisioning = value
        challenge = null
        registerCallId = SipMessage.token()
        registerSeq = 0
        registerExpires = 600
        nextConnectAt = 0
        if (enabled) maintain() else onEvent(VoiceEngineEvent("disconnected"))
    }

    fun maintain() {
        if (destroyed || !enabled) return
        val time = now()
        transactions.values.filter { it.deadline <= time }.toList().forEach {
            transactions.remove(key(it.request))
            transactionFailed(it, "SIP response timed out")
        }
        sessions.values.toList().forEach { session ->
            if (session.retireAt <= time) sessions.remove(session.callId)
            else if (session.replyDeadline > 0) {
                if (session.replyDeadline <= time) {
                    session.replyDeadline = 0
                    if (session === current) interrupt(session, "SIP answer was not acknowledged")
                } else if (session.replyNext <= time) {
                    session.reply?.let(::send)
                    session.replyInterval = (session.replyInterval * 2).coerceAtMost(4000)
                    session.replyNext = time + session.replyInterval
                }
            }
        }
        serverReplies.entries.removeAll { it.value.first <= time }
        if (registeredUntil > 0 && registeredUntil <= time) {
            registeredUntil = 0
            onEvent(VoiceEngineEvent("unregistered"))
        }
        if (!connected) {
            if (connectingAt?.let { time - it >= 10_000 } == true) {
                transport("close", socketId, "")
                connectingAt = null
            }
            if (connectingAt == null && time >= nextConnectAt) {
                socketId++
                connectingAt = time
                nextConnectAt = time + 3000
                if (!transport("connect", socketId, provisioning.websocketUrl)) connectingAt = null
            }
        } else if (time >= nextRegisterAt && transactions.values.none { it.request.method == "REGISTER" }) register()
    }

    fun socketEvent(event: NativeSipSocketEvent) {
        if (destroyed || !enabled || event.id != socketId) return
        when (event.type) {
            "open" -> {
                connected = true; connectingAt = null; nextRegisterAt = 0
                onEvent(VoiceEngineEvent("connected"))
                register()
            }
            "closed" -> {
                connected = false; connectingAt = null; registeredUntil = 0
                transactions.clear()
                nextConnectAt = now() + 2000
                onEvent(VoiceEngineEvent("disconnected"))
                current?.let { interrupt(it, "Voice transport disconnected") }
            }
            "message" -> {
                if (event.message == "\r\n\r\n") { transport("send", socketId, "\r\n"); return }
                val message = SipMessage.parse(event.message) ?: return
                if (message.status > 0) response(message) else request(message)
            }
        }
    }

    fun call(token: String, number: String, trace: String, recovery: Boolean = false) {
        check(connected && registeredUntil > now()) { "The phone is not registered" }
        check(current == null) { "Another local call is active" }
        require(number.matches(Regex("(?:\\+[0-9]{3,20}|\\*[0-9]{2})"))) { "Invalid call destination" }
        val session = Session(token, number, trace.ifBlank { UUID.randomUUID().toString() }, clientId,
            UUID.randomUUID().toString(), SipMessage.token(), SipMessage.token(), false, recovery)
        session.from = "${localAddress()};tag=${session.localTag}"
        session.to = "<sip:$number@${provisioning.domain}>"
        session.target = SipMessage.address(session.to)
        current = session; sessions[session.callId] = session
        emit(session, "calling")
        session.operation = scope.launch {
            try {
                val media = media(session)
                val offer = media.offer()
                if (session !== current || session.ending) return@launch
                val invite = newRequest("INVITE", session.target, session.callId, session.localSeq, session.from, session.to,
                    listOf("Contact" to contact(), "X-Viti-Client-ID" to clientId, "X-Viti-Trace-ID" to session.trace,
                        "X-Viti-Connection-ID" to session.connection, "Content-Type" to "application/sdp"), offer)
                session.invite = invite
                submit(invite, session = session)
            } catch (_: Exception) { if (session === current && !session.ending) finish(session, "failed", "Could not prepare call audio") }
        }
    }

    fun answer(token: String) {
        val session = requireSession(token)
        check(session.incoming && !session.ending) { "This call cannot be answered" }
        if (session.active) return
        // Accept the action synchronously. ICE/SDP completes asynchronously,
        // as with the previous call engine; do not block Telecom's callback.
        if (session.operation?.isActive == true) return
        session.operation = scope.launch {
            try {
                val offer = session.invite ?: error("Missing call offer")
                check(offer.header("Content-Type").substringBefore(';').equals("application/sdp", true) && offer.body.isNotBlank()) { "Missing audio offer" }
                val answer = media(session).answer(offer.body)
                check(session === current && !session.ending) { "This call has ended" }
                answerReply(session, offer, answer)
                session.active = true
                session.announcedActive = true
                emit(session, "active")
            } catch (_: Exception) {
                if (session === current && !session.ending) {
                    session.invite?.let { cacheReply(it, SipMessage.response(it, 488, session.localTag)) }
                    finish(session, "failed", "Could not accept call audio")
                }
            }
        }
    }

    fun end(token: String, rejected: Boolean = false) {
        val session = requireSession(token)
        session.ending = true
        if (session.incoming && !session.active) {
            session.invite?.let { cacheReply(it, SipMessage.response(it, if (rejected) 603 else 486, session.localTag)) }
        } else if (session.active) submit(dialogRequest(session, "BYE"), session = session)
        // A pending outgoing INVITE is retained so a late final answer gets
        // ACK/BYE, never resurrecting a locally ended call.
        finish(session, "ended")
    }

    fun mute(token: String, muted: Boolean) {
        val session = requireSession(token)
        session.muted = muted; applyAudio(session)
        emit(session, "mute-changed", "muted=$muted")
    }
    fun telecomMute(token: String, muted: Boolean) { requireSession(token).also { it.telecomMuted = muted; applyAudio(it) } }
    fun inactive(token: String, inactive: Boolean) { requireSession(token).also { it.inactive = inactive; applyAudio(it) } }
    fun dtmf(token: String, tone: Char) {
        val session = requireSession(token)
        check(session.active) { "The call is not connected" }
        require(tone in "0123456789*#") { "Invalid DTMF key" }
        session.media?.dtmf(tone) ?: error("Call audio is unavailable")
        emit(session, "dtmf")
    }

    fun close() { destroyed = true; stopConnection() }

    private fun stopConnection() {
        current?.let { it.operation?.cancel(); it.media?.close() }
        current = null; sessions.clear(); transactions.clear(); serverReplies.clear()
        transport("close", socketId, "")
        connected = false; connectingAt = null; registeredUntil = 0
    }
    private fun localAddress() = "<sip:${provisioning.username}@${provisioning.domain}>"
    private fun contact() = "<sip:$contactUser@native.invalid;transport=ws>;reg-id=1;+sip.instance=\"<urn:uuid:$instanceId>\""
    private fun via() = "SIP/2.0/${if (provisioning.websocketUrl.startsWith("wss://")) "WSS" else "WS"} native.invalid;branch=z9hG4bK${SipMessage.token()};rport"
    private fun key(message: SipMessage) = "${message.callId}:${message.cseq}:${message.method}"
    private fun serverKey(message: SipMessage) = "${key(message)}:${message.branch}:${SipMessage.tag(message.header("From"))}:${SipMessage.tag(message.header("To"))}"
    private fun send(message: SipMessage) = connected && transport("send", socketId, message.encode())
    private fun requireSession(token: String) = current?.takeIf { it.token == token } ?: throw VoiceCommandException("This call leg is no longer current")
    private fun emit(session: Session, type: String, cause: String = "") {
        onEvent(VoiceEngineEvent(type, peer = session.peer, cause = cause, traceId = session.trace,
            participantId = session.participant, legToken = session.token, connectionId = session.connection))
    }
    private fun media(session: Session): NativeVoiceMedia {
        session.media?.let { return it }
        return mediaFactory(provisioning) { event ->
            scope.launch {
                if (session !== current || session.ending) return@launch
                when (event.state) {
                    "failed", "disconnected" -> interrupt(session, "Call audio disconnected")
                    "connected" -> emit(session, "media", "sent=${event.sent},received=${event.received}")
                }
                event.quality?.let { quality ->
                    emit(session, "media", "sent=${event.sent},received=${event.received}")
                    onEvent(VoiceEngineEvent("quality", legToken = session.token,
                        traceId = session.trace, connectionId = session.connection, quality = quality))
                }
            }
        }.also { session.media = it; applyAudio(session) }
    }
    private fun applyAudio(session: Session) { session.media?.setAudioState(session.muted || session.telecomMuted || session.inactive, session.inactive) }
    private fun finish(session: Session, type: String, cause: String = "") {
        if (session.terminal) return
        session.terminal = true; session.replyDeadline = 0; session.retireAt = now() + 64_000
        session.operation?.cancel(); session.media?.close(); session.media = null
        if (session !== current) return
        current = null
        emit(session, type, cause)
    }
    private fun interrupt(session: Session, cause: String) {
        finish(session, if (session.announcedActive || session.recovery) "interrupted" else "failed", cause)
    }

    private fun newRequest(method: String, uri: String, id: String, seq: Long, from: String, to: String,
        extra: List<Pair<String, String>> = emptyList(), body: String = ""): SipMessage {
        val request = SipMessage("$method $uri SIP/2.0", listOf("Via" to via(),
            "Max-Forwards" to "70", "From" to from, "To" to to, "Call-ID" to id, "CSeq" to "$seq $method",
            "User-Agent" to "Viti Operator · $clientName", "Supported" to "outbound, path", "Allow" to "INVITE, ACK, CANCEL, BYE, OPTIONS, INFO, UPDATE") + extra, body)
        return challenge?.let { (name, value) -> request.replace(name, digest.authorization(value, provisioning.username, provisioning.password, method, uri)) } ?: request
    }
    private fun submit(request: SipMessage, attempts: Int = 0, session: Session? = null) {
        val tx = Transaction(request, now() + 32_000, attempts, session)
        transactions[key(request)] = tx
        if (!send(request)) { transactions.remove(key(request)); transactionFailed(tx, "Voice transport unavailable") }
    }
    private fun register(attempts: Int = 0) {
        nextRegisterAt = now() + 60_000
        submit(newRequest("REGISTER", "sip:${provisioning.domain}", registerCallId, ++registerSeq,
            "${localAddress()};tag=$registerTag", localAddress(), listOf("Contact" to "${contact()};expires=$registerExpires", "Expires" to registerExpires.toString())), attempts)
    }

    private fun response(message: SipMessage) {
        val tx = transactions[key(message)]
        if (tx == null) {
            if (message.method == "INVITE" && message.status in 200..299) sessions[message.callId]?.takeIf {
                it.invite?.cseq == message.cseq && it.invite?.branch == message.branch
            }?.let { accepted(it, message) }
            return
        }
        if (message.branch != tx.request.branch) return
        val session = tx.session
        if (message.status < 200) {
            if (message.method == "INVITE" && session != null) {
                if (session.ending && !session.cancelled) {
                    session.cancelled = true
                    val cancel = tx.request.copy(start = "CANCEL ${tx.request.uri} SIP/2.0", body = "")
                        .replace("CSeq", "${tx.request.cseq} CANCEL")
                    submit(cancel, session = session)
                } else if (session === current && message.status == 180) emit(session, "ringing")
                else if (session === current && message.status == 183 && message.body.isNotBlank()) {
                    scope.launch { runCatching { session.media?.acceptAnswer(message.body, true) } }
                }
            }
            return
        }
        transactions.remove(key(message))
        if (message.method == "REGISTER" && message.status == 423 && tx.attempts < 2) {
            val minimum = message.header("Min-Expires").toLongOrNull()
            if (minimum != null && minimum in 1..86400) { registerExpires = minimum; register(tx.attempts + 1); return }
        }
        if (message.method == "INVITE" && message.status >= 300) {
            send(tx.request.copy(start = "ACK ${tx.request.uri} SIP/2.0", body = "")
                .replace("CSeq", "${tx.request.cseq} ACK").replace("To", message.header("To")))
        }
        if (message.status in listOf(401, 407) && tx.attempts < 2 && (session?.ending != true || message.method == "BYE")) {
            val name = if (message.status == 401) "WWW-Authenticate" else "Proxy-Authenticate"
            val authName = if (message.status == 401) "Authorization" else "Proxy-Authorization"
            try {
                val value = message.header(name)
                val seq = if (message.method == "REGISTER") ++registerSeq else session?.let { ++it.localSeq } ?: tx.request.cseq + 1
                val retried = tx.request.replace("Via", via())
                    .replace("CSeq", "$seq ${message.method}").replace(authName,
                        digest.authorization(value, provisioning.username, provisioning.password, message.method, tx.request.uri))
                challenge = authName to value
                if (message.method == "INVITE") session?.invite = retried
                submit(retried, tx.attempts + 1, session)
            } catch (_: Exception) { transactionFailed(tx, "SIP authentication failed") }
            return
        }
        if (message.status !in 200..299) { transactionFailed(tx, "SIP request rejected (${message.status})"); return }
        when (message.method) {
            "REGISTER" -> {
                val own = message.all("Contact").flatMap(SipMessage::commaValues).find { SipMessage.user(it) == contactUser }
                val seconds = own?.let { SipMessage.parameter(it, "expires").toLongOrNull() }
                    ?: message.header("Expires").toLongOrNull()
                if (own == null || seconds == null || seconds <= 0) { transactionFailed(tx, "Registration not confirmed"); return }
                registeredUntil = now() + seconds.coerceAtMost(86400) * 1000
                nextRegisterAt = now() + minOf(60_000, (seconds * 1000 / 2).coerceAtLeast(1000))
                onEvent(VoiceEngineEvent("registered", registrationExpiresAtMs = registeredUntil))
            }
            "INVITE" -> if (session != null) accepted(session, message)
        }
    }

    private fun accepted(session: Session, response: SipMessage) {
        session.to = response.header("To")
        session.target = response.header("Contact").takeIf { it.isNotBlank() }?.let(SipMessage::address) ?: session.target
        session.routes = response.all("Record-Route").flatMap(SipMessage::commaValues).reversed()
        send(dialogRequest(session, "ACK", response.cseq))
        if (session.ending || session.terminal) {
            if (!session.active) { session.active = true; submit(dialogRequest(session, "BYE"), session = session) }
            return
        }
        if (session.active || session !== current) return
        session.active = true
        session.operation = scope.launch {
            try {
                check(response.body.isNotBlank()) { "Missing call answer" }
                session.media?.acceptAnswer(response.body)
                if (session === current && !session.ending) { session.announcedActive = true; emit(session, "active") }
            } catch (_: Exception) { if (session === current) interrupt(session, "Could not connect call audio") }
        }
    }
    private fun dialogRequest(session: Session, method: String, seq: Long = ++session.localSeq): SipMessage =
        newRequest(method, session.target, session.callId, seq, session.from, session.to,
            session.routes.map { "Route" to it } + listOf("Contact" to contact(), "X-Viti-Client-ID" to clientId))

    private fun transactionFailed(tx: Transaction, reason: String) {
        if (tx.request.method == "REGISTER") {
            registeredUntil = 0; nextRegisterAt = now() + 3000
            onEvent(VoiceEngineEvent("registration-failed", cause = reason))
        } else if (tx.request.method == "INVITE") tx.session?.takeIf { it === current && !it.ending }?.let { interrupt(it, reason) }
    }

    private fun cacheReply(request: SipMessage, reply: SipMessage) {
        if (serverReplies.size >= 128) serverReplies.remove(serverReplies.keys.first())
        serverReplies[serverKey(request)] = (now() + 64_000) to reply
        send(reply)
    }
    private fun answerReply(session: Session, request: SipMessage, body: String) {
        val reply = SipMessage.response(request, 200, session.localTag, body,
            listOf("Contact" to contact(), "X-Viti-Client-ID" to clientId, "Allow" to "INVITE, ACK, CANCEL, BYE, OPTIONS, INFO, UPDATE"))
        session.reply = reply; session.replyDeadline = now() + 32_000; session.replyNext = now() + 1000
        cacheReply(request, reply)
    }
    private fun request(message: SipMessage) {
        if (message.method != "ACK") serverReplies[serverKey(message)]?.let { send(it.second); return }
        val session = sessions[message.callId]
        when (message.method) {
            "OPTIONS" -> cacheReply(message, SipMessage.response(message, 200, extra = listOf("Allow" to "INVITE, ACK, CANCEL, BYE, OPTIONS, INFO, UPDATE", "Accept" to "application/sdp")))
            "INVITE" -> {
                if (session != null) {
                    if (session !== current || session.ending || !dialogMatches(session, message)) { cacheReply(message, SipMessage.response(message, 481)); return }
                    if (!session.active || message.cseq <= session.remoteSeq || session.operation?.isActive == true) { cacheReply(message, SipMessage.response(message, 491, session.localTag)); return }
                    session.remoteSeq = message.cseq
                    session.operation = scope.launch {
                        try {
                            val answer = session.media?.answer(message.body) ?: error("No media")
                            if (session === current && !session.ending) answerReply(session, message, answer)
                        } catch (_: Exception) { cacheReply(message, SipMessage.response(message, 488, session.localTag)) }
                    }
                    return
                }
                if (current != null) { cacheReply(message, SipMessage.response(message, 486)); return }
                if (SipMessage.tag(message.header("To")).isNotBlank()) { cacheReply(message, SipMessage.response(message, 481)); return }
                val localTag = SipMessage.token()
                val created = Session(UUID.randomUUID().toString(), peerName(message.header("From")),
                    message.header("X-Viti-Trace-ID").ifBlank { message.callId },
                    message.header("X-Viti-Participant-ID").ifBlank { clientId },
                    message.header("X-Viti-Connection-ID").ifBlank { UUID.randomUUID().toString() }, message.callId, localTag, true)
                created.invite = message; created.remoteSeq = message.cseq
                created.from = SipMessage.withTag(message.header("To"), localTag)
                created.to = message.header("From")
                created.target = SipMessage.address(message.header("Contact").ifBlank { created.to })
                created.routes = message.all("Record-Route").flatMap(SipMessage::commaValues)
                current = created; sessions[created.callId] = created
                send(SipMessage.response(message, 100))
                cacheReply(message, SipMessage.response(message, 180, localTag))
                emit(created, "incoming")
            }
            "ACK" -> if (session === current && session != null && dialogMatches(session, message) && message.cseq == session.reply?.cseq) session.replyDeadline = 0
            "CANCEL" -> {
                val invite = session?.invite
                if (session == null || !session.incoming || invite == null || message.cseq != invite.cseq || message.branch != invite.branch) { cacheReply(message, SipMessage.response(message, 481)); return }
                cacheReply(message, SipMessage.response(message, 200))
                if (!session.active && session === current) {
                    cacheReply(invite, SipMessage.response(invite, 487, session.localTag))
                    finish(session, "ended")
                }
            }
            "BYE" -> {
                if (session == null || !dialogMatches(session, message)) { cacheReply(message, SipMessage.response(message, 481)); return }
                cacheReply(message, SipMessage.response(message, 200, session.localTag))
                if (session === current) finish(session, "ended")
            }
            "INFO", "UPDATE" -> {
                if (session == null || session !== current || !dialogMatches(session, message)) cacheReply(message, SipMessage.response(message, 481))
                else if (message.body.isNotEmpty() && message.method == "UPDATE") cacheReply(message, SipMessage.response(message, 488, session.localTag))
                else cacheReply(message, SipMessage.response(message, 200, session.localTag))
            }
            "NOTIFY", "MESSAGE" -> cacheReply(message, SipMessage.response(message, 200))
            else -> cacheReply(message, SipMessage.response(message, 501))
        }
    }
    private fun dialogMatches(session: Session, message: SipMessage) = SipMessage.tag(message.header("To")) == session.localTag &&
        SipMessage.tag(message.header("From")) == SipMessage.tag(session.to)
    private fun peerName(from: String): String {
        val display = from.substringBefore('<', "").trim().removeSurrounding("\"")
        return display.ifBlank { SipMessage.user(from).ifBlank { "Private number" } }
    }
}

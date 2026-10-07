package com.viti.operator.voice

import android.content.Context
import com.viti.operator.data.VoiceProvisioning
import com.viti.operator.data.VoiceCallQuality
import com.viti.operator.data.VoiceMediaRoute
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withTimeout
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.ensureActive
import org.webrtc.*
import org.webrtc.audio.JavaAudioDeviceModule
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

/** Native audio-only DTLS/SRTP peer. No WebView, browser, or idle microphone. */
internal class NativeWebRtcMedia(
    context: Context,
    provisioning: VoiceProvisioning,
    private val scope: CoroutineScope,
    private val onEvent: (NativeMediaEvent) -> Unit,
) : NativeVoiceMedia {
    private val audioDevice: JavaAudioDeviceModule
    private val factory: PeerConnectionFactory
    private val source: AudioSource
    private val localTrack: AudioTrack
    private val peer: PeerConnection
    private val sender: RtpSender
    @Volatile private var closed = false
    @Volatile private var gathered = CompletableDeferred<Unit>()
    @Volatile private var firstCandidate = CompletableDeferred<Unit>()
    private val candidateLock = Any()
    private val candidates = mutableListOf<IceCandidate>()
    private var statsJob: Job? = null
    private val quality = NativeVoiceQuality()
    private var inactive = false

    init {
        initialize(context.applicationContext)
        audioDevice = JavaAudioDeviceModule.builder(context.applicationContext).createAudioDeviceModule()
        factory = PeerConnectionFactory.builder().setAudioDeviceModule(audioDevice).createPeerConnectionFactory()
        source = factory.createAudioSource(MediaConstraints())
        localTrack = factory.createAudioTrack("viti-audio", source)
        val servers = provisioning.iceServers.map { server -> PeerConnection.IceServer.builder(server.urls)
            .setUsername(server.username).setPassword(server.credential).createIceServer() }
        val configuration = PeerConnection.RTCConfiguration(servers).apply {
            sdpSemantics = PeerConnection.SdpSemantics.UNIFIED_PLAN
            bundlePolicy = PeerConnection.BundlePolicy.MAXBUNDLE
            rtcpMuxPolicy = PeerConnection.RtcpMuxPolicy.REQUIRE
            enableIceGatheringOnAnyAddressPorts = true
            if (provisioning.iceServers.any { it.urls.any { url -> url.startsWith("turn:") || url.startsWith("turns:") } })
                iceTransportsType = PeerConnection.IceTransportsType.RELAY
        }
        peer = factory.createPeerConnection(configuration, object : PeerConnection.Observer {
            override fun onSignalingChange(state: PeerConnection.SignalingState) = Unit
            override fun onIceConnectionChange(state: PeerConnection.IceConnectionState) = Unit
            override fun onIceConnectionReceivingChange(receiving: Boolean) = Unit
            override fun onIceGatheringChange(state: PeerConnection.IceGatheringState) {
                if (state == PeerConnection.IceGatheringState.COMPLETE) gathered.complete(Unit)
            }
            override fun onConnectionChange(state: PeerConnection.PeerConnectionState) {
                if (closed) return
                scope.launch {
                    if (closed) return@launch
                    when (state) {
                        PeerConnection.PeerConnectionState.CONNECTED -> {
                            onEvent(NativeMediaEvent("connected"))
                            startStats()
                        }
                        PeerConnection.PeerConnectionState.DISCONNECTED -> onEvent(NativeMediaEvent("disconnected"))
                        PeerConnection.PeerConnectionState.FAILED -> onEvent(NativeMediaEvent("failed"))
                        else -> Unit
                    }
                }
            }
            override fun onIceCandidate(candidate: IceCandidate) {
                synchronized(candidateLock) { candidates.add(candidate) }
                firstCandidate.complete(Unit)
            }
            override fun onIceCandidatesRemoved(removed: Array<out IceCandidate>) {
                synchronized(candidateLock) { candidates.removeAll { candidate -> removed.any { it.sdp == candidate.sdp && it.sdpMLineIndex == candidate.sdpMLineIndex } } }
            }
            override fun onAddStream(stream: MediaStream) = Unit
            override fun onRemoveStream(stream: MediaStream) = Unit
            override fun onDataChannel(channel: DataChannel) = Unit
            override fun onRenegotiationNeeded() = Unit
        }) ?: error("Could not create native audio peer")
        sender = peer.addTrack(localTrack, listOf("viti-call")) ?: error("Could not add native audio")
    }

    override suspend fun offer(): String = localDescription(SessionDescription.Type.OFFER)
    override suspend fun answer(offer: String): String {
        setDescription(SessionDescription(SessionDescription.Type.OFFER, offer), false)
        return localDescription(SessionDescription.Type.ANSWER)
    }
    override suspend fun acceptAnswer(answer: String, provisional: Boolean) {
        setDescription(SessionDescription(if (provisional) SessionDescription.Type.PRANSWER else SessionDescription.Type.ANSWER, answer), false)
    }

    private suspend fun localDescription(type: SessionDescription.Type): String {
        check(!closed) { "Call audio has ended" }
        val constraints = MediaConstraints().apply { mandatory.add(MediaConstraints.KeyValuePair("OfferToReceiveAudio", "true")) }
        val description = withTimeout(10_000) {
            suspendCancellableCoroutine<SessionDescription> { continuation ->
                val observer = object : SdpObserver {
                    override fun onCreateSuccess(value: SessionDescription) { if (continuation.isActive) continuation.resume(value) }
                    override fun onCreateFailure(reason: String) { if (continuation.isActive) continuation.resumeWithException(VoiceCommandException("Could not negotiate audio")) }
                    override fun onSetSuccess() = Unit
                    override fun onSetFailure(reason: String) = Unit
                }
                if (type == SessionDescription.Type.OFFER) peer.createOffer(observer, constraints) else peer.createAnswer(observer, constraints)
            }
        }
        val oldUfrag = peer.localDescription?.description?.let { Regex("(?m)^a=ice-ufrag:([^\\r\\n]+)").find(it)?.groupValues?.get(1) }
        val newUfrag = Regex("(?m)^a=ice-ufrag:([^\\r\\n]+)").find(description.description)?.groupValues?.get(1)
        if (oldUfrag != newUfrag) {
            synchronized(candidateLock) { candidates.clear() }
            firstCandidate = CompletableDeferred()
        }
        gathered = CompletableDeferred()
        setDescription(description, true)
        if (peer.iceGatheringState() == PeerConnection.IceGatheringState.COMPLETE) gathered.complete(Unit)
        withTimeout(10_000) { gathered.await() }
        withTimeout(10_000) { firstCandidate.await() }
        check(!closed) { "Call audio has ended" }
        val original = peer.localDescription?.description ?: error("Missing native audio description")
        val additions = synchronized(candidateLock) { candidates.map { it.sdpMLineIndex to it.sdp } }
        val sdp = NativeSipSdp.withCandidates(original, additions)
        check(sdp.contains("a=candidate:")) { "No usable audio network candidate" }
        return sdp
    }

    private suspend fun setDescription(value: SessionDescription, local: Boolean) = withTimeout(10_000) {
        check(!closed) { "Call audio has ended" }
        suspendCancellableCoroutine<Unit> { continuation ->
            val observer = object : SdpObserver {
                override fun onSetSuccess() { if (continuation.isActive) continuation.resume(Unit) }
                override fun onSetFailure(reason: String) { if (continuation.isActive) continuation.resumeWithException(VoiceCommandException("Could not apply call audio description")) }
                override fun onCreateSuccess(value: SessionDescription) = Unit
                override fun onCreateFailure(reason: String) = Unit
            }
            if (local) peer.setLocalDescription(observer, value) else peer.setRemoteDescription(observer, value)
        }
    }

    override fun setAudioState(microphoneMuted: Boolean, inactive: Boolean) {
        if (closed) return
        this.inactive = inactive
        localTrack.setEnabled(!microphoneMuted)
        audioDevice.setMicrophoneMute(microphoneMuted)
        audioDevice.setSpeakerMute(inactive)
    }
    override fun dtmf(tone: Char) {
        check(!closed && !inactive) { "Call audio is unavailable" }
        val dtmf = sender.dtmf()
        check(dtmf != null && dtmf.canInsertDtmf() && dtmf.insertDtmf(tone.toString(), 160, 100)) { "Could not send the DTMF tone" }
    }
    private fun startStats() {
        if (statsJob?.isActive == true) return
        statsJob = scope.launch {
            while (!closed) {
                val report = runCatching { withTimeout(5000) {
                    suspendCancellableCoroutine<RTCStatsReport> { continuation ->
                        peer.getStats { value -> if (continuation.isActive) continuation.resume(value) }
                    }
                } }.getOrNull()
                currentCoroutineContext().ensureActive()
                if (!closed && report != null) {
                    val values = report.statsMap.values.map { NativeRtcStat(it.id, it.type, it.timestampUs / 1000, it.members) }
                    val sample = quality.sample(values)
                    onEvent(NativeMediaEvent(quality = sample,
                        sent = values.any { it.type == "outbound-rtp" && (it.values["bytesSent"] as? Number)?.toLong()?.let { n -> n > 0 } == true },
                        received = values.any { it.type == "inbound-rtp" && (it.values["bytesReceived"] as? Number)?.toLong()?.let { n -> n > 0 } == true }))
                } else if (!closed) onEvent(NativeMediaEvent(quality = quality.unavailable()))
                delay(2000)
            }
        }
    }
    override fun close() {
        if (closed) return
        closed = true
        gathered.cancel()
        firstCandidate.cancel()
        statsJob?.cancel()
        peer.close(); peer.dispose()
        localTrack.dispose(); source.dispose(); factory.dispose(); audioDevice.release()
    }
    companion object {
        private var initialized = false
        @Synchronized private fun initialize(context: Context) {
            if (initialized) return
            PeerConnectionFactory.initialize(PeerConnectionFactory.InitializationOptions.builder(context).createInitializationOptions())
            initialized = true
        }
    }
}

internal object NativeSipSdp {
    // JNI descriptions may omit gathered candidates. SIP needs a complete,
    // non-trickle offer/answer, with each candidate in its own media section.
    fun withCandidates(sdp: String, candidates: List<Pair<Int, String>>): String {
        val result = mutableListOf<String>()
        var section = -1
        fun append() {
            candidates.filter { it.first == section }.map { "a=${it.second.removePrefix("a=")}" }
                .distinct().filterNot { it in result }.forEach(result::add)
        }
        sdp.trimEnd('\r', '\n').split("\r\n").forEach { line ->
            if (line.startsWith("m=")) { append(); section++ }
            result.add(line)
        }
        append()
        return result.joinToString("\r\n", postfix = "\r\n")
    }
}

internal data class NativeRtcStat(val id: String, val type: String, val atMs: Double, val values: Map<String, Any>)

/** Interval loss only: resets, idle reports and replaced SSRCs remain unavailable. */
internal class NativeVoiceQuality {
    private var previous = emptyMap<String, NativeRtcStat>()
    private var sequence = 0L
    fun unavailable(): VoiceCallQuality {
        previous = emptyMap()
        return VoiceCallQuality(++sequence, "unavailable", System.currentTimeMillis())
    }
    fun sample(stats: List<NativeRtcStat>, nowMs: Long = System.currentTimeMillis()): VoiceCallQuality {
        fun NativeRtcStat.number(name: String) = (values[name] as? Number)?.toDouble()?.takeIf { it.isFinite() }
        val inbound = stats.firstOrNull { it.type == "inbound-rtp" && (it.values["kind"] == "audio" || it.values["mediaType"] == "audio") }
        val outbound = stats.firstOrNull { it.type == "remote-inbound-rtp" && (it.values["kind"] == "audio" || it.values["mediaType"] == "audio") }
        fun loss(value: NativeRtcStat?): Pair<Double?, Double?> {
            val old = value?.let { previous[it.id] } ?: return null to null
            val packets = value.number("packetsReceived") ?: return null to null
            val lost = value.number("packetsLost") ?: return null to null
            val dp = packets - (old.number("packetsReceived") ?: return null to null)
            val dl = lost - (old.number("packetsLost") ?: return null to null)
            val elapsed = value.atMs - old.atMs
            return if (dp < 0 || dl < 0 || dp + dl <= 0 || elapsed <= 0) null to null else (100 * dl / (dp + dl)) to elapsed
        }
        val (inLoss, inWindow) = loss(inbound)
        val (outLoss, outWindow) = loss(outbound)
        val selected = stats.firstOrNull { it.type == "transport" }?.values?.get("selectedCandidatePairId")
        val pair = stats.find { it.type == "candidate-pair" && it.id == selected } ?: stats.firstOrNull { it.type == "candidate-pair" && it.values["nominated"] == true && it.values["state"] == "succeeded" }
        val local = stats.find { it.id == pair?.values?.get("localCandidateId") }
        val remote = stats.find { it.id == pair?.values?.get("remoteCandidateId") }
        val route = pair?.let { VoiceMediaRoute(if (local?.values?.get("candidateType") == "relay" || remote?.values?.get("candidateType") == "relay") "turn-relay" else "direct",
            (local?.values?.get("protocol") as? String) ?: "unknown", (local?.values?.get("candidateType") as? String) ?: "unknown", (remote?.values?.get("candidateType") as? String) ?: "unknown") }
        val mediaRtt = outbound?.number("roundTripTime")?.times(1000)
        val iceRtt = pair?.number("currentRoundTripTime")?.times(1000)
        previous = stats.associateBy { it.id }
        return VoiceCallQuality(++sequence, if (inLoss != null || outLoss != null) "available" else "collecting", nowMs,
            inLoss, outLoss, inbound?.number("jitter")?.times(1000), mediaRtt ?: iceRtt,
            if (mediaRtt != null) "media-peer" else if (iceRtt != null) "ice-path" else "", inWindow, outWindow, route)
    }
}

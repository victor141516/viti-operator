package com.viti.operator.data

import java.net.URI

private const val DEFAULT_SERVER_URL = "http://127.0.0.1:18788"
private const val DEFAULT_TAILSCALE_HTTP_PORT = 18788

fun normalizeServerUrl(value: String): String {
    val raw = value.trim().ifBlank { DEFAULT_SERVER_URL }
    val withScheme = if (raw.contains("://")) raw else "http://$raw"
    return runCatching {
        val uri = URI(withScheme)
        require(!uri.host.isNullOrBlank())
        val port = when {
            uri.port >= 0 -> uri.port
            uri.scheme.equals("http", ignoreCase = true) -> DEFAULT_TAILSCALE_HTTP_PORT
            else -> -1
        }
        URI(uri.scheme, uri.userInfo, uri.host, port, uri.path, uri.query, uri.fragment)
            .toString()
            .trimEnd('/')
    }.getOrElse { withScheme.trimEnd('/') }
}

data class ServerSettings(
    val serverUrl: String = DEFAULT_SERVER_URL,
    val apiKey: String = "",
)

data class RuntimeStatus(
    val ready: Boolean = false,
    val simReady: Boolean = false,
    val tunnelReady: Boolean = false,
    val imsReady: Boolean = false,
    val contactsVersion: Long = 0,
    val detail: String = "",
)

data class ServerContactsSnapshot(val contacts: List<PhoneContact>, val version: Long)

data class ClientIdentity(
    val id: String,
    val name: String,
    val hasSim: Boolean,
    val cellular: CellularObservation? = null,
)

data class CellularObservation(
    val generation: String,
    val generationStartedAt: String,
    val sequence: Long,
    val measuredAt: String,
    val subscriptionRole: String,
    val homeNetwork: CellularNetworkDetails,
    val servingNetwork: CellularNetworkDetails,
    val service: CellularServiceDetails,
    val dataAccess: CellularDataAccessDetails,
    val signal: CellularSignalDetails,
    val receivedAt: String = "",
    val stale: Boolean = false,
)

data class CellularNetworkDetails(
    val availability: String = "unknown",
    val operator: String = "",
    val mcc: String = "",
    val mnc: String = "",
    val measuredAt: String = "",
)

data class CellularServiceDetails(
    val availability: String = "unknown",
    val registration: String = "",
    val roaming: Boolean? = null,
    val measuredAt: String = "",
)

data class CellularDataAccessDetails(
    val availability: String = "unknown",
    val technology: String = "",
    val measuredAt: String = "",
)

data class CellularSignalDetails(
    val availability: String = "unknown",
    val level: Int? = null,
    val dbm: Int? = null,
    val measuredAt: String = "",
)

data class TopologyNode(
    val state: String = "idle",
    val label: String = "No data",
    val detail: String = "",
    val evidence: String = "",
    val observedAt: String = "",
    val mcc: String = "",
    val mnc: String = "",
)

data class TopologySim(
    val state: String = "idle",
    val label: String = "SIM not configured",
    val detail: String = "",
    val mode: String = "disabled",
    val holderClientId: String = "",
    val authAgentId: String = "",
    val selected: Boolean = false,
    val cellularStatus: String = "unknown",
    val cellular: CellularObservation? = null,
)

data class TopologyClient(
    val id: String,
    val kind: String,
    val name: String,
    val hasSim: Boolean,
    val simHolder: Boolean,
    val online: Boolean,
    val lastSeen: String,
    val cellular: CellularObservation? = null,
)

data class SIMAuthAssignmentStatus(
    val mode: String = "disabled",
    val clientId: String = "",
    val claimedClientId: String = "",
)

data class ConnectionTopology(
    val generatedAt: String = "",
    val server: TopologyNode = TopologyNode(label = "Viti server"),
    val sim: TopologySim = TopologySim(),
    val epdg: TopologyNode = TopologyNode(label = "ePDG tunnel"),
    val ims: TopologyNode = TopologyNode(label = "IMS registration"),
    val operator: TopologyNode = TopologyNode(label = "Mobile carrier"),
    val clients: List<TopologyClient> = emptyList(),
)

data class IMSRegistrationState(
    val enabled: Boolean = true,
    val registered: Boolean = false,
    val detail: String = "",
)

data class PhoneRoutingSettings(
    val remoteSimModeEnabled: Boolean = false,
    val restoreNetworkMask: String = "",
)

data class Message(
    val id: String,
    val direction: String,
    val from: String,
    val to: String,
    val text: String,
    val state: String,
    val deliveryState: String = "",
    val deliveryReportRequested: Boolean = false,
    val statusDetail: String = "",
    val failureCode: String = "",
    val failureReason: String = "",
    val parts: List<SMSPartStatus> = emptyList(),
    val createdAt: String,
    val contactId: String = "",
    val contactName: String = "",
)

data class SMSPartStatus(
    val part: Int,
    val parts: Int,
    val state: String,
    val statusCode: String = "",
    val failureCode: String = "",
    val failureReason: String = "",
    val detail: String = "",
)

data class Call(
    val id: String,
	val traceId: String = "",
    val direction: String,
    val peer: String,
    val state: String,
    val startedAt: String,
    val endedAt: String,
    val recordingTracks: List<RecordingTrack> = emptyList(),
    val contactId: String = "",
    val contactName: String = "",
)

data class RecordingTrack(
    val id: String,
    val role: String,
    val participantId: String,
    val clientId: String,
    val label: String,
    val contentType: String,
    val createdAt: String,
    val offsetMs: Long,
    val durationMs: Long,
)

data class RecordingSettings(val enabled: Boolean = false)

data class VoiceProvisioning(
    val enabled: Boolean = false,
    val websocketUrl: String = "",
    val domain: String = "",
    val username: String = "",
    val password: String = "",
    val iceServers: List<VoiceIceServer> = emptyList(),
    val diagnosticsEnabled: Boolean = false,
)

data class VoiceIceServer(
    val urls: List<String>,
    val username: String = "",
    val credential: String = "",
)

data class VoiceUiState(
    val state: String = "disconnected",
    val peer: String = "",
    val detail: String = "Telephony disconnected",
    val microphoneMuted: Boolean = false,
    val quality: VoiceCallQuality? = null,
) {
    val registered: Boolean get() = state == "registered"
    val incoming: Boolean get() = state == "incoming"
    val ending: Boolean get() = state == "ending"
	val inCall: Boolean get() = state in setOf("calling", "ringing", "active", "media", "reconnecting")
}

data class VoiceCallQuality(
    val sequence: Long,
    val status: String,
    val sampledAtMs: Long,
    val inboundLossPercent: Double? = null,
    val outboundLossPercent: Double? = null,
    val inboundJitterMs: Double? = null,
    val roundTripTimeMs: Double? = null,
    val roundTripScope: String = "",
    val inboundWindowMs: Double? = null,
    val outboundWindowMs: Double? = null,
    val route: VoiceMediaRoute? = null,
)

data class VoiceMediaRoute(
    val mode: String,
    val protocol: String,
    val localCandidateType: String,
    val remoteCandidateType: String,
)

data class AuthChallenge(
    val id: String,
    val rand: String,
    val autn: String,
)

data class AuthMaterial(
    val res: String = "",
    val ck: String = "",
    val ik: String = "",
    val auts: String = "",
)

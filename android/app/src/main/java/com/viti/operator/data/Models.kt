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
    val detail: String = "",
)

data class ClientIdentity(
    val id: String,
    val name: String,
    val hasSim: Boolean,
)

data class TopologyNode(
    val state: String = "idle",
    val label: String = "No data",
    val detail: String = "",
)

data class TopologySim(
    val state: String = "idle",
    val label: String = "SIM not configured",
    val detail: String = "",
    val mode: String = "disabled",
    val holderClientId: String = "",
    val authAgentId: String = "",
)

data class TopologyClient(
    val id: String,
    val kind: String,
    val name: String,
    val hasSim: Boolean,
    val simHolder: Boolean,
    val online: Boolean,
    val lastSeen: String,
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
    val createdAt: String,
)

data class Call(
    val id: String,
    val direction: String,
    val peer: String,
    val state: String,
    val startedAt: String,
    val endedAt: String,
    val recordingTracks: List<RecordingTrack> = emptyList(),
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
) {
    val registered: Boolean get() = state == "registered"
    val incoming: Boolean get() = state == "incoming"
    val inCall: Boolean get() = state in setOf("calling", "ringing", "active", "media")
}

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

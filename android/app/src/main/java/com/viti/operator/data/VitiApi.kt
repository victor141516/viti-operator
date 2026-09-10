package com.viti.operator.data

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.json.JSONArray
import org.json.JSONObject
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL

class VitiApi(private val settings: ServerSettings) {
    suspend fun status(): RuntimeStatus = request("/api/v1/status").let { body ->
        RuntimeStatus(
            ready = body.optBoolean("ready"),
            simReady = body.optBoolean("sim_ready"),
            tunnelReady = body.optBoolean("tunnel_ready"),
            imsReady = body.optBoolean("ims_ready"),
            contactsVersion = body.optLong("contacts_version"),
            detail = body.optString("detail"),
        )
    }

    suspend fun heartbeat(identity: ClientIdentity, authActive: Boolean = false) {
        val currentIdentity = identity.withCurrentCellularObservation()
        val payload = JSONObject()
            .put("id", currentIdentity.id)
            .put("kind", "android")
            .put("name", currentIdentity.name)
            .put("auth_agent_id", currentIdentity.id)
            .put("has_sim", currentIdentity.hasSim)
            .put("auth_active", authActive)
        currentIdentity.cellular?.let { payload.put("cellular", it.toJson()) }
        rawRequest(
            path = "/api/v1/clients/heartbeat",
            method = "POST",
            body = payload,
            acceptedStatuses = setOf(204),
        )
    }

    suspend fun setSIMAuthAssignment(identity: ClientIdentity, enabled: Boolean) {
        rawRequest(
            path = "/api/v1/sim-auth/assignment",
            method = "PUT",
            body = JSONObject()
                .put("client_id", identity.id)
                .put("agent_id", identity.id)
                .put("enabled", enabled),
            acceptedStatuses = setOf(200),
        )
    }

    suspend fun simAuthAssignment(): SIMAuthAssignmentStatus = request("/api/v1/sim-auth/assignment").let { body ->
        SIMAuthAssignmentStatus(
            mode = body.optString("mode").ifBlank { "disabled" },
            clientId = body.optString("client_id"),
            claimedClientId = body.optString("claimed_client_id"),
        )
    }

    suspend fun topology(): ConnectionTopology = request("/api/v1/topology").let { body ->
        ConnectionTopology(
            generatedAt = body.optString("generated_at"),
            server = topologyNodeFromJson(body.optJSONObject("server")),
            sim = body.optJSONObject("sim").let { sim ->
                TopologySim(
                    state = sim?.optString("state").orEmpty().ifBlank { "idle" },
                    label = sim?.optString("label").orEmpty().ifBlank { "SIM not configured" },
                    detail = sim?.optString("detail").orEmpty(),
                    mode = sim?.optString("mode").orEmpty().ifBlank { "disabled" },
                    holderClientId = sim?.optString("holder_client_id").orEmpty(),
                    authAgentId = sim?.optString("auth_agent_id").orEmpty(),
                    selected = sim?.optBoolean("selected") == true,
                    cellularStatus = sim?.optString("cellular_status").orEmpty().ifBlank { "unknown" },
                    cellular = cellularFromJson(sim?.optJSONObject("cellular")),
                )
            },
            epdg = topologyNodeFromJson(body.optJSONObject("epdg"), "ePDG tunnel"),
            ims = topologyNodeFromJson(body.optJSONObject("ims"), "IMS registration"),
            operator = topologyNodeFromJson(body.optJSONObject("operator"), "Mobile carrier"),
            clients = body.optJSONArray("clients").mapObjects { client ->
                TopologyClient(
                    id = client.optString("id"),
                    kind = client.optString("kind"),
                    name = client.optString("name"),
                    hasSim = client.optBoolean("has_sim"),
                    simHolder = client.optBoolean("sim_holder"),
                    online = client.optBoolean("online"),
                    lastSeen = client.optString("last_seen"),
                    cellular = cellularFromJson(client.optJSONObject("cellular")),
                )
            },
        )
    }

	suspend fun imsRegistration(): IMSRegistrationState = request("/api/v1/ims-registration").let { body ->
		IMSRegistrationState(
			enabled = body.optBoolean("enabled", true),
			registered = body.optBoolean("registered"),
			detail = body.optString("detail"),
		)
	}

	suspend fun setIMSRegistration(enabled: Boolean): IMSRegistrationState = request(
		path = "/api/v1/ims-registration",
		method = "PUT",
		body = JSONObject().put("enabled", enabled),
	).let { body ->
		IMSRegistrationState(
			enabled = body.optBoolean("enabled", enabled),
			registered = body.optBoolean("registered"),
			detail = body.optString("detail"),
		)
	}

    suspend fun messages(): List<Message> = request("/api/v1/messages")
        .optJSONArray("messages")
        .mapObjects(::messageFromJson)

    suspend fun calls(): List<Call> = request("/api/v1/calls")
        .optJSONArray("calls")
        .mapObjects(::callFromJson)

	suspend fun isCarrierCallActive(traceId: String): Boolean =
		calls().any { it.traceId == traceId && it.endedAt.isBlank() && it.state == "active" }

	suspend fun leaveCallParticipant(traceId: String, identity: ClientIdentity, connectionId: String, operationId: String) {
		rawRequest(
			path = "/api/v1/calls/participants/leave",
			method = "POST",
			body = JSONObject()
				.put("trace_id", traceId)
				.put("client_id", identity.id)
				.put("connection_id", connectionId)
				.put("operation_id", operationId),
			acceptedStatuses = setOf(200),
		)
	}

    suspend fun contacts(query: String = ""): ServerContactsSnapshot {
        val suffix = if (query.isBlank()) "" else "?q=${java.net.URLEncoder.encode(query, Charsets.UTF_8.name())}"
        val body = request("/api/v1/contacts$suffix")
        val contacts = body.optJSONArray("contacts").mapObjects { contact ->
            val id = contact.optString("id")
            val name = contact.optString("name")
            val names = contact.optJSONArray("names").mapStrings()
            val sources = contact.optJSONArray("sources").mapObjects { it.optString("client_name") }
            contact.optJSONArray("numbers").mapObjects { number ->
                PhoneContact(
                    id = "$id:${number.optString("normalized").ifBlank { number.optString("original") }}",
                    name = name,
                    number = number.optString("normalized").ifBlank { number.optString("original") },
                    alternativeNames = names.drop(1),
                    sourceNames = sources,
                )
            }
        }.flatten()
        return ServerContactsSnapshot(contacts, body.optLong("version"))
    }

    suspend fun replaceContacts(identity: ClientIdentity, snapshot: DeviceContactsSnapshot) {
        val body = withContext(Dispatchers.Default) {
            val contacts = JSONArray()
            snapshot.contacts.forEach { contact ->
                val numbers = JSONArray()
                contact.numbers.forEach { number ->
                    numbers.put(JSONObject().put("original", number.original).put("normalized", number.normalized).put("label", number.label))
                }
                contacts.put(JSONObject().put("source_contact_id", contact.id).put("names", JSONArray(contact.names)).put("numbers", numbers))
            }
            JSONObject()
                .put("client_name", identity.name)
                .put("region", snapshot.region)
                .put("snapshot_revision", snapshot.revision)
                .put("contacts", contacts)
        }
        rawRequest(
            path = "/api/v1/contacts/sources/${urlSegment(identity.id)}",
            method = "PUT",
            body = body,
            acceptedStatuses = setOf(204),
        )
    }

    suspend fun recordingSettings(): RecordingSettings = request("/api/v1/recording").let {
        RecordingSettings(it.optBoolean("enabled"))
    }

    suspend fun setRecordingEnabled(enabled: Boolean): RecordingSettings = request(
        path = "/api/v1/recording",
        method = "PUT",
        body = JSONObject().put("enabled", enabled),
    ).let { RecordingSettings(it.optBoolean("enabled")) }

    suspend fun recordingAudio(callId: String, trackIds: List<String>): ByteArray = withContext(Dispatchers.IO) {
        require(trackIds.isNotEmpty()) { "Select at least one recording track" }
        val encodedTracks = java.net.URLEncoder.encode(trackIds.joinToString(","), Charsets.UTF_8.name())
        val connection = URL(normalizeServerUrl(settings.serverUrl) + "/api/v1/calls/${urlSegment(callId)}/recording?tracks=$encodedTracks").openConnection() as HttpURLConnection
        try {
            connection.connectTimeout = 10_000
            connection.readTimeout = 60_000
            if (settings.apiKey.isNotBlank()) connection.setRequestProperty("Authorization", "Bearer ${settings.apiKey}")
            val code = connection.responseCode
            if (code != 200) {
                val text = connection.errorStream?.bufferedReader(Charsets.UTF_8)?.use { it.readText() }.orEmpty()
                val message = runCatching { JSONObject(text).optString("error") }.getOrNull().orEmpty()
                throw IOException(message.ifBlank { "The server returned HTTP $code" })
            }
            connection.inputStream.use { it.readBytes() }
        } finally {
            connection.disconnect()
        }
    }

    suspend fun voice(): VoiceProvisioning = request("/api/v1/voice").let { body ->
        VoiceProvisioning(
            enabled = body.optBoolean("enabled"),
            websocketUrl = body.optString("websocket_url"),
            domain = body.optString("domain"),
            username = body.optString("username"),
            password = body.optString("password"),
            diagnosticsEnabled = body.optBoolean("diagnostics_enabled"),
            iceServers = body.optJSONArray("ice_servers").mapObjects { server ->
                VoiceIceServer(
                    urls = server.optJSONArray("urls").mapStrings(),
                    username = server.optString("username"),
                    credential = server.optString("credential"),
                )
            },
        )
    }

    suspend fun sendMessage(to: String, text: String): Message = request(
        path = "/api/v1/messages",
        method = "POST",
        body = JSONObject().put("to", to).put("text", text),
        acceptedStatuses = setOf(200, 201, 202),
    ).let(::messageFromJson)

    suspend fun startCall(to: String): Call = request(
        path = "/api/v1/calls",
        method = "POST",
        body = JSONObject().put("to", to),
        acceptedStatuses = setOf(200, 201),
    ).let(::callFromJson)

    suspend fun normalizeCallDestination(number: String, country: String): String {
        val trimmed = number.trim()
        if (isCanonicalE164(trimmed)) return trimmed
        return try {
            request(
                path = "/api/v1/calls/normalize",
                method = "POST",
                body = JSONObject().put("number", trimmed).put("country", country),
            ).optString("number").takeIf(::isCanonicalE164)
                ?: throw IOException("The server returned an invalid dialing destination")
        } catch (error: IOException) {
            if (error.message.orEmpty().contains("HTTP 404") || error.message.orEmpty().contains("HTTP 405")) {
                throw IOException("This server cannot normalize this number yet. Enter a canonical international number beginning with +.", error)
            }
            throw error
        }
    }

    suspend fun claimRecordingParticipant(traceId: String, participantId: String, identity: ClientIdentity) {
        rawRequest(
            path = "/api/v1/calls/participants",
            method = "POST",
            body = JSONObject()
                .put("trace_id", traceId)
                .put("participant_id", participantId)
                .put("client_id", identity.id)
                .put("name", identity.name),
            acceptedStatuses = setOf(200),
        )
    }

    suspend fun recordDiagnostic(body: JSONObject) {
        rawRequest(
            path = "/api/v1/diagnostics/events",
            method = "POST",
            body = body,
            acceptedStatuses = setOf(202, 204),
        )
    }

    suspend fun pollChallenge(agentId: String): AuthChallenge? {
        val response = rawRequest(
            path = "/api/v1/auth/agents/${urlSegment(agentId)}/challenges",
            acceptedStatuses = setOf(200, 204),
            readTimeoutMs = 30_000,
        )
        if (response.code == 204) return null
        val body = JSONObject(response.body)
        return AuthChallenge(body.getString("id"), body.getString("rand"), body.getString("autn"))
    }

    suspend fun answerChallenge(agentId: String, challengeId: String, material: AuthMaterial) {
        val body = JSONObject()
        if (material.auts.isNotEmpty()) {
            body.put("auts", material.auts)
        } else {
            body.put("res", material.res).put("ck", material.ck).put("ik", material.ik)
        }
        rawRequest(
            path = "/api/v1/auth/agents/${urlSegment(agentId)}/challenges/${urlSegment(challengeId)}/response",
            method = "POST",
            body = body,
            acceptedStatuses = setOf(204),
        )
    }

    suspend fun answerChallengeError(agentId: String, challengeId: String) {
        rawRequest(
            path = "/api/v1/auth/agents/${urlSegment(agentId)}/challenges/${urlSegment(challengeId)}/response",
            method = "POST",
            body = JSONObject().put("error", "The SIM could not resolve the authentication challenge"),
            acceptedStatuses = setOf(204),
        )
    }

    private suspend fun request(
        path: String,
        method: String = "GET",
        body: JSONObject? = null,
        acceptedStatuses: Set<Int> = setOf(200),
    ): JSONObject {
        val response = rawRequest(path, method, body, acceptedStatuses)
        return if (response.body.isBlank()) JSONObject() else JSONObject(response.body)
    }

    private suspend fun rawRequest(
        path: String,
        method: String = "GET",
        body: JSONObject? = null,
        acceptedStatuses: Set<Int>,
        readTimeoutMs: Int = 15_000,
    ): HttpResult = withContext(Dispatchers.IO) {
        val connection = URL(normalizeServerUrl(settings.serverUrl) + path).openConnection() as HttpURLConnection
        try {
            connection.requestMethod = method
            connection.connectTimeout = 10_000
            connection.readTimeout = readTimeoutMs
            connection.setRequestProperty("Accept", "application/json")
            if (settings.apiKey.isNotBlank()) {
                connection.setRequestProperty("Authorization", "Bearer ${settings.apiKey}")
            }
            if (body != null) {
                connection.doOutput = true
                connection.setRequestProperty("Content-Type", "application/json")
                connection.outputStream.bufferedWriter(Charsets.UTF_8).use { it.write(body.toString()) }
            }
            val code = connection.responseCode
            val stream = if (code in acceptedStatuses) connection.inputStream else connection.errorStream
            val text = stream?.bufferedReader(Charsets.UTF_8)?.use { it.readText() }.orEmpty()
            if (code !in acceptedStatuses) {
                val message = runCatching { JSONObject(text).optString("error") }.getOrNull().orEmpty()
                throw IOException(message.ifBlank { "The server returned HTTP $code" })
            }
            HttpResult(code, text)
        } finally {
            connection.disconnect()
        }
    }

    private fun messageFromJson(body: JSONObject) = Message(
        id = body.optString("id"),
        direction = body.optString("direction"),
        from = body.optString("from"),
        to = body.optString("to"),
        text = body.optString("text"),
        state = body.optString("state"),
        deliveryState = body.optString("delivery_state"),
        deliveryReportRequested = body.optBoolean("delivery_report_requested"),
        statusDetail = body.optString("status_detail"),
        failureCode = body.optString("failure_code"),
        failureReason = body.optString("failure_reason"),
        parts = body.optJSONArray("parts").mapObjects { part ->
            SMSPartStatus(
                part = part.optInt("part"),
                parts = part.optInt("parts"),
                state = part.optString("state"),
                statusCode = part.optString("status_code"),
                failureCode = part.optString("failure_code"),
                failureReason = part.optString("failure_reason"),
                detail = part.optString("detail"),
            )
        },
        createdAt = body.optString("created_at"),
        contactId = body.optString("contact_id"),
        contactName = body.optString("contact_name"),
    )

    private fun callFromJson(body: JSONObject) = Call(
        id = body.optString("id"),
		traceId = body.optString("trace_id"),
        direction = body.optString("direction"),
        peer = body.optString("peer"),
        state = body.optString("state"),
        startedAt = body.optString("started_at"),
        endedAt = body.optString("ended_at"),
        recordingTracks = body.optJSONArray("recording_tracks").mapObjects { track ->
            RecordingTrack(
                id = track.optString("id"),
                role = track.optString("role"),
                participantId = track.optString("participant_id"),
                clientId = track.optString("client_id"),
                label = track.optString("label"),
                contentType = track.optString("content_type"),
                createdAt = track.optString("created_at"),
                offsetMs = track.optLong("offset_ms"),
                durationMs = track.optLong("duration_ms"),
            )
        },
        contactId = body.optString("contact_id"),
        contactName = body.optString("contact_name"),
    )

    private fun topologyNodeFromJson(body: JSONObject?, fallbackLabel: String = "Viti server") = TopologyNode(
        state = body?.optString("state").orEmpty().ifBlank { "idle" },
        label = body?.optString("label").orEmpty().ifBlank { fallbackLabel },
        detail = body?.optString("detail").orEmpty(),
        evidence = body?.optString("evidence").orEmpty(),
        observedAt = body?.optString("observed_at").orEmpty(),
        mcc = body?.optString("mcc").orEmpty(),
        mnc = body?.optString("mnc").orEmpty(),
    )

    private fun cellularFromJson(body: JSONObject?): CellularObservation? = body?.let {
        CellularObservation(
            generation = it.optString("generation"),
            generationStartedAt = it.optString("generation_started_at"),
            sequence = it.optLong("sequence"),
            measuredAt = it.optString("measured_at"),
            subscriptionRole = it.optString("subscription_role"),
            homeNetwork = cellularNetworkFromJson(it.optJSONObject("home_network")),
            servingNetwork = cellularNetworkFromJson(it.optJSONObject("serving_network")),
            service = it.optJSONObject("service").let { service ->
                CellularServiceDetails(
                    availability = service?.optString("availability").orEmpty().ifBlank { "unknown" },
                    registration = service?.optString("registration").orEmpty(),
                    roaming = service?.takeIf { value -> value.has("roaming") }?.optBoolean("roaming"),
                    measuredAt = service?.optString("measured_at").orEmpty(),
                )
            },
            dataAccess = it.optJSONObject("data_access").let { access ->
                CellularDataAccessDetails(
                    availability = access?.optString("availability").orEmpty().ifBlank { "unknown" },
                    technology = access?.optString("technology").orEmpty(),
                    measuredAt = access?.optString("measured_at").orEmpty(),
                )
            },
            signal = it.optJSONObject("signal").let { signal ->
                CellularSignalDetails(
                    availability = signal?.optString("availability").orEmpty().ifBlank { "unknown" },
                    level = signal?.takeIf { value -> value.has("level") }?.optInt("level"),
                    dbm = signal?.takeIf { value -> value.has("dbm") }?.optInt("dbm"),
                    measuredAt = signal?.optString("measured_at").orEmpty(),
                )
            },
            receivedAt = it.optString("received_at"),
            stale = it.optBoolean("stale"),
        )
    }

    private fun cellularNetworkFromJson(body: JSONObject?): CellularNetworkDetails = CellularNetworkDetails(
        availability = body?.optString("availability").orEmpty().ifBlank { "unknown" },
        operator = body?.optString("operator").orEmpty(),
        mcc = body?.optString("mcc").orEmpty(),
        mnc = body?.optString("mnc").orEmpty(),
        measuredAt = body?.optString("measured_at").orEmpty(),
    )

    private fun CellularObservation.toJson(): JSONObject = JSONObject()
        .put("generation", generation)
        .put("generation_started_at", generationStartedAt)
        .put("sequence", sequence)
        .put("measured_at", measuredAt)
        .put("subscription_role", subscriptionRole)
        .put("home_network", homeNetwork.toJson())
        .put("serving_network", servingNetwork.toJson())
        .put("service", service.toJson())
        .put("data_access", dataAccess.toJson())
        .put("signal", signal.toJson())

    private fun CellularNetworkDetails.toJson(): JSONObject = JSONObject()
        .put("availability", availability)
        .putIfNotBlank("operator", operator)
        .putIfNotBlank("mcc", mcc)
        .putIfNotBlank("mnc", mnc)
        .putIfNotBlank("measured_at", measuredAt)

    private fun CellularServiceDetails.toJson(): JSONObject = JSONObject()
        .put("availability", availability)
        .putIfNotBlank("registration", registration)
        .apply { roaming?.let { put("roaming", it) } }
        .putIfNotBlank("measured_at", measuredAt)

    private fun CellularDataAccessDetails.toJson(): JSONObject = JSONObject()
        .put("availability", availability)
        .putIfNotBlank("technology", technology)
        .putIfNotBlank("measured_at", measuredAt)

    private fun CellularSignalDetails.toJson(): JSONObject = JSONObject()
        .put("availability", availability)
        .apply {
            level?.let { put("level", it) }
            dbm?.let { put("dbm", it) }
        }
        .putIfNotBlank("measured_at", measuredAt)

    private fun JSONObject.putIfNotBlank(name: String, value: String): JSONObject = apply {
        if (value.isNotBlank()) put(name, value)
    }

    private fun <T> JSONArray?.mapObjects(transform: (JSONObject) -> T): List<T> {
        if (this == null) return emptyList()
        return List(length()) { transform(getJSONObject(it)) }
    }

    private fun JSONArray?.mapStrings(): List<String> {
        if (this == null) return emptyList()
        return List(length()) { getString(it) }
    }

    private fun urlSegment(value: String): String = java.net.URLEncoder.encode(value, Charsets.UTF_8.name())

    private data class HttpResult(val code: Int, val body: String)
}

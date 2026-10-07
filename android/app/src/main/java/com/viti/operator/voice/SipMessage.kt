package com.viti.operator.voice

import java.security.MessageDigest
import java.util.UUID

/** Small wire codec for the trusted Asterisk client leg, not the carrier SIP stack. */
internal data class SipMessage(val start: String, val headers: List<Pair<String, String>>, val body: String = "") {
    val status: Int get() = if (start.startsWith("SIP/2.0 ")) start.split(' ').getOrNull(1)?.toIntOrNull() ?: 0 else 0
    val method: String get() = if (status == 0) start.substringBefore(' ') else header("CSeq").substringAfter(' ', "").trim()
    val uri: String get() = start.split(' ').getOrElse(1) { "" }
    val callId: String get() = header("Call-ID")
    val cseq: Long get() = header("CSeq").substringBefore(' ').toLongOrNull() ?: -1
    val branch: String get() = parameter(header("Via"), "branch")
    fun header(name: String) = all(name).firstOrNull().orEmpty()
    fun all(name: String) = headers.filter { canonical(it.first) == canonical(name) }.map { it.second }
    fun encode(): String {
        require(!start.contains('\r') && !start.contains('\n'))
        val clean = headers.filterNot { canonical(it.first) == "content-length" }
        clean.forEach { (name, value) -> require(name.matches(Regex("[A-Za-z0-9.!%*_+`'~-]+")) && !value.contains('\r') && !value.contains('\n')) }
        return (listOf(start) + clean.map { "${it.first}: ${it.second}" } + "Content-Length: ${body.toByteArray(Charsets.UTF_8).size}" + "" + "").joinToString("\r\n") + body
    }
    fun replace(name: String, value: String) = copy(headers = headers.filterNot { canonical(it.first) == canonical(name) } + (name to value))

    companion object {
        private val compact = mapOf("v" to "via", "f" to "from", "t" to "to", "i" to "call-id", "m" to "contact", "l" to "content-length", "c" to "content-type")
        private fun canonical(name: String) = name.lowercase().let { compact[it] ?: it }
        fun parse(raw: String): SipMessage? {
            if (raw.length > 256 * 1024 || !raw.contains("\r\n\r\n")) return null
            val head = raw.substringBefore("\r\n\r\n").replace(Regex("\r\n[ \t]+"), " ").split("\r\n")
            val first = head.firstOrNull() ?: return null
            if (!first.matches(Regex("(?:SIP/2\\.0 [0-9]{3} .*|[A-Z]+ sips?:[^ ]+ SIP/2\\.0)"))) return null
            val headers = head.drop(1).map { line ->
                if (!line.contains(':')) return null
                line.substringBefore(':').trim() to line.substringAfter(':').trim()
            }
            val message = SipMessage(first, headers, raw.substringAfter("\r\n\r\n"))
            val lengths = message.all("Content-Length").map { it.toIntOrNull() ?: return null }
            if (lengths.distinct().size > 1 || lengths.any { it != message.body.toByteArray(Charsets.UTF_8).size }) return null
            if (message.callId.isBlank() || message.cseq < 0 || message.branch.isBlank()) return null
            return message
        }
        fun address(value: String): String = if ('<' in value) value.substringAfter('<').substringBefore('>') else value.substringBefore(';').trim()
        fun user(value: String) = address(value).substringAfter(':').substringBefore('@')
        fun parameter(value: String, name: String): String = Regex("(?:^|;)\\s*${Regex.escape(name)}=(?:\"([^\"]*)\"|([^;,\\s]+))", RegexOption.IGNORE_CASE)
            .find(value)?.let { it.groupValues[1].ifEmpty { it.groupValues[2] } }.orEmpty()
        fun tag(value: String) = parameter(value, "tag")
        fun withTag(value: String, tag: String) = if (tag(value).isNotBlank()) value else "$value;tag=$tag"
        fun token() = UUID.randomUUID().toString().replace("-", "")
        fun commaValues(value: String): List<String> {
            val result = mutableListOf<String>()
            var quoted = false; var angle = 0; var escaped = false; var begin = 0
            value.forEachIndexed { n, c ->
                if (escaped) { escaped = false; return@forEachIndexed }
                if (c == '\\' && quoted) { escaped = true; return@forEachIndexed }
                if (c == '"') quoted = !quoted
                if (!quoted) { if (c == '<') angle++; if (c == '>') angle-- }
                if (c == ',' && !quoted && angle == 0) { result.add(value.substring(begin, n).trim()); begin = n + 1 }
            }
            result.add(value.substring(begin).trim())
            return result
        }
        fun response(request: SipMessage, code: Int, toTag: String = "", body: String = "", extra: List<Pair<String, String>> = emptyList()): SipMessage {
            val reasons = mapOf(100 to "Trying", 180 to "Ringing", 200 to "OK", 400 to "Bad Request", 481 to "Call/Transaction Does Not Exist", 486 to "Busy Here", 487 to "Request Terminated", 488 to "Not Acceptable Here", 491 to "Request Pending", 501 to "Not Implemented", 603 to "Decline")
            return SipMessage("SIP/2.0 $code ${reasons[code] ?: "Response"}",
                request.all("Via").map { "Via" to it } + listOf("From" to request.header("From"),
                    "To" to if (toTag.isBlank()) request.header("To") else withTag(request.header("To"), toTag),
                    "Call-ID" to request.callId, "CSeq" to request.header("CSeq")) + extra +
                    if (body.isNotEmpty()) listOf("Content-Type" to "application/sdp") else emptyList(), body)
        }
    }
}

internal class SipDigest(private val cnonce: String = SipMessage.token()) {
    private val counts = mutableMapOf<String, Long>()
    fun authorization(challenge: String, username: String, password: String, method: String, uri: String): String {
        require(challenge.startsWith("Digest ", true)) { "Unsupported SIP authentication" }
        val values = Regex("([a-zA-Z_-]+)\\s*=\\s*(?:\"((?:\\\\.|[^\"])*)\"|([^, ]+))").findAll(challenge.substring(7))
            .associate { it.groupValues[1].lowercase() to it.groupValues[2].ifEmpty { it.groupValues[3] } }
        val realm = values["realm"] ?: error("Missing SIP realm")
        val nonce = values["nonce"] ?: error("Missing SIP nonce")
        val algorithm = values["algorithm"] ?: "MD5"
        val hashAlgorithm = when (algorithm.uppercase()) { "MD5", "MD5-SESS" -> "MD5"; "SHA-256", "SHA-256-SESS" -> "SHA-256"; else -> error("Unsupported SIP digest") }
        fun hash(value: String) = MessageDigest.getInstance(hashAlgorithm).digest(value.toByteArray(Charsets.UTF_8)).joinToString("") { "%02x".format(it) }
        fun quote(value: String) = "\"" + value.replace("\\", "\\\\").replace("\"", "\\\"") + "\""
        val qops = values["qop"]?.split(',')?.map { it.trim() }
        require(qops == null || "auth" in qops) { "Unsupported SIP qop" }
        val nc = ((counts[nonce] ?: 0) + 1).also { counts[nonce] = it }.toString(16).padStart(8, '0')
        var ha1 = hash("$username:$realm:$password")
        if (algorithm.endsWith("-sess", true)) ha1 = hash("$ha1:$nonce:$cnonce")
        val ha2 = hash("$method:$uri")
        val digest = if (qops == null) hash("$ha1:$nonce:$ha2") else hash("$ha1:$nonce:$nc:$cnonce:auth:$ha2")
        val fields = mutableListOf("username=${quote(username)}", "realm=${quote(realm)}", "nonce=${quote(nonce)}", "uri=${quote(uri)}", "response=${quote(digest)}", "algorithm=$algorithm")
        values["opaque"]?.let { fields.add("opaque=${quote(it)}") }
        if (qops != null) fields.addAll(listOf("qop=auth", "nc=$nc", "cnonce=${quote(cnonce)}"))
        else if (algorithm.endsWith("-sess", true)) fields.add("cnonce=${quote(cnonce)}")
        return "Digest " + fields.joinToString(", ")
    }
}

package com.viti.operator.data

internal data class HistoryCheckpoint(
    val cursor: String = "",
    val messages: Map<String, String> = emptyMap(),
    val calls: Map<String, String> = emptyMap(),
    val contactsVersion: Long = 0,
)

internal data class HistoryPatch(
    val mode: String,
    val baseCursor: String,
    val cursor: String,
    val messages: Map<String, String>,
    val calls: Map<String, String>,
    val deletedMessages: Set<String> = emptySet(),
    val deletedCalls: Set<String> = emptySet(),
    val contactsVersion: Long = 0,
)

/** Immutable replacement prevents a cursor from advancing without its rows. */
internal fun applyHistoryPatch(current: HistoryCheckpoint, patch: HistoryPatch): HistoryCheckpoint {
    fun revision(cursor: String): Pair<String, Long> {
        val parts = cursor.split(':')
        require(parts.size == 2 && parts[0].startsWith("history-") && parts[0].length > 8)
        val value = parts[1].toLong()
        require(value >= 0 && value.toString() == parts[1])
        return parts[0] to value
    }
    val next = revision(patch.cursor)
    require(patch.messages.keys.none(String::isBlank) && patch.calls.keys.none(String::isBlank))
    require(patch.messages.keys.intersect(patch.deletedMessages).isEmpty())
    require(patch.calls.keys.intersect(patch.deletedCalls).isEmpty())
    if (patch.mode == "full") {
        require(patch.deletedMessages.isEmpty() && patch.deletedCalls.isEmpty())
        return HistoryCheckpoint(patch.cursor, patch.messages.toMap(), patch.calls.toMap(), patch.contactsVersion)
    }
    require(patch.mode == "delta")
    // An explicit check can race with a push already applied by this client.
    if (patch.cursor == current.cursor) return current
    require(current.cursor.isNotBlank() && patch.baseCursor == current.cursor)
    val before = revision(current.cursor)
    require(next.first == before.first && next.second > before.second)
    return HistoryCheckpoint(patch.cursor,
        (current.messages - patch.deletedMessages) + patch.messages,
        (current.calls - patch.deletedCalls) + patch.calls,
        patch.contactsVersion)
}

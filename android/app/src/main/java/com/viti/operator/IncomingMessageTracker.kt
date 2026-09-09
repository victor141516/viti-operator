package com.viti.operator

import com.viti.operator.data.Message

internal class IncomingMessageTracker {
    private var sourceKey: String? = null
    private var baselineReady = false
    private var knownIncomingIds = emptySet<String>()

    fun update(source: String, messages: List<Message>): List<Message> {
        if (source != sourceKey) {
            sourceKey = source
            baselineReady = false
            knownIncomingIds = emptySet()
        }
        val incoming = messages.filter { it.direction == "incoming" }
        val unseen = if (baselineReady) {
            incoming.filter { it.id !in knownIncomingIds }.sortedBy { it.createdAt }
        } else {
            emptyList()
        }
        knownIncomingIds = incoming.mapTo(mutableSetOf()) { it.id }
        baselineReady = true
        return unseen
    }
}

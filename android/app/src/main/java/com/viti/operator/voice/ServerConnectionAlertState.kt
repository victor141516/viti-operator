package com.viti.operator.voice

import java.util.UUID

/** Silence applies to one continuous outage, including changes in its reason. */
internal class ServerConnectionAlertState {
    var outageToken: String? = null
        private set
    var silenced: Boolean = false
        private set

    val visible: Boolean get() = outageToken != null
    val shouldRing: Boolean get() = visible && !silenced

    fun show() {
        if (!visible) outageToken = UUID.randomUUID().toString()
    }

    fun silence(token: String?): Boolean {
        if (token == null || token != outageToken || silenced) return false
        silenced = true
        return true
    }

    fun dismiss() {
        outageToken = null
        silenced = false
    }
}

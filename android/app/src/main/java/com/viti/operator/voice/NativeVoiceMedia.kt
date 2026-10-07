package com.viti.operator.voice

import com.viti.operator.data.VoiceCallQuality

internal interface NativeVoiceMedia : AutoCloseable {
    suspend fun offer(): String
    suspend fun answer(offer: String): String
    suspend fun acceptAnswer(answer: String, provisional: Boolean = false)
    fun setAudioState(microphoneMuted: Boolean, inactive: Boolean)
    fun dtmf(tone: Char)
}

internal data class NativeMediaEvent(val state: String = "", val quality: VoiceCallQuality? = null, val sent: Boolean = false, val received: Boolean = false)

package com.viti.operator

import android.app.NotificationManager
import android.content.Context
import android.media.AudioAttributes
import android.media.AudioManager
import android.media.Ringtone
import android.media.RingtoneManager
import android.os.Build
import android.os.Handler
import android.os.Looper

/**
 * Plays the user's configured ringtone for as long as a Viti call is waiting.
 *
 * Notification channels normally own alert audio. A call is different from a
 * one-shot notification, though: its ringtone must continue until somebody
 * answers or rejects it. Keeping that lifecycle here also avoids relying on
 * OEM-specific handling of Notification.FLAG_INSISTENT.
 */
internal class IncomingCallRinger(
    context: Context,
    private val channelId: String,
) {
    private val applicationContext = context.applicationContext
    private val notificationManager = applicationContext.getSystemService(NotificationManager::class.java)
    private val audioManager = applicationContext.getSystemService(AudioManager::class.java)
    private val handler = Handler(Looper.getMainLooper())
    private var ringtone: Ringtone? = null

    private val replayForLegacyAndroid = object : Runnable {
        override fun run() {
            val current = ringtone ?: return
            if (!current.isPlaying) current.play()
            handler.postDelayed(this, LEGACY_REPLAY_INTERVAL_MS)
        }
    }

    fun start() {
        if (ringtone?.isPlaying == true || !soundIsAllowed()) return
        stop()
        val uri = RingtoneManager.getDefaultUri(RingtoneManager.TYPE_RINGTONE)
            ?: RingtoneManager.getDefaultUri(RingtoneManager.TYPE_NOTIFICATION)
            ?: return
        val next = RingtoneManager.getRingtone(applicationContext, uri) ?: return
        next.audioAttributes = AudioAttributes.Builder()
            .setUsage(AudioAttributes.USAGE_NOTIFICATION_RINGTONE)
            .setContentType(AudioAttributes.CONTENT_TYPE_SONIFICATION)
            .build()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
            next.isLooping = true
        }
        ringtone = next
        next.play()
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.P) {
            handler.postDelayed(replayForLegacyAndroid, LEGACY_REPLAY_INTERVAL_MS)
        }
    }

    fun stop() {
        handler.removeCallbacks(replayForLegacyAndroid)
        ringtone?.stop()
        ringtone = null
    }

    private fun soundIsAllowed(): Boolean {
        if (audioManager.ringerMode != AudioManager.RINGER_MODE_NORMAL) return false
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return true
        val channel = notificationManager.getNotificationChannel(channelId) ?: return true
        return channel.importance != NotificationManager.IMPORTANCE_NONE && channel.sound != null
    }

    private companion object {
        const val LEGACY_REPLAY_INTERVAL_MS = 4_000L
    }
}

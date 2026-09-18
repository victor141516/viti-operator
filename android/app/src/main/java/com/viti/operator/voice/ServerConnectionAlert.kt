package com.viti.operator.voice

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.media.AudioAttributes
import android.media.RingtoneManager
import android.os.Build
import androidx.core.app.NotificationCompat
import com.viti.operator.IncomingCallRinger
import com.viti.operator.MainActivity
import com.viti.operator.R
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

/** Owns the local, non-call alert shown while this device cannot receive calls. */
internal class ServerConnectionAlert(
    context: Context,
    private val scope: CoroutineScope,
) {
    private val applicationContext = context.applicationContext
    private val notificationManager = applicationContext.getSystemService(NotificationManager::class.java)
    private val ringer = IncomingCallRinger(applicationContext, CHANNEL_ID)
    private var stopRingingJob: Job? = null
    private var hasRungForCurrentAlert = false

    var visible: Boolean = false
        private set

    init {
        createChannel()
    }

    fun show(ring: Boolean) {
        if (!visible) {
            visible = true
            notificationManager.notify(NOTIFICATION_ID, notification())
        }
        if (ring) startRinging()
    }

    fun startRinging() {
        if (!visible || hasRungForCurrentAlert) return
        hasRungForCurrentAlert = true
        ringer.start()
        stopRingingJob = scope.launch {
            delay(MAX_RING_DURATION_MS)
            ringer.stop()
        }
    }

    fun dismiss() {
        visible = false
        hasRungForCurrentAlert = false
        stopRingingJob?.cancel()
        stopRingingJob = null
        ringer.stop()
        notificationManager.cancel(NOTIFICATION_ID)
    }

    private fun notification(): Notification {
        val openApp = PendingIntent.getActivity(
            applicationContext,
            OPEN_REQUEST,
            Intent(applicationContext, MainActivity::class.java)
                .addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP or Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val detail = "Viti cannot reach the server. Calls may not arrive on this device."
        return NotificationCompat.Builder(applicationContext, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_launcher_foreground)
            .setContentTitle("No connection")
            .setContentText(detail)
            .setStyle(NotificationCompat.BigTextStyle().bigText(detail))
            .setCategory(NotificationCompat.CATEGORY_ERROR)
            .setPriority(NotificationCompat.PRIORITY_MAX)
            .setVisibility(NotificationCompat.VISIBILITY_PUBLIC)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            // Audio is owned by IncomingCallRinger so it can be stopped after
            // a bounded interval without removing the persistent warning.
            .setSilent(true)
            .setContentIntent(openApp)
            .build()
    }

    private fun createChannel() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return
        val sound = RingtoneManager.getDefaultUri(RingtoneManager.TYPE_RINGTONE)
        val attributes = AudioAttributes.Builder()
            .setUsage(AudioAttributes.USAGE_NOTIFICATION_RINGTONE)
            .setContentType(AudioAttributes.CONTENT_TYPE_SONIFICATION)
            .build()
        notificationManager.createNotificationChannel(
            NotificationChannel(CHANNEL_ID, "Connection alerts", NotificationManager.IMPORTANCE_HIGH).apply {
                description = "Alerts when this device cannot receive Viti calls"
                setSound(sound, attributes)
                enableVibration(true)
                lockscreenVisibility = Notification.VISIBILITY_PUBLIC
            },
        )
    }

    private companion object {
        const val CHANNEL_ID = "server_connection_alerts_v1"
        const val NOTIFICATION_ID = 4411
        const val OPEN_REQUEST = 4412
        const val MAX_RING_DURATION_MS = 30_000L
    }
}

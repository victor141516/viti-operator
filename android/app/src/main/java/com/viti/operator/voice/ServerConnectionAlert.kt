package com.viti.operator.voice

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.media.AudioAttributes
import android.media.RingtoneManager
import android.net.Uri
import android.os.Build
import androidx.core.app.NotificationCompat
import com.viti.operator.IncomingCallRinger
import com.viti.operator.MainActivity
import com.viti.operator.R

/** Owns the local, non-call alert shown while this device cannot receive calls. */
internal class ServerConnectionAlert(
    context: Context,
) {
    private val applicationContext = context.applicationContext
    private val notificationManager = applicationContext.getSystemService(NotificationManager::class.java)
    private val ringer = IncomingCallRinger(applicationContext, CHANNEL_ID)
    private val state = ServerConnectionAlertState()
    private var availability: CallAvailability? = null
    private var silenceAction: PendingIntent? = null

    val visible: Boolean get() = state.visible

    init {
        createChannel()
    }

    fun show(reason: CallAvailability, ring: Boolean) {
        if (!visible || availability != reason) {
            state.show()
            availability = reason
            notificationManager.notify(NOTIFICATION_ID, notification(reason))
        }
        if (ring) startRinging() else pauseRinging()
    }

    fun startRinging() {
        if (!state.shouldRing) return
        ringer.start()
    }

    fun pauseRinging() = ringer.stop()

    fun silence(outageToken: String?) {
        if (!state.silence(outageToken)) return
        ringer.stop()
        availability?.let { notificationManager.notify(NOTIFICATION_ID, notification(it)) }
    }

    fun dismiss() {
        state.dismiss()
        availability = null
        silenceAction?.cancel()
        silenceAction = null
        ringer.stop()
        notificationManager.cancel(NOTIFICATION_ID)
    }

    private fun notification(reason: CallAvailability): Notification {
        val openApp = PendingIntent.getActivity(
            applicationContext,
            OPEN_REQUEST,
            Intent(applicationContext, MainActivity::class.java)
                .addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP or Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val detail = if (state.silenced) {
            "${reason.detail} Sound silenced until connection recovers."
        } else {
            reason.detail
        }
        val builder = NotificationCompat.Builder(applicationContext, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_launcher_foreground)
            .setContentTitle(reason.title)
            .setContentText(detail)
            .setStyle(NotificationCompat.BigTextStyle().bigText(detail))
            .setCategory(NotificationCompat.CATEGORY_ERROR)
            .setPriority(NotificationCompat.PRIORITY_MAX)
            .setVisibility(NotificationCompat.VISIBILITY_PUBLIC)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            // The app-owned ringtone stops on recovery or Silence. A real call
            // pauses its audio without clearing the persistent outage warning.
            .setSilent(true)
            .setContentIntent(openApp)
        if (!state.silenced) {
            val token = checkNotNull(state.outageToken)
            val action = silenceAction ?: PendingIntent.getService(
                applicationContext,
                SILENCE_REQUEST,
                Intent(applicationContext, VoiceCallService::class.java)
                    .setAction(VoiceCallService.ACTION_SILENCE_CONNECTION_ALERT)
                    .setData(Uri.parse("viti://connection-alert/$token"))
                    .putExtra(VoiceCallService.EXTRA_CONNECTION_ALERT_TOKEN, token),
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
            ).also { silenceAction = it }
            builder.addAction(android.R.drawable.ic_lock_silent_mode, "Silence", action)
        }
        return builder.build()
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
        const val SILENCE_REQUEST = 4413
    }
}

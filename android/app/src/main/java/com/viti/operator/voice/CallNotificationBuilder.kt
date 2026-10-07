package com.viti.operator.voice

import android.content.Context
import androidx.core.app.NotificationCompat

/** Soundless channel and silent notification are not equivalent: the latter
 * suppresses heads-up and full-screen presentation, even on a high channel. */
internal fun callNotificationBuilder(context: Context, incoming: Boolean): NotificationCompat.Builder =
    NotificationCompat.Builder(context, VoiceCallService.CALL_CHANNEL_ID)
        .setSilent(!incoming)
        .setOnlyAlertOnce(true)
        .setForegroundServiceBehavior(NotificationCompat.FOREGROUND_SERVICE_IMMEDIATE)
        .setSound(null)
        .apply { if (incoming) setVibrate(longArrayOf(0, 150)) }

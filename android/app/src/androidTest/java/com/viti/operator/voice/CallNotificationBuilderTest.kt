package com.viti.operator.voice

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Intent
import androidx.core.app.NotificationCompat
import androidx.core.app.Person
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import com.viti.operator.R
import com.viti.operator.MainActivity
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class CallNotificationBuilderTest {
    @Test
    fun incomingCallIsNotSilentlyGroupedAndRemainsEligibleForHeadsUp() {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        val notification = callNotificationBuilder(context, incoming = true)
            .setSmallIcon(R.drawable.ic_launcher_foreground).build()
        assertNull(notification.group)
        assertEquals(Notification.GROUP_ALERT_ALL, notification.groupAlertBehavior)
        assertNull(notification.sound)
    }

    @Test
    fun ongoingCallRemainsQuietWithoutPeekingAgain() {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        val notification = callNotificationBuilder(context, incoming = false)
            .setSmallIcon(R.drawable.ic_launcher_foreground).build()
        assertEquals(Notification.GROUP_ALERT_SUMMARY, notification.groupAlertBehavior)
    }

    @Test
    fun showLocalHeadsUpFixtureWhenExplicitlyRequested() {
        if (InstrumentationRegistry.getArguments().getString("capture_heads_up") != "true") return
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        val manager = context.getSystemService(NotificationManager::class.java)
        manager.createNotificationChannel(NotificationChannel(VoiceCallService.CALL_CHANNEL_ID,
            "Incoming call alerts", NotificationManager.IMPORTANCE_HIGH).apply {
            setSound(null, null)
            enableVibration(true)
        })
        val action = PendingIntent.getActivity(context, 4499, Intent(context, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        val notification = callNotificationBuilder(context, incoming = true)
            .setSmallIcon(R.drawable.ic_launcher_foreground)
            .setCategory(NotificationCompat.CATEGORY_CALL)
            .setPriority(NotificationCompat.PRIORITY_MAX)
            .setContentTitle("Incoming service call")
            .setStyle(NotificationCompat.CallStyle.forIncomingCall(
                Person.Builder().setName("Local notification test").build(), action, action))
            .setFullScreenIntent(action, true)
            .build()
        try {
            manager.notify(4499, notification)
            Thread.sleep(8_000)
        } finally {
            manager.cancel(4499)
            action.cancel()
        }
    }
}

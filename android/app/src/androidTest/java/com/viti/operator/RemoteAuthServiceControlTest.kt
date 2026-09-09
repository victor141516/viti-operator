package com.viti.operator

import android.content.Intent
import androidx.core.content.ContextCompat
import androidx.test.core.app.ApplicationProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import com.viti.operator.auth.RemoteAuthService
import com.viti.operator.data.vitiClientIdentity
import com.viti.operator.data.SettingsStore
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.delay
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

/**
 * Device-side control used by the end-to-end remote-SIM test. Running through
 * instrumentation starts the foreground service as the application UID; it
 * does not add an exported receiver or a debug backdoor to the production APK.
 */
@RunWith(AndroidJUnit4::class)
class RemoteAuthServiceControlTest {
    @Test
    fun startRemoteAuthenticationAgent() = runBlocking {
        val context = ApplicationProvider.getApplicationContext<android.content.Context>()
        val settings = SettingsStore(context).settings.first()
        val identity = context.vitiClientIdentity()
        assertTrue("The authentication client id must not be blank", identity.id.isNotBlank())
        InstrumentationRegistry.getInstrumentation().sendStatus(
            1,
            android.os.Bundle().apply {
                putString("viti_server", settings.serverUrl)
                putString("viti_agent", identity.id)
            },
        )
        ContextCompat.startForegroundService(
            context,
            Intent(context, RemoteAuthService::class.java).setAction(RemoteAuthService.ACTION_CLAIM),
        )
        // Instrumentation tears down the target process when the test ends on
        // some OEM builds. Holding it for the requested window lets the host
        // perform a real ePDG challenge while the production service runs.
        val holdMs = InstrumentationRegistry.getArguments()
            .getString("hold_ms")
            ?.toLongOrNull()
            ?.coerceIn(0, 300_000)
            ?: 0
        if (holdMs > 0) delay(holdMs)
    }
}

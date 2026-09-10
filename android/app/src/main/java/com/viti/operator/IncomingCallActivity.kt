package com.viti.operator

import android.Manifest
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.ServiceConnection
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.os.IBinder
import android.view.WindowManager
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.systemBarsPadding
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Call
import androidx.compose.material.icons.filled.CallEnd
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.lifecycle.lifecycleScope
import com.viti.operator.ui.VitiTheme
import com.viti.operator.voice.VoiceCallService
import com.viti.operator.voice.VoiceCallSnapshot
import com.viti.operator.voice.shouldShowIncomingCall
import kotlinx.coroutines.Job
import kotlinx.coroutines.launch

/**
 * A volatile lock-screen projection of the incoming leg owned by
 * [VoiceCallService]. It never creates or restores a call on its own.
 */
class IncomingCallActivity : ComponentActivity() {
    private var expectedToken by mutableStateOf("")
    private var fallbackPeer by mutableStateOf("")
    private var snapshot by mutableStateOf(VoiceCallSnapshot())
    private var service: VoiceCallService? = null
    private var bindingRequested = false
    private var snapshotJob: Job? = null
    private var actionSubmitted by mutableStateOf(false)
    private var notice by mutableStateOf("")
    private var permissionRequestToken: String? = null
    private var autoAnswerToken: String? = null

    private val microphonePermission = registerForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
        val token = permissionRequestToken
        permissionRequestToken = null
        if (granted && token != null) {
            answerIfCurrent(token)
        } else if (!granted) {
            notice = "Microphone access is required to answer. You can still decline the call."
        }
    }

    private val connection = object : ServiceConnection {
        override fun onServiceConnected(name: ComponentName, binder: IBinder) {
            val connected = (binder as VoiceCallService.LocalBinder).service
            service = connected
            snapshot = connected.snapshot.value
            if (!shouldShowIncomingCall(snapshot, expectedToken)) {
                closePresentation()
                return
            }
            snapshotJob?.cancel()
            snapshotJob = lifecycleScope.launch {
                connected.snapshot.collect { current ->
                    snapshot = current
                    if (!shouldShowIncomingCall(current, expectedToken)) closePresentation()
                }
            }
            consumeAutoAnswer()
        }

        override fun onServiceDisconnected(name: ComponentName) {
            snapshotJob?.cancel()
            snapshotJob = null
            service = null
            closePresentation()
        }

        override fun onBindingDied(name: ComponentName) {
            snapshotJob?.cancel()
            snapshotJob = null
            service = null
            closePresentation()
        }

        override fun onNullBinding(name: ComponentName) {
            service = null
            closePresentation()
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        configureLockScreenPresentation()
        enableEdgeToEdge()
        acceptIntent(intent)
        if (expectedToken.isBlank()) {
            closePresentation()
            return
        }
        setContent {
            VitiTheme {
                IncomingCallScreen(
                    peer = snapshot.displayName.ifBlank { snapshot.peer }.ifBlank { fallbackPeer }.ifBlank { "Private number" },
                    detail = snapshot.voice.detail.ifBlank { "Incoming service call" },
                    busy = actionSubmitted,
                    notice = notice,
                    onAnswer = ::answer,
                    onDecline = ::decline,
                )
            }
        }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        acceptIntent(intent)
        service?.let { connected ->
            snapshot = connected.snapshot.value
            if (!shouldShowIncomingCall(snapshot, expectedToken)) closePresentation() else consumeAutoAnswer()
        }
    }

    override fun onStart() {
        super.onStart()
        if (expectedToken.isBlank() || bindingRequested) return
        bindingRequested = bindService(Intent(this, VoiceCallService::class.java), connection, 0)
        if (!bindingRequested) {
            closePresentation()
        }
    }

    override fun onStop() {
        snapshotJob?.cancel()
        snapshotJob = null
        if (bindingRequested) {
            runCatching { unbindService(connection) }
            bindingRequested = false
        }
        service = null
        super.onStop()
    }

    private fun acceptIntent(intent: Intent) {
        val next = intent.getStringExtra(VoiceCallService.EXTRA_LEG_TOKEN).orEmpty()
        if (next != expectedToken) {
            expectedToken = next
            fallbackPeer = intent.getStringExtra(VoiceCallService.EXTRA_CALLER_NAME).orEmpty()
            actionSubmitted = false
            notice = ""
            permissionRequestToken = null
            autoAnswerToken = null
        }
        if (intent.action == ACTION_ANSWER_CALL && permissionRequestToken != next && !actionSubmitted) {
            autoAnswerToken = next
            intent.action = null
        }
    }

    private fun answer() {
        val token = expectedToken
        if (!isCurrent(token) || actionSubmitted || permissionRequestToken != null) return
        if (ContextCompat.checkSelfPermission(this, Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED) {
            answerIfCurrent(token)
        } else {
            permissionRequestToken = token
            microphonePermission.launch(Manifest.permission.RECORD_AUDIO)
        }
    }

    private fun answerIfCurrent(token: String) {
        val connected = service ?: return closePresentation()
        val current = connected.snapshot.value
        if (token != expectedToken || !shouldShowIncomingCall(current, token)) {
            closePresentation()
            return
        }
        actionSubmitted = true
        notice = "Answering…"
        connected.runtime.answer(token)
    }

    private fun consumeAutoAnswer() {
        val token = autoAnswerToken ?: return
        autoAnswerToken = null
        if (token == expectedToken) answer()
    }

    private fun decline() {
        val token = expectedToken
        val connected = service ?: return closePresentation()
        if (!isCurrent(token) || actionSubmitted) return
        actionSubmitted = true
        notice = "Declining…"
        connected.runtime.reject(token)
    }

    private fun isCurrent(token: String): Boolean =
        token.isNotBlank() && token == expectedToken && service?.snapshot?.value?.let { shouldShowIncomingCall(it, token) } == true

    private fun configureLockScreenPresentation() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O_MR1) {
            setShowWhenLocked(true)
            setTurnScreenOn(true)
        } else {
            @Suppress("DEPRECATION")
            window.addFlags(
                WindowManager.LayoutParams.FLAG_SHOW_WHEN_LOCKED or
                    WindowManager.LayoutParams.FLAG_TURN_SCREEN_ON,
            )
        }
    }

    private fun closePresentation() {
        if (!isFinishing && !isDestroyed) finishAndRemoveTask()
    }

    companion object {
        const val ACTION_ANSWER_CALL = "com.viti.operator.incoming.action.ANSWER"
    }
}

@Composable
internal fun IncomingCallScreen(
    peer: String,
    detail: String,
    busy: Boolean,
    notice: String,
    onAnswer: () -> Unit,
    onDecline: () -> Unit,
) {
    Box(
        modifier = Modifier
            .fillMaxSize()
            .background(MaterialTheme.colorScheme.surface)
            .systemBarsPadding()
            .padding(horizontal = 28.dp, vertical = 36.dp),
    ) {
        Column(
            modifier = Modifier.fillMaxSize(),
            horizontalAlignment = Alignment.CenterHorizontally,
            verticalArrangement = Arrangement.SpaceBetween,
        ) {
            Column(horizontalAlignment = Alignment.CenterHorizontally) {
                Text(
                    text = "Incoming Viti call",
                    style = MaterialTheme.typography.titleMedium,
                    color = MaterialTheme.colorScheme.primary,
                    fontWeight = FontWeight.SemiBold,
                )
                Spacer(Modifier.size(40.dp))
                Surface(
                    modifier = Modifier.size(112.dp),
                    shape = CircleShape,
                    color = MaterialTheme.colorScheme.primaryContainer,
                    contentColor = MaterialTheme.colorScheme.onPrimaryContainer,
                ) {
                    Box(contentAlignment = Alignment.Center) {
                        Text(
                            text = peer.trim().firstOrNull()?.uppercase() ?: "?",
                            style = MaterialTheme.typography.displayMedium,
                            fontWeight = FontWeight.Medium,
                        )
                    }
                }
                Spacer(Modifier.size(24.dp))
                Text(
                    text = peer,
                    style = MaterialTheme.typography.headlineMedium,
                    fontWeight = FontWeight.SemiBold,
                    textAlign = TextAlign.Center,
                    maxLines = 2,
                    overflow = TextOverflow.Ellipsis,
                )
                Spacer(Modifier.size(8.dp))
                Text(
                    text = detail,
                    style = MaterialTheme.typography.bodyLarge,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    textAlign = TextAlign.Center,
                    maxLines = 3,
                    overflow = TextOverflow.Ellipsis,
                )
                if (notice.isNotBlank()) {
                    Spacer(Modifier.size(18.dp))
                    Text(
                        text = notice,
                        style = MaterialTheme.typography.bodyMedium,
                        color = if (busy) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.error,
                        textAlign = TextAlign.Center,
                    )
                }
            }
            Row(
                horizontalArrangement = Arrangement.spacedBy(64.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                CallActionButton(
                    label = "Decline",
                    containerColor = MaterialTheme.colorScheme.error,
                    contentColor = MaterialTheme.colorScheme.onError,
                    enabled = !busy,
                    onClick = onDecline,
                ) { Icon(Icons.Default.CallEnd, contentDescription = null, modifier = Modifier.size(30.dp)) }
                CallActionButton(
                    label = "Answer",
                    containerColor = Color(0xFF247A45),
                    contentColor = Color.White,
                    enabled = !busy,
                    onClick = onAnswer,
                ) { Icon(Icons.Default.Call, contentDescription = null, modifier = Modifier.size(30.dp)) }
            }
        }
    }
}

@Composable
private fun CallActionButton(
    label: String,
    containerColor: Color,
    contentColor: Color,
    enabled: Boolean,
    onClick: () -> Unit,
    icon: @Composable () -> Unit,
) {
    Column(horizontalAlignment = Alignment.CenterHorizontally) {
        Surface(
            onClick = onClick,
            enabled = enabled,
            modifier = Modifier.size(72.dp),
            shape = CircleShape,
            color = containerColor,
            contentColor = contentColor,
        ) {
            Box(contentAlignment = Alignment.Center) { icon() }
        }
        Spacer(Modifier.size(10.dp))
        Text(label, style = MaterialTheme.typography.labelLarge)
    }
}

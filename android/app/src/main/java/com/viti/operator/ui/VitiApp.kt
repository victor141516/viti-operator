package com.viti.operator.ui

import android.Manifest
import android.graphics.Color as AndroidColor
import android.media.MediaPlayer
import android.view.MotionEvent
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient as AndroidWebViewClient
import android.content.pm.PackageManager
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawing
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.clickable
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Message
import androidx.compose.material.icons.automirrored.filled.Send
import androidx.compose.material.icons.filled.Call
import androidx.compose.material.icons.filled.CallEnd
import androidx.compose.material.icons.automirrored.filled.CallReceived
import androidx.compose.material.icons.filled.CellTower
import androidx.compose.material.icons.filled.CheckCircle
import androidx.compose.material.icons.filled.CloudOff
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Dialpad
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.Key
import androidx.compose.material.icons.filled.Person
import androidx.compose.material.icons.filled.PhoneAndroid
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material.icons.filled.Pause
import androidx.compose.material.icons.filled.Download
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material.icons.filled.MicOff
import androidx.compose.material.icons.filled.NotificationsActive
import androidx.compose.material.icons.filled.Fullscreen
import androidx.compose.material.icons.filled.ContactPhone
import androidx.compose.material.icons.filled.Search
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material.icons.filled.SwapHoriz
import androidx.compose.material.icons.filled.WarningAmber
import androidx.compose.material.icons.automirrored.filled.VolumeUp
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.Checkbox
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.CenterAlignedTopAppBar
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ElevatedCard
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ExtendedFloatingActionButton
import androidx.compose.material3.FloatingActionButton
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SegmentedButton
import androidx.compose.material3.SegmentedButtonDefaults
import androidx.compose.material3.SingleChoiceSegmentedButtonRow
import androidx.compose.material3.Slider
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.material3.adaptive.navigationsuite.NavigationSuiteScaffold
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.core.content.ContextCompat
import androidx.compose.ui.platform.LocalConfiguration
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.compose.ui.viewinterop.AndroidView
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import com.viti.operator.AppUiState
import com.viti.operator.MainViewModel
import com.viti.operator.data.Call
import com.viti.operator.data.ConnectionTopology
import com.viti.operator.data.Message
import com.viti.operator.data.normalizeServerUrl
import com.viti.operator.data.TopologyClient
import com.viti.operator.data.RuntimeStatus
import com.viti.operator.data.CellularTelemetryMonitor
import com.viti.operator.data.ServerSettings
import com.viti.operator.data.PhoneContact
import com.viti.operator.data.VitiApi
import com.viti.operator.data.VoiceCallQuality
import com.viti.operator.voice.VoiceCallRuntime
import com.viti.operator.voice.IncomingCallPresentationStatus
import com.viti.operator.voice.VoiceCallSnapshot
import com.viti.operator.voice.VoiceWebViewClient
import org.json.JSONObject
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.io.File
import java.time.Duration
import java.time.Instant
import java.time.ZoneId
import java.time.format.DateTimeFormatter

private enum class Destination(val label: String, val icon: ImageVector) {
    Messages("Messages", Icons.AutoMirrored.Filled.Message),
    Calls("Calls", Icons.Default.Call),
    Connection("Connection", Icons.Default.Settings),
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun VitiApp(
    viewModel: MainViewModel,
    voiceRuntime: VoiceCallRuntime,
    voiceClient: VoiceWebViewClient,
    onPlaceCall: (String) -> Unit,
    onAnswerCall: () -> Unit,
    onRejectCall: () -> Unit,
    onHangupCall: () -> Unit,
    onSetMuted: (Boolean) -> Unit,
    onSendDtmf: (Char) -> Unit,
    onSelectAudioEndpoint: (String) -> Unit,
    onPrepareShizuku: () -> Unit,
    onStartAuthAgent: () -> Unit,
    onStopAuthAgent: () -> Unit,
	onEnableServerLine: () -> Unit,
	onReturnLineToPhone: () -> Unit,
    onRequestContacts: () -> Unit,
    callPresentationStatus: IncomingCallPresentationStatus,
    onRequestCallNotifications: () -> Unit,
    onOpenFullScreenCallSettings: () -> Unit,
    onOpenCallChannelSettings: () -> Unit,
) {
    val ui by viewModel.uiState.collectAsStateWithLifecycle()
    val settings by viewModel.settings.collectAsStateWithLifecycle()
    val authService by viewModel.authService.collectAsStateWithLifecycle()
	val phoneRouting by viewModel.phoneRouting.collectAsStateWithLifecycle()
    val callSnapshot by voiceRuntime.snapshot.collectAsStateWithLifecycle()
    var destination by rememberSaveable { mutableStateOf(Destination.Messages) }

    Box(Modifier.fillMaxSize()) {
      NavigationSuiteScaffold(
        navigationSuiteItems = {
            Destination.entries.forEach { item ->
                item(
                    selected = destination == item,
                    onClick = { destination = item },
                    icon = { Icon(item.icon, contentDescription = item.label) },
                    label = { Text(item.label) },
                )
            }
        },
    ) {
        Scaffold(
            contentWindowInsets = WindowInsets.safeDrawing,
            topBar = {
                CenterAlignedTopAppBar(
                    title = {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Surface(
                                shape = RoundedCornerShape(12.dp),
                                color = MaterialTheme.colorScheme.primary,
                                contentColor = MaterialTheme.colorScheme.onPrimary,
                            ) {
                                Text("V", modifier = Modifier.padding(horizontal = 11.dp, vertical = 6.dp), fontWeight = FontWeight.Black)
                            }
                            Spacer(Modifier.width(10.dp))
                            Text("Viti Operator", fontWeight = FontWeight.SemiBold)
                        }
                    },
                    actions = {
                        ConnectionPill(ui.status)
                        Spacer(Modifier.width(8.dp))
                    },
                )
            },
        ) { padding ->
            Column(Modifier.fillMaxSize().padding(padding)) {
                if (ui.notice.isNotBlank()) {
                    Surface(color = MaterialTheme.colorScheme.secondaryContainer) {
                        Text(
                            ui.notice,
                            modifier = Modifier.fillMaxWidth().padding(horizontal = 20.dp, vertical = 10.dp),
                            color = MaterialTheme.colorScheme.onSecondaryContainer,
                            style = MaterialTheme.typography.bodyMedium,
                        )
                    }
                }
                when (destination) {
                    Destination.Messages -> MessagesScreen(ui, viewModel::sendMessage, onRequestContacts)
                    Destination.Calls -> CallsScreen(
                        ui = ui,
                        settings = settings,
                        callSnapshot = callSnapshot,
                        onCall = onPlaceCall,
                        onAnswer = onAnswerCall,
                        onReject = onRejectCall,
                        onHangup = onHangupCall,
                        onSetMuted = onSetMuted,
                        onSendDtmf = onSendDtmf,
                        onSelectAudioEndpoint = onSelectAudioEndpoint,
                        onRequestContacts = onRequestContacts,
                    )
                    Destination.Connection -> ConnectionScreen(
                        ui = ui,
                        settings = settings,
                        authRunning = authService.running,
                        authDetail = authService.detail,
                        onSave = viewModel::saveSettings,
                        onPrepareShizuku = onPrepareShizuku,
                        onStartAuthAgent = onStartAuthAgent,
                        onStopAuthAgent = onStopAuthAgent,
						phoneRouting = phoneRouting,
						onEnableServerLine = onEnableServerLine,
						onReturnLineToPhone = onReturnLineToPhone,
                        onSetRecordingEnabled = viewModel::setRecordingEnabled,
                        callPresentationStatus = callPresentationStatus,
                        onRequestCallNotifications = onRequestCallNotifications,
                        onOpenFullScreenCallSettings = onOpenFullScreenCallSettings,
                        onOpenCallChannelSettings = onOpenCallChannelSettings,
                    )
                }
            }
        }
      }
      AndroidView(
          factory = { voiceClient.view },
          modifier = Modifier.size(1.dp).alpha(0f).align(Alignment.TopStart),
      )
    }
}

@Composable
private fun ConnectionPill(status: RuntimeStatus) {
    val authenticationUnavailable = status.imsReady && !status.simReady
    val background = when {
        status.ready -> MaterialTheme.colorScheme.tertiaryContainer
        authenticationUnavailable -> MaterialTheme.colorScheme.secondaryContainer
        else -> MaterialTheme.colorScheme.errorContainer
    }
    val foreground = when {
        status.ready -> MaterialTheme.colorScheme.onTertiaryContainer
        authenticationUnavailable -> MaterialTheme.colorScheme.onSecondaryContainer
        else -> MaterialTheme.colorScheme.onErrorContainer
    }
    Surface(shape = CircleShape, color = background, contentColor = foreground) {
        Row(
            modifier = Modifier.padding(horizontal = 10.dp, vertical = 6.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Icon(
                when {
                    status.ready -> Icons.Default.CheckCircle
                    authenticationUnavailable -> Icons.Default.WarningAmber
                    else -> Icons.Default.CloudOff
                },
                contentDescription = null,
                modifier = Modifier.size(16.dp),
            )
            Spacer(Modifier.width(5.dp))
            Text(
                when {
                    status.ready -> "IMS"
                    authenticationUnavailable -> "IMS · no SIM auth"
                    else -> "No network"
                },
                style = MaterialTheme.typography.labelMedium,
            )
        }
    }
}

private data class MessageConversation(val peer: String, val latest: Message)

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun MessagesScreen(
    ui: AppUiState,
    onSend: (String, String) -> Unit,
    onRequestContacts: () -> Unit,
) {
    var query by rememberSaveable { mutableStateOf("") }
    var composerOpen by rememberSaveable { mutableStateOf(false) }
    var composerRecipient by rememberSaveable { mutableStateOf("") }
    var threadPeer by rememberSaveable { mutableStateOf<String?>(null) }
    val conversations = remember(ui.messages, ui.contacts, query) {
        ui.messages
            .groupBy(::messagePeer)
            .mapNotNull { (peer, messages) ->
                messages.maxByOrNull { it.createdAt }?.let { MessageConversation(peer, it) }
            }
            .sortedByDescending { it.latest.createdAt }
            .filter { conversation ->
                val name = contactName(ui.contacts, conversation.peer)
                query.isBlank() || name.contains(query, true) || conversation.peer.contains(query) || conversation.latest.text.contains(query, true)
            }
    }

    Box(Modifier.fillMaxSize()) {
        LazyColumn(
            modifier = Modifier.fillMaxSize().align(Alignment.TopCenter).widthIn(max = 760.dp),
            contentPadding = PaddingValues(start = 20.dp, top = 18.dp, end = 20.dp, bottom = 108.dp),
        ) {
            item {
                Text("Messages", style = MaterialTheme.typography.headlineLarge, fontWeight = FontWeight.Bold)
                Spacer(Modifier.height(16.dp))
                AppSearchField(query, "Search conversations", { query = it })
                Spacer(Modifier.height(14.dp))
            }
            if (conversations.isEmpty()) {
                item {
                    EmptyListState(
                        icon = Icons.AutoMirrored.Filled.Message,
                        title = if (query.isBlank()) "No messages yet" else "No matches",
                        detail = if (query.isBlank()) "Sent and received SMS will appear here." else "Try a different name, number, or message.",
                    )
                }
            } else {
                itemsIndexed(conversations, key = { _, item -> item.peer }) { index, conversation ->
                    MessageConversationRow(conversation, ui.contacts) {
                        threadPeer = conversation.peer
                    }
                    if (index != conversations.lastIndex) HorizontalDivider(Modifier.padding(start = 64.dp))
                }
            }
        }
        ExtendedFloatingActionButton(
            onClick = { composerRecipient = ""; composerOpen = true },
            modifier = Modifier.align(Alignment.BottomEnd).padding(20.dp),
            icon = { Icon(Icons.Default.Edit, null) },
            text = { Text("New message") },
        )
    }
    if (composerOpen) {
        MessageComposerSheet(
            initialRecipient = composerRecipient,
            contacts = ui.contacts,
            contactsAvailable = ui.contactsAvailable,
            busy = ui.busy,
            lineReady = ui.status.ready,
            onRequestContacts = onRequestContacts,
            onDismiss = { composerOpen = false },
            onSend = { to, body -> onSend(to, body); composerOpen = false },
        )
    }
    threadPeer?.let { peer ->
        MessageThreadSheet(
            peer = peer,
            displayName = contactName(ui.contacts, peer),
            messages = ui.messages.filter { messagePeer(it) == peer },
            busy = ui.busy,
            lineReady = ui.status.ready,
            onDismiss = { threadPeer = null },
            onSend = { body -> onSend(peer, body) },
        )
    }
}

@Composable
private fun AppSearchField(value: String, placeholder: String, onValueChange: (String) -> Unit) {
    OutlinedTextField(
        value = value,
        onValueChange = onValueChange,
        modifier = Modifier.fillMaxWidth(),
        placeholder = { Text(placeholder) },
        leadingIcon = { Icon(Icons.Default.Search, null) },
        trailingIcon = {
            if (value.isNotBlank()) IconButton(onClick = { onValueChange("") }) {
                Icon(Icons.Default.Close, "Clear search")
            }
        },
        shape = RoundedCornerShape(28.dp),
        singleLine = true,
    )
}

@Composable
private fun MessageConversationRow(
    conversation: MessageConversation,
    contacts: List<PhoneContact>,
    onClick: () -> Unit,
) {
    val message = conversation.latest
    val name = message.contactName.ifBlank { contactName(contacts, conversation.peer) }
    Row(
        Modifier.fillMaxWidth().clickable(onClick = onClick).padding(vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        PersonAvatar(name)
        Spacer(Modifier.width(14.dp))
        Column(Modifier.weight(1f)) {
            Text(name, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.SemiBold, maxLines = 1, overflow = TextOverflow.Ellipsis)
            if (name != conversation.peer) Text(conversation.peer, style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            Spacer(Modifier.height(2.dp))
            Text(
                (if (message.direction == "outgoing") "You: " else "") + message.text,
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
        }
        Spacer(Modifier.width(12.dp))
        Column(horizontalAlignment = Alignment.End) {
            Text(formatActivityTime(message.createdAt), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            Spacer(Modifier.height(5.dp))
            Text(
                messageStatusLabel(message),
                style = MaterialTheme.typography.labelSmall,
                color = if (message.state == "failed") MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.primary,
            )
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun MessageComposerSheet(
    initialRecipient: String,
    contacts: List<PhoneContact>,
    contactsAvailable: Boolean,
    busy: Boolean,
    lineReady: Boolean,
    onRequestContacts: () -> Unit,
    onDismiss: () -> Unit,
    onSend: (String, String) -> Unit,
) {
    var to by rememberSaveable(initialRecipient) { mutableStateOf(initialRecipient) }
    var body by rememberSaveable { mutableStateOf("") }
    val suggestions = remember(contacts, to) {
        contacts.filter {
            to.isNotBlank() && (it.name.contains(to, true) || it.alternativeNames.any { name -> name.contains(to, true) } || it.number.contains(to))
        }.take(3)
    }
    ModalBottomSheet(onDismissRequest = onDismiss) {
        Column(
            Modifier.fillMaxWidth().widthIn(max = 680.dp).align(Alignment.CenterHorizontally).imePadding().padding(horizontal = 22.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            Text("New message", style = MaterialTheme.typography.headlineSmall, fontWeight = FontWeight.Bold)
            Text("Sent through your Viti IMS line.", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            OutlinedTextField(
                value = to,
                onValueChange = { to = it },
                modifier = Modifier.fillMaxWidth(),
                label = { Text("Recipient") },
                placeholder = { Text("+34…") },
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Phone),
                singleLine = true,
            )
            if (!contactsAvailable) {
                Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
                    Text(
                        "Syncing uploads contact names and phone numbers to your private Viti server so every connected client can resolve them.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    FilledTonalButton(onClick = onRequestContacts) {
                        Icon(Icons.Default.ContactPhone, null)
                        Spacer(Modifier.width(8.dp))
                        Text("Sync this phone's contacts")
                    }
                }
            }
            suggestions.forEach { contact ->
                ContactSuggestion(contact) { to = contact.number }
            }
            OutlinedTextField(
                value = body,
                onValueChange = { if (it.length <= 1000) body = it },
                modifier = Modifier.fillMaxWidth().height(132.dp),
                label = { Text("Message") },
                supportingText = { Text("${body.length}/1000") },
            )
            Button(
                onClick = { onSend(to, body) },
                enabled = lineReady && !busy && to.startsWith("+") && body.isNotBlank(),
                modifier = Modifier.fillMaxWidth().height(52.dp),
            ) {
                if (busy) CircularProgressIndicator(Modifier.size(20.dp), strokeWidth = 2.dp)
                else Icon(Icons.AutoMirrored.Filled.Send, null)
                Spacer(Modifier.width(8.dp))
                Text(if (busy) "Sending" else "Send SMS")
            }
            Spacer(Modifier.height(18.dp))
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun MessageThreadSheet(
    peer: String,
    displayName: String,
    messages: List<Message>,
    busy: Boolean,
    lineReady: Boolean,
    onDismiss: () -> Unit,
    onSend: (String) -> Unit,
) {
    var body by rememberSaveable(peer) { mutableStateOf("") }
    val sheetHeight = LocalConfiguration.current.screenHeightDp.dp * .88f
    val sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true)
    ModalBottomSheet(onDismissRequest = onDismiss, sheetState = sheetState) {
        Column(
            Modifier.fillMaxWidth().height(sheetHeight).widthIn(max = 680.dp).align(Alignment.CenterHorizontally).imePadding().padding(horizontal = 18.dp),
        ) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                PersonAvatar(displayName, 44.dp)
                Spacer(Modifier.width(12.dp))
                Column(Modifier.weight(1f)) {
                    Text(displayName, style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.Bold, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    Text(peer, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
            Spacer(Modifier.height(12.dp))
            HorizontalDivider()
            LazyColumn(
                modifier = Modifier.weight(1f).fillMaxWidth(),
                contentPadding = PaddingValues(vertical = 14.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                items(messages.sortedBy { it.createdAt }, key = { it.id }) { message ->
                    MessageBubble(message)
                }
            }
            Row(
                Modifier.fillMaxWidth().padding(bottom = 14.dp),
                verticalAlignment = Alignment.Bottom,
                horizontalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                OutlinedTextField(
                    value = body,
                    onValueChange = { if (it.length <= 1000) body = it },
                    modifier = Modifier.weight(1f),
                    placeholder = { Text("Message") },
                    shape = RoundedCornerShape(24.dp),
                    maxLines = 4,
                )
                IconButton(
                    onClick = { onSend(body); body = "" },
                    enabled = lineReady && !busy && body.isNotBlank(),
                    modifier = Modifier.size(52.dp),
                ) { Icon(Icons.AutoMirrored.Filled.Send, "Send message") }
            }
        }
    }
}

@Composable
private fun MessageBubble(message: Message) {
    val outgoing = message.direction == "outgoing"
    Row(Modifier.fillMaxWidth(), horizontalArrangement = if (outgoing) Arrangement.End else Arrangement.Start) {
        Surface(
            shape = RoundedCornerShape(18.dp),
            color = if (outgoing) MaterialTheme.colorScheme.primaryContainer else MaterialTheme.colorScheme.surfaceContainerHigh,
            modifier = Modifier.fillMaxWidth(.82f),
        ) {
            Column(Modifier.padding(horizontal = 14.dp, vertical = 10.dp)) {
                Text(message.text, style = MaterialTheme.typography.bodyLarge)
                Spacer(Modifier.height(4.dp))
                Text(
                    listOf(formatActivityTime(message.createdAt), messageStatusLabel(message)).filter(String::isNotBlank).joinToString(" · "),
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.align(Alignment.End),
                )
                val detail = messageStatusDetail(message)
                if (outgoing && detail.isNotBlank()) {
                    Spacer(Modifier.height(4.dp))
                    Text(
                        detail,
                        style = MaterialTheme.typography.labelSmall,
                        color = if (message.state == "failed") MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
        }
    }
}

@Composable
private fun ContactSuggestion(contact: PhoneContact, onClick: () -> Unit) {
    Row(
        Modifier.fillMaxWidth().clickable(onClick = onClick).padding(vertical = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        PersonAvatar(contact.name, 40.dp)
        Spacer(Modifier.width(12.dp))
        Column {
            Text(contact.name, fontWeight = FontWeight.SemiBold)
            Text(contact.number, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            if (contact.sourceNames.isNotEmpty()) Text(
                "From ${contact.sourceNames.joinToString()}",
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun CallsScreen(
    ui: AppUiState,
    settings: ServerSettings,
    callSnapshot: VoiceCallSnapshot,
    onCall: (String) -> Unit,
    onAnswer: () -> Unit,
    onReject: () -> Unit,
    onHangup: () -> Unit,
    onSetMuted: (Boolean) -> Unit,
    onSendDtmf: (Char) -> Unit,
    onSelectAudioEndpoint: (String) -> Unit,
    onRequestContacts: () -> Unit,
) {
    var query by rememberSaveable { mutableStateOf("") }
    var selectedTab by rememberSaveable { mutableStateOf(0) }
    var dialerOpen by rememberSaveable { mutableStateOf(false) }
    var dialerNumber by rememberSaveable { mutableStateOf("") }
    var recordingCall by remember { mutableStateOf<Call?>(null) }
    val activeServerCall = ui.calls.firstOrNull { it.endedAt.isBlank() && it.state in setOf("ringing", "dialing", "active") }
    val serverCallActive = activeServerCall != null
    val anotherClientOwnsCall = serverCallActive && !ui.voice.incoming && !ui.voice.inCall && !ui.voice.ending
    val canJoinCall = anotherClientOwnsCall && activeServerCall?.state == "active" && activeServerCall.peer != "*43"
    val filteredCalls = remember(ui.calls, ui.contacts, query) {
        ui.calls.filter {
            val name = contactName(ui.contacts, it.peer)
            query.isBlank() || name.contains(query, true) || it.peer.contains(query)
        }
    }
    val filteredContacts = remember(ui.contacts, query) {
        ui.contacts.filter {
            query.isBlank() || it.name.contains(query, true) || it.alternativeNames.any { name -> name.contains(query, true) } || it.number.contains(query)
        }
    }
    val canStartCall = ui.status.ready && ui.voice.registered && !serverCallActive

    Box(Modifier.fillMaxSize()) {
        LazyColumn(
            modifier = Modifier.fillMaxSize().align(Alignment.TopCenter).widthIn(max = 760.dp),
            contentPadding = PaddingValues(start = 20.dp, top = 18.dp, end = 20.dp, bottom = 108.dp),
        ) {
            item {
                Text("Calls", style = MaterialTheme.typography.headlineLarge, fontWeight = FontWeight.Bold)
                Spacer(Modifier.height(16.dp))
                AppSearchField(query, "Search contacts or numbers", { query = it })
                Spacer(Modifier.height(12.dp))
                SingleChoiceSegmentedButtonRow(Modifier.fillMaxWidth()) {
                    listOf("Recent", "Contacts").forEachIndexed { index, label ->
                        SegmentedButton(
                            selected = selectedTab == index,
                            onClick = { selectedTab = index },
                            shape = SegmentedButtonDefaults.itemShape(index, 2),
                        ) { Text(label) }
                    }
                }
                if (ui.voice.incoming || ui.voice.inCall || ui.voice.ending || anotherClientOwnsCall) {
                    Spacer(Modifier.height(14.dp))
                    ActiveCallPanel(
                        ui = ui,
                        callSnapshot = callSnapshot,
                        activeServerCall = activeServerCall,
                        anotherClientOwnsCall = anotherClientOwnsCall,
                        canJoinCall = canJoinCall,
                        onAnswer = onAnswer,
                        onReject = onReject,
                        onHangup = onHangup,
                        onSetMuted = onSetMuted,
                        onJoin = { onCall("*44") },
                        onOpenKeypad = { dialerOpen = true },
                        onSelectAudioEndpoint = onSelectAudioEndpoint,
                    )
                }
                Spacer(Modifier.height(10.dp))
            }
            if (selectedTab == 0) {
                if (filteredCalls.isEmpty()) {
                    item {
                        EmptyListState(
                            Icons.Default.Call,
                            if (query.isBlank()) "No calls yet" else "No matches",
                            if (query.isBlank()) "Your call activity will appear here." else "Try a different name or number.",
                        )
                    }
                } else {
                    itemsIndexed(filteredCalls, key = { _, item -> item.id }) { index, call ->
                        CallActivityRow(call, ui.contacts, canStartCall, { onCall(call.peer) }) { recordingCall = call }
                        if (index != filteredCalls.lastIndex) HorizontalDivider(Modifier.padding(start = 64.dp))
                    }
                }
            } else {
                if (!ui.contactsAvailable) {
                    item {
                        ElevatedCard(Modifier.fillMaxWidth()) {
                            Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
                                Text("Sync this phone's contacts", style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.SemiBold)
                                Text(
                                    "Names and phone numbers are uploaded to your private Viti server and shared with your connected clients.",
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                                FilledTonalButton(onClick = onRequestContacts, modifier = Modifier.fillMaxWidth()) {
                                    Icon(Icons.Default.ContactPhone, null)
                                    Spacer(Modifier.width(8.dp))
                                    Text("Sync contacts")
                                }
                            }
                        }
                    }
                }
                if (filteredContacts.isEmpty()) {
                    item { EmptyListState(Icons.Default.Person, "No contacts", if (query.isBlank()) "Synchronize contacts from a phone to see them here." else "Try a different name or number.") }
                } else {
                    itemsIndexed(filteredContacts, key = { _, item -> "${item.id}:${item.number}" }) { index, contact ->
                        ContactCallRow(contact, canStartCall) { onCall(contact.number) }
                        if (index != filteredContacts.lastIndex) HorizontalDivider(Modifier.padding(start = 64.dp))
                    }
                }
            }
        }
        if (!ui.voice.ending) {
            FloatingActionButton(
                onClick = { dialerOpen = true },
                modifier = Modifier.align(Alignment.BottomEnd).padding(20.dp),
            ) { Icon(Icons.Default.Dialpad, "Open dial pad") }
        }
    }
    if (dialerOpen) {
        DialerSheet(
            initialNumber = dialerNumber,
            ui = ui,
            callSnapshot = callSnapshot,
            serverCallActive = serverCallActive,
            anotherClientOwnsCall = anotherClientOwnsCall,
            onDismiss = { dialerOpen = false },
            onNumberChange = { dialerNumber = it },
            onCall = { dialerNumber = it; onCall(it); dialerOpen = false },
            onAnswer = onAnswer,
            onReject = onReject,
            onHangup = onHangup,
            onSetMuted = onSetMuted,
            onSendDtmf = onSendDtmf,
            onSelectAudioEndpoint = onSelectAudioEndpoint,
        )
    }
    recordingCall?.let { call ->
        RecordingSheet(call = call, settings = settings, onDismiss = { recordingCall = null })
    }
}

@Composable
private fun ActiveCallPanel(
    ui: AppUiState,
    callSnapshot: VoiceCallSnapshot,
    activeServerCall: Call?,
    anotherClientOwnsCall: Boolean,
    canJoinCall: Boolean,
    onAnswer: () -> Unit,
    onReject: () -> Unit,
    onHangup: () -> Unit,
    onSetMuted: (Boolean) -> Unit,
    onJoin: () -> Unit,
    onOpenKeypad: () -> Unit,
    onSelectAudioEndpoint: (String) -> Unit,
) {
    val peerNumber = activeServerCall?.peer?.takeUnless { it == "*44" }.orEmpty().ifBlank { ui.voice.peer }.ifBlank { "Call" }
    val peer = activeServerCall?.contactName.orEmpty().ifBlank { contactName(ui.contacts, peerNumber) }
    ElevatedCard(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                PersonAvatar(peer, 44.dp)
                Spacer(Modifier.width(12.dp))
                Column(Modifier.weight(1f)) {
                    Text(peer, fontWeight = FontWeight.SemiBold)
                    if (peer != peerNumber) Text(peerNumber, style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    Text(
                        if (anotherClientOwnsCall) {
                            when {
                                canJoinCall -> "Call in progress · you can join"
                                activeServerCall?.state == "active" -> "Call in progress on another device"
                                else -> "The call is connecting on another device"
                            }
                        } else ui.voice.detail,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                if (ui.voice.inCall) IconButton(onClick = onOpenKeypad) { Icon(Icons.Default.Dialpad, "Dial pad") }
            }
            when {
                ui.voice.ending -> Button(
                    onClick = if (callSnapshot.endingRejected) onReject else onHangup,
                    modifier = Modifier.fillMaxWidth().height(52.dp),
                ) {
                    Icon(Icons.Default.CallEnd, null)
                    Spacer(Modifier.width(6.dp))
                    Text(if (callSnapshot.endingRejected) "Retry reject" else "Retry hang up")
                }
                ui.voice.incoming -> Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                    Button(onClick = onAnswer, modifier = Modifier.weight(1f)) { Icon(Icons.AutoMirrored.Filled.CallReceived, null); Spacer(Modifier.width(6.dp)); Text("Answer") }
                    OutlinedButton(onClick = onReject, modifier = Modifier.weight(1f)) { Icon(Icons.Default.CallEnd, null); Spacer(Modifier.width(6.dp)); Text("Reject") }
                }
                ui.voice.inCall -> Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                    FilledTonalButton(
                        onClick = { onSetMuted(!ui.voice.microphoneMuted) },
                        modifier = Modifier.weight(1f).height(52.dp),
                    ) {
                        Icon(if (ui.voice.microphoneMuted) Icons.Default.Mic else Icons.Default.MicOff, null)
                        Spacer(Modifier.width(6.dp))
                        Text(if (ui.voice.microphoneMuted) "Unmute" else "Mute")
                    }
                    Button(onClick = onHangup, modifier = Modifier.weight(1f).height(52.dp)) {
                        Icon(Icons.Default.CallEnd, null)
                        Spacer(Modifier.width(6.dp))
                        Text("Hang up")
                    }
                }
                canJoinCall -> Button(onClick = onJoin, modifier = Modifier.fillMaxWidth()) {
                    Icon(Icons.AutoMirrored.Filled.CallReceived, null)
                    Spacer(Modifier.width(6.dp))
                    Text("Join")
                }
            }
            if (ui.voice.inCall) {
                AudioRouteButton(callSnapshot, onSelectAudioEndpoint)
                when {
                    callSnapshot.phase == com.viti.operator.voice.VoiceCallPhase.INACTIVE -> Text(
                        "Android has paused this call's audio for another call.",
                        style = MaterialTheme.typography.labelMedium,
                        color = MaterialTheme.colorScheme.tertiary,
                    )
                    callSnapshot.audio.telecomMuted -> Text(
                        "The microphone is muted by Android or a connected accessory.",
                        style = MaterialTheme.typography.labelMedium,
                        color = MaterialTheme.colorScheme.tertiary,
                    )
                }
            }
        }
    }
}

@Composable
private fun CallActivityRow(call: Call, contacts: List<PhoneContact>, enabled: Boolean, onCall: () -> Unit, onRecording: () -> Unit) {
    val failed = call.state == "failed"
    val direction = if (call.direction == "incoming") "Incoming" else "Outgoing"
    val detail = buildString {
        append(if (failed) "Missed call" else direction)
        formatCallDuration(call)?.let { append(" · "); append(it) }
    }
    val rowModifier = if (call.recordingTracks.isNotEmpty()) Modifier.clickable(onClick = onRecording) else Modifier
    val name = call.contactName.ifBlank { contactName(contacts, call.peer) }
    Row(rowModifier.fillMaxWidth().padding(vertical = 11.dp), verticalAlignment = Alignment.CenterVertically) {
        PersonAvatar(name)
        Spacer(Modifier.width(14.dp))
        Column(Modifier.weight(1f)) {
            Text(name, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.SemiBold, maxLines = 1, overflow = TextOverflow.Ellipsis)
            if (name != call.peer) Text(call.peer, style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            Text(detail, style = MaterialTheme.typography.bodyMedium, color = if (failed) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant)
            if (call.recordingTracks.isNotEmpty()) {
                Text("Recording available", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.primary)
            }
        }
        Column(horizontalAlignment = Alignment.End) {
            Text(formatActivityTime(call.startedAt), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            IconButton(onClick = onCall, enabled = enabled) { Icon(Icons.Default.Call, "Call ${call.peer}") }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun RecordingSheet(call: Call, settings: ServerSettings, onDismiss: () -> Unit) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var selected by remember(call.id) { mutableStateOf(call.recordingTracks.map { it.id }.toSet()) }
    var audioBytes by remember(call.id) { mutableStateOf<ByteArray?>(null) }
    var preparing by remember(call.id) { mutableStateOf(false) }
    var error by remember(call.id) { mutableStateOf("") }
    var playing by remember(call.id) { mutableStateOf(false) }
    var positionMs by remember(call.id) { mutableStateOf(0) }
    var durationMs by remember(call.id) { mutableStateOf(0) }
    var seeking by remember(call.id) { mutableStateOf(false) }
    var playbackSpeed by remember(call.id) { mutableStateOf(1f) }
    val sheetScroll = rememberScrollState()
    val player = remember(call.id) { MediaPlayer() }
    val cacheFile = remember(call.id) { File(context.cacheDir, "viti-recording-${call.id}.wav") }
    val saveRecording = rememberLauncherForActivityResult(ActivityResultContracts.CreateDocument("audio/wav")) { uri ->
        val bytes = audioBytes ?: return@rememberLauncherForActivityResult
        if (uri != null) scope.launch {
            runCatching { withContext(Dispatchers.IO) { context.contentResolver.openOutputStream(uri)?.use { it.write(bytes) } ?: error("Could not open the selected file") } }
                .onFailure { error = it.message ?: "Could not save the recording" }
        }
    }

    LaunchedEffect(call.id, selected) {
        player.reset()
        playing = false
        positionMs = 0
        durationMs = 0
        seeking = false
        audioBytes = null
        if (selected.isEmpty()) return@LaunchedEffect
        preparing = true
        error = ""
        runCatching {
            val bytes = VitiApi(settings).recordingAudio(call.id, selected.sorted())
            withContext(Dispatchers.IO) { cacheFile.writeBytes(bytes) }
            player.setDataSource(cacheFile.absolutePath)
            player.prepare()
            durationMs = player.duration.coerceAtLeast(0)
            bytes
        }.onSuccess { audioBytes = it }
            .onFailure { error = it.message ?: "Could not prepare the recording" }
        preparing = false
    }
    DisposableEffect(player) {
        player.setOnCompletionListener {
            positionMs = durationMs
            playing = false
        }
        onDispose {
            player.release()
            cacheFile.delete()
        }
    }
    LaunchedEffect(playing, seeking) {
        while (playing) {
            if (!seeking && player.isPlaying) positionMs = player.currentPosition.coerceIn(0, durationMs)
            delay(100)
        }
    }

    ModalBottomSheet(onDismissRequest = onDismiss) {
        Column(
            Modifier.fillMaxWidth().widthIn(max = 560.dp).align(Alignment.CenterHorizontally).verticalScroll(sheetScroll).padding(horizontal = 24.dp),
            verticalArrangement = Arrangement.spacedBy(14.dp),
        ) {
            Text("Call recording", style = MaterialTheme.typography.headlineSmall, fontWeight = FontWeight.Bold)
            Text(
                "${if (call.direction == "incoming") "Incoming from" else "Outgoing to"} ${call.peer} · ${formatActivityTime(call.startedAt)}",
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Text("Tracks", style = MaterialTheme.typography.labelLarge)
            call.recordingTracks.forEach { track ->
                val presence = recordingTrackPresence(track, positionMs.toLong())
                val presenceText = when (presence.phase) {
                    RecordingPresencePhase.BEFORE -> "Joins at ${formatPlaybackTime(presence.boundaryMs.coerceAtMost(Int.MAX_VALUE.toLong()).toInt())}"
                    RecordingPresencePhase.ACTIVE -> "Present now"
                    RecordingPresencePhase.AFTER -> "Left at ${formatPlaybackTime(presence.boundaryMs.coerceAtMost(Int.MAX_VALUE.toLong()).toInt())}"
                }
                Surface(shape = RoundedCornerShape(16.dp), color = MaterialTheme.colorScheme.surfaceContainer) {
                    Row(
                        Modifier.fillMaxWidth().alpha(if (presence.active) 1f else 0.5f).clickable {
                            selected = if (track.id in selected) selected - track.id else selected + track.id
                        }.padding(12.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        Checkbox(
                            checked = track.id in selected,
                            onCheckedChange = { checked -> selected = if (checked) selected + track.id else selected - track.id },
                        )
                        Icon(if (track.role == "remote") Icons.Default.Person else Icons.Default.Mic, null)
                        Spacer(Modifier.width(10.dp))
                        Column(Modifier.weight(1f)) {
                            Text(track.label, fontWeight = FontWeight.SemiBold, maxLines = 1, overflow = TextOverflow.Ellipsis)
                            Text(
                                if (track.role == "remote") "Other side of the call" else "Microphone from this device",
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                            )
                            Text(
                                presenceText,
                                style = MaterialTheme.typography.labelSmall,
                                color = if (presence.active) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant,
                                fontWeight = FontWeight.SemiBold,
                            )
                        }
                    }
                }
            }
            if (preparing) Row(verticalAlignment = Alignment.CenterVertically) {
                CircularProgressIndicator(Modifier.size(22.dp), strokeWidth = 2.dp)
                Spacer(Modifier.width(10.dp)); Text("Preparing selected tracks…")
            }
            if (error.isNotBlank()) Text(error, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
            Surface(shape = RoundedCornerShape(18.dp), color = MaterialTheme.colorScheme.surfaceContainer) {
                Column(Modifier.fillMaxWidth().padding(14.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(10.dp), verticalAlignment = Alignment.CenterVertically) {
                        FilledTonalButton(
                            onClick = {
                                if (playing) {
                                    player.pause()
                                    positionMs = player.currentPosition.coerceIn(0, durationMs)
                                    playing = false
                                } else {
                                    if (durationMs > 0 && positionMs >= durationMs) {
                                        player.seekTo(0)
                                        positionMs = 0
                                    }
                                    player.start()
                                    player.playbackParams = player.playbackParams.setSpeed(playbackSpeed)
                                    playing = true
                                }
                            },
                            enabled = audioBytes != null && !preparing,
                            modifier = Modifier.weight(1f).height(48.dp),
                        ) {
                            Icon(if (playing) Icons.Default.Pause else Icons.Default.PlayArrow, null)
                            Spacer(Modifier.width(8.dp)); Text(if (playing) "Pause" else "Play")
                        }
                        OutlinedButton(
                            onClick = {
                                playbackSpeed = when (playbackSpeed) {
                                    1f -> 1.5f
                                    1.5f -> 2f
                                    else -> 1f
                                }
                                if (playing) player.playbackParams = player.playbackParams.setSpeed(playbackSpeed)
                            },
                            enabled = audioBytes != null && !preparing,
                            modifier = Modifier.height(48.dp),
                        ) {
                            Text("${playbackSpeed.toString().removeSuffix(".0")}×")
                        }
                    }
                    Slider(
                        value = positionMs.toFloat(),
                        onValueChange = {
                            seeking = true
                            positionMs = it.toInt().coerceIn(0, durationMs)
                        },
                        onValueChangeFinished = {
                            player.seekTo(positionMs)
                            seeking = false
                        },
                        valueRange = 0f..durationMs.coerceAtLeast(1).toFloat(),
                        enabled = audioBytes != null && !preparing && durationMs > 0,
                        modifier = Modifier.fillMaxWidth(),
                    )
                    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                        Text(formatPlaybackTime(positionMs), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                        Text(formatPlaybackTime(durationMs), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
            }
            OutlinedButton(
                onClick = { saveRecording.launch("viti-call-${call.id}.wav") },
                enabled = audioBytes != null && !preparing,
                modifier = Modifier.fillMaxWidth().height(48.dp),
            ) {
                Icon(Icons.Default.Download, null)
                Spacer(Modifier.width(8.dp)); Text("Save selected WAV")
            }
            Spacer(Modifier.height(24.dp))
        }
    }
}

private fun formatPlaybackTime(milliseconds: Int): String {
    val totalSeconds = milliseconds.coerceAtLeast(0) / 1000
    val hours = totalSeconds / 3600
    val minutes = totalSeconds % 3600 / 60
    val seconds = totalSeconds % 60
    return if (hours > 0) "%d:%02d:%02d".format(hours, minutes, seconds) else "%d:%02d".format(minutes, seconds)
}

@Composable
private fun ContactCallRow(contact: PhoneContact, enabled: Boolean, onCall: () -> Unit) {
    Row(Modifier.fillMaxWidth().padding(vertical = 10.dp), verticalAlignment = Alignment.CenterVertically) {
        PersonAvatar(contact.name)
        Spacer(Modifier.width(14.dp))
        Column(Modifier.weight(1f)) {
            Text(contact.name, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.SemiBold, maxLines = 1, overflow = TextOverflow.Ellipsis)
            Text(contact.number, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            if (contact.sourceNames.isNotEmpty()) Text(
                "From ${contact.sourceNames.joinToString()}",
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
        }
        IconButton(onClick = onCall, enabled = enabled) { Icon(Icons.Default.Call, "Call ${contact.name}") }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun DialerSheet(
    initialNumber: String,
    ui: AppUiState,
    callSnapshot: VoiceCallSnapshot,
    serverCallActive: Boolean,
    anotherClientOwnsCall: Boolean,
    onDismiss: () -> Unit,
    onNumberChange: (String) -> Unit,
    onCall: (String) -> Unit,
    onAnswer: () -> Unit,
    onReject: () -> Unit,
    onHangup: () -> Unit,
    onSetMuted: (Boolean) -> Unit,
    onSendDtmf: (Char) -> Unit,
    onSelectAudioEndpoint: (String) -> Unit,
) {
    var number by rememberSaveable(initialNumber) { mutableStateOf(initialNumber) }
    val ending = ui.voice.ending || callSnapshot.phase == com.viti.operator.voice.VoiceCallPhase.ENDING
    val dialRows = if (ui.voice.inCall) listOf("123", "456", "789", "*0#") else listOf("123", "456", "789", "+0⌫")
    ModalBottomSheet(onDismissRequest = onDismiss) {
        Column(
            Modifier.fillMaxWidth().widthIn(max = 520.dp).align(Alignment.CenterHorizontally).verticalScroll(rememberScrollState()).padding(horizontal = 24.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            Text(
                when {
                    ending -> "Ending call"
                    ui.voice.inCall -> ui.voice.peer.ifBlank { "Call in progress" }
                    else -> "Dial pad"
                },
                style = MaterialTheme.typography.headlineSmall,
                fontWeight = FontWeight.Bold,
            )
            Spacer(Modifier.height(12.dp))
            OutlinedTextField(
                value = number,
                onValueChange = { number = it; onNumberChange(it) },
                modifier = Modifier.fillMaxWidth(),
                placeholder = { Text(if (ui.voice.inCall) "DTMF tones" else "Phone number") },
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Phone),
                readOnly = ui.voice.inCall || ending,
                singleLine = true,
            )
            Spacer(Modifier.height(12.dp))
            dialRows.forEach { row ->
                Row(horizontalArrangement = Arrangement.spacedBy(18.dp)) {
                    row.forEach { key ->
                        TextButton(
                            enabled = !ending,
                            onClick = {
                                if (ui.voice.inCall) onSendDtmf(key)
                                else {
                                    number = if (key == '⌫') number.dropLast(1) else number + key
                                    onNumberChange(number)
                                }
                            },
                            modifier = Modifier.size(72.dp, 58.dp),
                        ) { Text(key.toString(), style = MaterialTheme.typography.headlineSmall) }
                    }
                }
            }
            Spacer(Modifier.height(12.dp))
            if (ui.voice.inCall) {
                AudioRouteButton(callSnapshot, onSelectAudioEndpoint)
                if (ui.voice.quality != null) {
                    Spacer(Modifier.height(12.dp))
                    CallQualityCard(ui.voice.quality, callSnapshot.legToken)
                }
                Spacer(Modifier.height(12.dp))
            }
            when {
                ending -> Button(
                    onClick = if (callSnapshot.endingRejected) onReject else onHangup,
                    modifier = Modifier.fillMaxWidth().height(52.dp),
                ) {
                    Icon(Icons.Default.CallEnd, null)
                    Spacer(Modifier.width(8.dp))
                    Text(if (callSnapshot.endingRejected) "Retry reject" else "Retry hang up")
                }
                ui.voice.incoming -> Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                    Button(onClick = onAnswer, modifier = Modifier.weight(1f).height(52.dp)) { Text("Answer") }
                    OutlinedButton(onClick = onReject, modifier = Modifier.weight(1f).height(52.dp)) { Text("Reject") }
                }
                ui.voice.inCall -> Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                    FilledTonalButton(
                        onClick = { onSetMuted(!ui.voice.microphoneMuted) },
                        modifier = Modifier.weight(1f).height(52.dp),
                    ) {
                        Icon(if (ui.voice.microphoneMuted) Icons.Default.Mic else Icons.Default.MicOff, null)
                        Spacer(Modifier.width(6.dp))
                        Text(if (ui.voice.microphoneMuted) "Unmute" else "Mute")
                    }
                    Button(onClick = onHangup, modifier = Modifier.weight(1f).height(52.dp)) {
                        Icon(Icons.Default.CallEnd, null); Spacer(Modifier.width(8.dp)); Text("Hang up")
                    }
                }
                else -> Button(
                    onClick = { onCall(number) },
                    enabled = ui.status.ready && ui.voice.registered && !serverCallActive && number.isNotBlank(),
                    modifier = Modifier.fillMaxWidth().height(52.dp),
                ) {
                    Icon(Icons.Default.Call, null); Spacer(Modifier.width(8.dp))
                    Text(if (anotherClientOwnsCall) "Line busy" else if (ui.voice.registered) "Call" else "Connecting…")
                }
            }
            Spacer(Modifier.height(20.dp))
        }
    }
}

@Composable
private fun CallQualityCard(quality: VoiceCallQuality?, legToken: String) {
    if (quality == null) return
    var expanded by rememberSaveable(legToken) { mutableStateOf(false) }
    val status = when (quality.status) {
        "connecting" -> "Connecting media…"
        "collecting" -> "Collecting measurements…"
        "reconnecting" -> "Media reconnecting…"
        "unsupported" -> "Live statistics unavailable"
        "unavailable" -> "Measurements unavailable"
        else -> "Live media quality"
    }
    val route = quality.route?.let {
        "${if (it.mode == "turn-relay") "TURN relay" else "ICE direct"} · ${it.protocol.uppercase()}"
    }.orEmpty()
    Card(
        onClick = { expanded = !expanded },
        modifier = Modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerHigh),
        shape = RoundedCornerShape(16.dp),
    ) {
        Column(Modifier.fillMaxWidth().padding(14.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
            Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
                Column(Modifier.weight(1f)) {
                    Text(status, style = MaterialTheme.typography.titleSmall, fontWeight = FontWeight.SemiBold)
                    if (route.isNotBlank()) Text(route, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
                Text(if (expanded) "Hide details" else "Details", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.primary)
            }
            if (expanded) {
                Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    QualityMetric("Incoming loss", qualityPercent(quality.inboundLossPercent), Modifier.weight(1f))
                    QualityMetric("Outgoing loss", qualityPercent(quality.outboundLossPercent), Modifier.weight(1f))
                }
                Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    QualityMetric("Incoming jitter", qualityMilliseconds(quality.inboundJitterMs), Modifier.weight(1f))
                    QualityMetric(
                        if (quality.roundTripScope == "media-peer") "Media RTT" else if (quality.roundTripScope == "ice-path") "ICE path RTT" else "Round-trip time",
                        qualityMilliseconds(quality.roundTripTimeMs),
                        Modifier.weight(1f),
                    )
                }
                Text(
                    "Incoming values describe RTP received by this device. Outgoing loss is reported by the media peer. RTT covers only the WebRTC media path, not the mobile operator or PSTN leg.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
    }
}

@Composable
private fun QualityMetric(label: String, value: String, modifier: Modifier = Modifier) {
    Surface(modifier = modifier, shape = RoundedCornerShape(12.dp), color = MaterialTheme.colorScheme.surface) {
        Column(Modifier.padding(10.dp)) {
            Text(label, style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
            Text(value, style = MaterialTheme.typography.titleSmall, fontWeight = FontWeight.SemiBold)
        }
    }
}

private fun qualityPercent(value: Double?): String = value?.let { "${qualityNumber(it)} %" } ?: "Unavailable"
private fun qualityMilliseconds(value: Double?): String = value?.let { "${qualityNumber(it)} ms" } ?: "Unavailable"
private fun qualityNumber(value: Double): String = if (value < 10) "%.1f".format(value) else "%.0f".format(value)

@Composable
private fun AudioRouteButton(
    snapshot: VoiceCallSnapshot,
    onSelect: (String) -> Unit,
) {
    val endpoints = snapshot.audio.available
    if (endpoints.isEmpty()) return
    var expanded by remember { mutableStateOf(false) }
    Box(Modifier.fillMaxWidth()) {
        FilledTonalButton(
            onClick = { expanded = true },
            enabled = snapshot.platformReady,
            modifier = Modifier.fillMaxWidth().heightIn(min = 48.dp),
        ) {
            Icon(Icons.AutoMirrored.Filled.VolumeUp, null)
            Spacer(Modifier.width(8.dp))
            Text(
                snapshot.audio.current?.name ?: "Choose audio route",
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
        }
        DropdownMenu(expanded = expanded, onDismissRequest = { expanded = false }) {
            endpoints.forEach { endpoint ->
                DropdownMenuItem(
                    text = { Text(endpoint.name) },
                    leadingIcon = { Icon(Icons.AutoMirrored.Filled.VolumeUp, null) },
                    onClick = {
                        expanded = false
                        onSelect(endpoint.id)
                    },
                )
            }
        }
    }
}

@Composable
private fun PersonAvatar(label: String, size: androidx.compose.ui.unit.Dp = 48.dp) {
    Surface(shape = CircleShape, color = MaterialTheme.colorScheme.primaryContainer, contentColor = MaterialTheme.colorScheme.onPrimaryContainer, modifier = Modifier.size(size)) {
        Box(contentAlignment = Alignment.Center) {
            val initial = label.trim().firstOrNull()?.uppercaseChar()
            if (initial != null && initial.isLetter()) Text(initial.toString(), style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.Bold)
            else Icon(Icons.Default.Person, null, Modifier.size(size * .55f))
        }
    }
}

@Composable
private fun EmptyListState(icon: ImageVector, title: String, detail: String) {
    Column(
        Modifier.fillMaxWidth().padding(vertical = 48.dp, horizontal = 20.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Surface(shape = CircleShape, color = MaterialTheme.colorScheme.surfaceContainerHigh) {
            Icon(icon, null, Modifier.padding(18.dp).size(28.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Spacer(Modifier.height(14.dp))
        Text(title, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.SemiBold)
        Spacer(Modifier.height(4.dp))
        Text(detail, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

private fun messagePeer(message: Message): String = if (message.direction == "incoming") message.from else message.to

private fun contactName(contacts: List<PhoneContact>, number: String): String {
    val normalized = number.filter(Char::isDigit)
    return contacts.firstOrNull { it.number.filter(Char::isDigit) == normalized }?.name ?: number
}

private fun formatActivityTime(value: String): String {
    val instant = runCatching { Instant.parse(value) }.getOrNull() ?: return ""
    val zone = ZoneId.systemDefault()
    val time = instant.atZone(zone)
    val today = Instant.now().atZone(zone).toLocalDate()
    return when (time.toLocalDate()) {
        today -> time.format(DateTimeFormatter.ofPattern("HH:mm"))
        today.minusDays(1) -> "Yesterday"
        else -> time.format(DateTimeFormatter.ofPattern("MMM d", java.util.Locale.forLanguageTag("en-US")))
    }
}

private fun formatCallDuration(call: Call): String? {
    if (call.endedAt.isBlank()) return null
    val start = runCatching { Instant.parse(call.startedAt) }.getOrNull() ?: return null
    val end = runCatching { Instant.parse(call.endedAt) }.getOrNull() ?: return null
    val seconds = Duration.between(start, end).seconds.coerceAtLeast(0)
    return if (seconds < 60) "${seconds}s" else "${seconds / 60} min"
}

@Composable
private fun ConnectionScreen(
    ui: AppUiState,
    settings: ServerSettings,
    authRunning: Boolean,
    authDetail: String,
    onSave: (ServerSettings) -> Unit,
    onPrepareShizuku: () -> Unit,
    onStartAuthAgent: () -> Unit,
    onStopAuthAgent: () -> Unit,
	phoneRouting: com.viti.operator.data.PhoneRoutingSettings,
	onEnableServerLine: () -> Unit,
	onReturnLineToPhone: () -> Unit,
    onSetRecordingEnabled: (Boolean) -> Unit,
    callPresentationStatus: IncomingCallPresentationStatus,
    onRequestCallNotifications: () -> Unit,
    onOpenFullScreenCallSettings: () -> Unit,
    onOpenCallChannelSettings: () -> Unit,
) {
    var url by rememberSaveable(settings.serverUrl) { mutableStateOf(settings.serverUrl) }
    var token by rememberSaveable(settings.apiKey) { mutableStateOf(settings.apiKey) }
	var showReturnToPhoneDialog by rememberSaveable { mutableStateOf(false) }
	if (showReturnToPhoneDialog) {
		AlertDialog(
			onDismissRequest = { showReturnToPhoneDialog = false },
			icon = { Icon(Icons.Default.WarningAmber, contentDescription = null) },
			title = { Text("Return the line to the phone?") },
			text = {
				Text(
					"The phone is currently not registering with IMS and cannot use 2G or 3G. " +
						"If the server unregisters, incoming calls will have nowhere to go. " +
						"Do you want to unregister the server and re-enable IMS and 2G/3G on the phone?",
				)
			},
			confirmButton = {
				Button(onClick = {
					showReturnToPhoneDialog = false
					onReturnLineToPhone()
				}) { Text("Yes, use the phone") }
			},
			dismissButton = {
				TextButton(onClick = { showReturnToPhoneDialog = false }) { Text("No") }
			},
		)
	}
    Column(
        Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(16.dp),
    ) {
        Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
            SectionTitle("Connection", "Real-time line status", Modifier.weight(1f))
            TopologyOverallChip(topologyOverallState(ui.topology))
        }
        TopologyViewerCard(settings, ui.clientId)
        CellularDetailsPermissionCard()
        IncomingCallAlertsCard(
            status = callPresentationStatus,
            onRequestNotifications = onRequestCallNotifications,
            onOpenFullScreenSettings = onOpenFullScreenCallSettings,
            onOpenChannelSettings = onOpenCallChannelSettings,
        )
        ElevatedCard {
            Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                Text("Server", style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.SemiBold)
                OutlinedTextField(url, { url = it }, label = { Text("URL") }, modifier = Modifier.fillMaxWidth(), singleLine = true)
                Text(
                    "You can enter only the host name or IP address. If you omit the scheme or port, the app uses http:// and private port 18788.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                OutlinedTextField(token, { token = it }, label = { Text("Access token") }, visualTransformation = PasswordVisualTransformation(), modifier = Modifier.fillMaxWidth(), singleLine = true)
                Button(onClick = { onSave(settings.copy(serverUrl = url, apiKey = token)) }, modifier = Modifier.fillMaxWidth()) { Text("Save and check") }
            }
        }
        ElevatedCard {
            Row(Modifier.fillMaxWidth().padding(20.dp), verticalAlignment = Alignment.CenterVertically) {
                Column(Modifier.weight(1f)) {
                    Text("Call recording", style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.SemiBold)
                    Text(
                        "Store the other party and every connected device as separate lossless tracks on the server.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                Spacer(Modifier.width(16.dp))
                Switch(
                    checked = ui.recording.enabled,
                    onCheckedChange = onSetRecordingEnabled,
                    enabled = !ui.busy,
                )
            }
        }
		ElevatedCard {
			Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
				Row(verticalAlignment = Alignment.CenterVertically) {
					Icon(Icons.Default.SwapHoriz, contentDescription = null)
					Spacer(Modifier.width(10.dp))
					Column(Modifier.weight(1f)) {
						Text("Line destination", style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.SemiBold)
						Text(
							if (phoneRouting.remoteSimModeEnabled) "Calls are received by Viti Operator" else "Native phone service is available",
							style = MaterialTheme.typography.bodyMedium,
							color = MaterialTheme.colorScheme.onSurfaceVariant,
						)
					}
				}
				HorizontalDivider()
				Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
					Column(Modifier.weight(1f)) {
						Text("Isolate native phone service", style = MaterialTheme.typography.titleMedium)
						Text(
							if (phoneRouting.remoteSimModeEnabled) "Phone IMS disabled · 4G/5G only" else "IMS and legacy networks available",
							style = MaterialTheme.typography.bodySmall,
							color = MaterialTheme.colorScheme.onSurfaceVariant,
						)
					}
					Switch(
						checked = phoneRouting.remoteSimModeEnabled,
						onCheckedChange = { enabled ->
							if (enabled) onEnableServerLine() else showReturnToPhoneDialog = true
						},
						enabled = !ui.busy,
					)
				}
				if (phoneRouting.remoteSimModeEnabled) {
					Surface(
						shape = RoundedCornerShape(16.dp),
						color = if (ui.serverIMS.enabled) MaterialTheme.colorScheme.primaryContainer else MaterialTheme.colorScheme.errorContainer,
					) {
						Column(Modifier.fillMaxWidth().padding(16.dp)) {
							Text(
								if (ui.serverIMS.enabled) "Server IMS registration active" else "Line has no IMS destination",
								style = MaterialTheme.typography.titleSmall,
							)
							Text(
								if (ui.serverIMS.registered) "Registered with the carrier" else ui.serverIMS.detail.ifBlank { "Waiting for carrier confirmation" },
								style = MaterialTheme.typography.bodySmall,
							)
						}
					}
					if (ui.serverIMS.enabled) {
						OutlinedButton(
							onClick = { showReturnToPhoneDialog = true },
							modifier = Modifier.fillMaxWidth(),
							enabled = !ui.busy,
						) { Text("Unregister server") }
					} else {
						Button(onClick = onEnableServerLine, modifier = Modifier.fillMaxWidth(), enabled = !ui.busy) {
							Text("Register server")
						}
					}
				} else {
					Button(onClick = onEnableServerLine, modifier = Modifier.fillMaxWidth(), enabled = !ui.busy) {
						Text("Use the line in Viti Operator")
					}
				}
			}
		}
        ElevatedCard {
            Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Icon(Icons.Default.PhoneAndroid, null)
                    Spacer(Modifier.width(10.dp))
                    Text("Authentication with this SIM", style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.SemiBold)
                }
                Text(authDetail, color = MaterialTheme.colorScheme.onSurfaceVariant)
                Text(
                    when {
                        ui.topology.sim.holderClientId == ui.clientId -> "This phone is resolving authentication challenges for the line."
                        ui.topology.sim.mode == "remote" -> "Another connected phone is resolving authentication challenges."
                        ui.topology.sim.mode == "openstick" -> "The server is currently using the USB modem automatically."
                        else -> "No authentication source is available."
                    },
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                FilledTonalButton(onClick = onPrepareShizuku, modifier = Modifier.fillMaxWidth()) {
                    Icon(Icons.Default.Key, null)
                    Spacer(Modifier.width(8.dp))
                    Text("Prepare permission with Shizuku")
                }
                if (authRunning) {
                    OutlinedButton(onClick = onStopAuthAgent, modifier = Modifier.fillMaxWidth()) {
                        Icon(Icons.Default.Delete, null)
                        Spacer(Modifier.width(8.dp))
                        Text("Stop using this phone")
                    }
                } else {
                    Button(onClick = onStartAuthAgent, modifier = Modifier.fillMaxWidth()) {
                        Icon(Icons.Default.CellTower, null)
                        Spacer(Modifier.width(8.dp))
                        Text("Use this SIM for authentication")
                    }
                }
                Text(
                    "The most recently activated phone takes over. If none is available, the server falls back to the USB modem.",
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        Spacer(Modifier.height(24.dp))
    }
}

@Composable
private fun CellularDetailsPermissionCard() {
    val context = LocalContext.current
    var granted by remember {
        mutableStateOf(
            ContextCompat.checkSelfPermission(context, Manifest.permission.READ_PHONE_STATE) == PackageManager.PERMISSION_GRANTED,
        )
    }
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { allowed ->
        granted = allowed
        CellularTelemetryMonitor.get(context).refreshRegistration()
    }
    ElevatedCard(modifier = Modifier.fillMaxWidth()) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
                Surface(
                    modifier = Modifier.size(40.dp),
                    shape = CircleShape,
                    color = if (granted) MaterialTheme.colorScheme.primaryContainer else MaterialTheme.colorScheme.surfaceContainerHighest,
                    contentColor = if (granted) MaterialTheme.colorScheme.onPrimaryContainer else MaterialTheme.colorScheme.onSurfaceVariant,
                ) {
                    Box(contentAlignment = Alignment.Center) {
                        Icon(Icons.Default.CellTower, contentDescription = null, modifier = Modifier.size(22.dp))
                    }
                }
                Spacer(Modifier.width(12.dp))
                Column(Modifier.weight(1f)) {
                    Text("Cellular network details", style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.SemiBold)
                    Text(
                        if (granted) "Available to the connection inspector" else "Optional phone permission",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
            Text(
                "Viti can report the home carrier, serving network, service state, data technology, and signal level " +
                    "for the same default-data subscription used for SIM authentication. This metadata is sent to your Viti server. " +
                    "Phone number, IMSI, ICCID, SIM slot, and cell location are never included.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            if (!granted) {
                FilledTonalButton(
                    onClick = { launcher.launch(Manifest.permission.READ_PHONE_STATE) },
                    modifier = Modifier.fillMaxWidth().heightIn(min = 48.dp),
                ) {
                    Text("Allow network details")
                }
            }
        }
    }
}

@Composable
private fun IncomingCallAlertsCard(
    status: IncomingCallPresentationStatus,
    onRequestNotifications: () -> Unit,
    onOpenFullScreenSettings: () -> Unit,
    onOpenChannelSettings: () -> Unit,
) {
    ElevatedCard(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Icon(Icons.Default.NotificationsActive, contentDescription = null)
                Spacer(Modifier.width(10.dp))
                Column(Modifier.weight(1f)) {
                    Text("Incoming call alerts", style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.SemiBold)
                    Text(
                        "Android presents Viti service calls without replacing your cellular dialer.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
            HorizontalDivider()
            CallAlertCapabilityRow(
                icon = Icons.Default.NotificationsActive,
                title = "Notifications",
                detail = if (status.notificationsEnabled) "Allowed" else "Blocked — calls can ring without a visible alert",
                available = status.notificationsEnabled,
            )
            if (!status.notificationsEnabled) {
                Button(onClick = onRequestNotifications, modifier = Modifier.fillMaxWidth()) {
                    Text("Allow call notifications")
                }
            }
            CallAlertCapabilityRow(
                icon = Icons.Default.Call,
                title = "Call alert channel",
                detail = when {
                    status.callChannelImportance == null -> "Managed by this Android version"
                    !status.callChannelEnabled -> "Blocked"
                    status.prominentCallChannel -> "High priority"
                    else -> "Reduced priority — heads-up alerts may not appear"
                },
                available = status.callChannelEnabled && status.prominentCallChannel,
            )
            OutlinedButton(onClick = onOpenChannelSettings, modifier = Modifier.fillMaxWidth()) {
                Text("Manage call alert channel")
            }
            CallAlertCapabilityRow(
                icon = Icons.Default.Fullscreen,
                title = "Full screen while locked",
                detail = if (status.fullScreenIntentAllowed) {
                    "Allowed by Android"
                } else {
                    "Not allowed — Android will use a heads-up fallback"
                },
                available = status.fullScreenIntentAllowed,
            )
            if (status.fullScreenSettingAvailable && !status.fullScreenIntentAllowed) {
                FilledTonalButton(onClick = onOpenFullScreenSettings, modifier = Modifier.fillMaxWidth()) {
                    Text("Allow full-screen call alerts")
                }
            }
            Text(
                "The final full-screen or heads-up presentation is chosen by Android and the device manufacturer. " +
                    "Viti keeps its own ringtone active until this device's call leg is answered or ended.",
                style = MaterialTheme.typography.labelMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

@Composable
private fun CallAlertCapabilityRow(
    icon: ImageVector,
    title: String,
    detail: String,
    available: Boolean,
) {
    Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
        Surface(
            modifier = Modifier.size(40.dp),
            shape = CircleShape,
            color = if (available) MaterialTheme.colorScheme.primaryContainer else MaterialTheme.colorScheme.errorContainer,
            contentColor = if (available) MaterialTheme.colorScheme.onPrimaryContainer else MaterialTheme.colorScheme.onErrorContainer,
        ) {
            Box(contentAlignment = Alignment.Center) {
                Icon(icon, contentDescription = null, modifier = Modifier.size(22.dp))
            }
        }
        Spacer(Modifier.width(12.dp))
        Column(Modifier.weight(1f)) {
            Text(title, style = MaterialTheme.typography.titleMedium)
            Text(detail, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

private data class TopologyViewerConfig(
    val viewerUrl: String,
    val token: String,
    val currentClientId: String,
)

@Composable
private fun TopologyViewerCard(serverSettings: ServerSettings, currentClientId: String) {
    val context = LocalContext.current
    val viewerUrl = remember(serverSettings.serverUrl) {
        "${normalizeServerUrl(serverSettings.serverUrl)}/topology.html?embed=android"
    }
    val webView = remember(context) {
        var mapGesture = false
        WebView(context).apply {
            setBackgroundColor(AndroidColor.TRANSPARENT)
            settings.javaScriptEnabled = true
            settings.domStorageEnabled = true
            settings.cacheMode = WebSettings.LOAD_NO_CACHE
            settings.builtInZoomControls = false
            settings.displayZoomControls = false
            settings.setSupportZoom(false)
            isHorizontalScrollBarEnabled = false
            isVerticalScrollBarEnabled = false
            setOnTouchListener { view, event ->
                when (event.actionMasked) {
                    MotionEvent.ACTION_DOWN -> {
                        val mapTop = view.height * .08f
                        val mapBottom = view.height * .82f
                        mapGesture = event.y in mapTop..mapBottom
                        view.parent?.requestDisallowInterceptTouchEvent(mapGesture)
                    }
                    MotionEvent.ACTION_MOVE, MotionEvent.ACTION_POINTER_DOWN ->
                        view.parent?.requestDisallowInterceptTouchEvent(mapGesture)
                    MotionEvent.ACTION_UP, MotionEvent.ACTION_CANCEL -> {
                        view.parent?.requestDisallowInterceptTouchEvent(false)
                        mapGesture = false
                    }
                }
                false
            }
            webViewClient = object : AndroidWebViewClient() {
                override fun onPageFinished(view: WebView, url: String) {
                    configureTopologyViewer(view)
                }
            }
        }
    }

    DisposableEffect(webView) {
        onDispose {
            webView.stopLoading()
            webView.destroy()
        }
    }

    ElevatedCard(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(24.dp),
        colors = CardDefaults.elevatedCardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerLow),
    ) {
        Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Column(Modifier.padding(start = 20.dp, top = 20.dp, end = 20.dp)) {
                Text("Line path", style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.SemiBold)
                Text(
                    "Drag to pan and pinch to zoom",
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            AndroidView(
                factory = { webView },
                modifier = Modifier.fillMaxWidth().height(620.dp),
                update = { view ->
                    view.tag = TopologyViewerConfig(viewerUrl, serverSettings.apiKey, currentClientId)
                    if (view.url != viewerUrl) view.loadUrl(viewerUrl) else configureTopologyViewer(view)
                },
            )
        }
    }
}

private fun configureTopologyViewer(view: WebView) {
    val config = view.tag as? TopologyViewerConfig ?: return
    val payload = JSONObject()
        .put("token", config.token)
        .put("currentClientId", config.currentClientId)
        .toString()
    view.evaluateJavascript("if (window.vitiTopology) window.vitiTopology.configure($payload);", null)
}

@Composable
private fun ConnectionTopologyCard(topology: ConnectionTopology, currentClientId: String) {
    val clients = topology.clients
    ElevatedCard(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(24.dp),
        colors = CardDefaults.elevatedCardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerLow),
    ) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(18.dp)) {
            Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.Top) {
                Column(Modifier.weight(1f)) {
                    Text("Line path", style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.SemiBold)
                    Text(
                        "From the SIM to the carrier and every client",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                LiveChip()
            }
            BoxWithConstraints(Modifier.fillMaxWidth()) {
                if (maxWidth >= 840.dp) {
                    WideTopologyMap(topology, clients, currentClientId)
                } else {
                    NarrowTopologyMap(topology, clients, currentClientId)
                }
            }
            TopologyInsight(topology)
            TopologyLegend()
        }
    }
}

@Composable
private fun WideTopologyMap(topology: ConnectionTopology, clients: List<TopologyClient>, currentClientId: String) {
    Column(Modifier.fillMaxWidth(), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
            TopologyNodeCard(topology.sim.label, simDetail(topology), topology.sim.state, "SIM", Modifier.weight(1f))
            TopologyConnector(simConnectorLabel(topology), topology.sim.state)
            TopologyNodeCard(topology.server.label, topology.server.detail, topology.server.state, "S", Modifier.weight(1f), emphasized = true)
            Column(Modifier.weight(1.45f), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                clients.forEach { client -> ClientTopologyNode(client, currentClientId, showConnector = true) }
            }
        }
        Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.Top) {
            Spacer(Modifier.weight(1f))
            Spacer(Modifier.width(74.dp))
            OperatorTopologyPath(topology, Modifier.weight(1f))
            Spacer(Modifier.weight(1.45f))
        }
    }
}

@Composable
private fun NarrowTopologyMap(topology: ConnectionTopology, clients: List<TopologyClient>, currentClientId: String) {
    Column(Modifier.fillMaxWidth(), horizontalAlignment = Alignment.CenterHorizontally) {
        TopologyNodeCard(topology.sim.label, simDetail(topology), topology.sim.state, "SIM", Modifier.fillMaxWidth())
        TopologyConnector(simConnectorLabel(topology), topology.sim.state, vertical = true)
        TopologyNodeCard(topology.server.label, topology.server.detail, topology.server.state, "S", Modifier.fillMaxWidth(), emphasized = true)
        OperatorTopologyPath(topology, Modifier.fillMaxWidth())
        HorizontalDivider(Modifier.padding(top = 18.dp, bottom = 14.dp))
        Text("Connected clients", Modifier.fillMaxWidth(), style = MaterialTheme.typography.titleSmall, fontWeight = FontWeight.SemiBold)
        Spacer(Modifier.height(10.dp))
        Row(
            Modifier.fillMaxWidth().horizontalScroll(rememberScrollState()),
            horizontalArrangement = Arrangement.spacedBy(10.dp),
        ) {
            clients.forEach { client ->
                ClientTopologyNode(client, currentClientId, showConnector = false, modifier = Modifier.width(156.dp))
            }
        }
    }
}

@Composable
private fun OperatorTopologyPath(topology: ConnectionTopology, modifier: Modifier = Modifier) {
    val imsState = topologyVisualState(topology.ims.state, topology.epdg.state)
    val operatorState = topologyVisualState(topology.operator.state, topology.ims.state)
    Column(modifier, horizontalAlignment = Alignment.CenterHorizontally) {
        TopologyConnector("ePDG tunnel", topology.epdg.state, vertical = true)
        TopologyNodeCard(topology.epdg.label, topology.epdg.detail, topology.epdg.state, "E", Modifier.fillMaxWidth())
        TopologyConnector("IMS registration", imsState, vertical = true)
        TopologyNodeCard(topology.ims.label, topology.ims.detail, imsState, "I", Modifier.fillMaxWidth())
        TopologyConnector("Carrier network", operatorState, vertical = true)
        TopologyNodeCard(topology.operator.label, topology.operator.detail, operatorState, "O", Modifier.fillMaxWidth())
    }
}

@Composable
private fun ClientTopologyNode(
    client: TopologyClient,
    currentClientId: String,
    showConnector: Boolean,
    modifier: Modifier = Modifier,
) {
    val current = client.id == currentClientId
    val state = when {
        current && client.online -> "healthy"
        client.online -> "peer"
        else -> "idle"
    }
    val detail = when {
        current -> "This phone"
        client.online -> "Available now"
        else -> "Offline"
    }
    if (showConnector) {
        Row(modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
            TopologyConnector("WebRTC / API", state)
            TopologyNodeCard(client.name, detail, state, if (client.kind == "android") "A" else "W", Modifier.weight(1f), badge = if (client.simHolder) "SIM" else "")
        }
    } else {
        TopologyNodeCard(client.name, detail, state, if (client.kind == "android") "A" else "W", modifier, badge = if (client.simHolder) "SIM" else "")
    }
}

@Composable
private fun TopologyNodeCard(
    label: String,
    detail: String,
    state: String,
    monogram: String,
    modifier: Modifier = Modifier,
    emphasized: Boolean = false,
    badge: String = "",
) {
    val color = topologyColor(state)
    Surface(
        modifier = modifier.alpha(if (state == "idle") .68f else 1f),
        shape = RoundedCornerShape(16.dp),
        color = when (state) {
            "healthy", "warning", "failed" -> color.copy(alpha = .09f)
            else -> MaterialTheme.colorScheme.surfaceContainer
        },
        border = androidx.compose.foundation.BorderStroke(if (emphasized) 2.dp else 1.dp, if (emphasized) MaterialTheme.colorScheme.primary else color.copy(alpha = .6f)),
        shadowElevation = if (emphasized) 4.dp else 0.dp,
    ) {
        Row(Modifier.padding(if (emphasized) 16.dp else 13.dp), verticalAlignment = Alignment.CenterVertically) {
            Surface(shape = RoundedCornerShape(11.dp), color = color.copy(alpha = .14f), contentColor = color) {
                Box(Modifier.size(38.dp), contentAlignment = Alignment.Center) {
                    Text(monogram, style = MaterialTheme.typography.labelMedium, fontWeight = FontWeight.Black)
                }
            }
            Spacer(Modifier.width(10.dp))
            Column(Modifier.weight(1f)) {
                Text(label, style = MaterialTheme.typography.labelLarge, maxLines = 1, overflow = TextOverflow.Ellipsis)
                Text(
                    detail.ifBlank { topologyStatusLabel(state) },
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 2,
                    overflow = TextOverflow.Ellipsis,
                )
                Spacer(Modifier.height(5.dp))
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Surface(Modifier.size(7.dp), shape = CircleShape, color = color) {}
                    Spacer(Modifier.width(5.dp))
                    Text(topologyStatusLabel(state), style = MaterialTheme.typography.labelSmall, color = color, fontWeight = FontWeight.SemiBold)
                }
            }
            if (badge.isNotBlank()) {
                Surface(shape = CircleShape, color = MaterialTheme.colorScheme.primaryContainer, contentColor = MaterialTheme.colorScheme.onPrimaryContainer) {
                    Text(badge, Modifier.padding(horizontal = 7.dp, vertical = 4.dp), style = MaterialTheme.typography.labelSmall, fontWeight = FontWeight.Bold)
                }
            }
        }
    }
}

@Composable
private fun TopologyConnector(label: String, state: String, vertical: Boolean = false) {
    val color = topologyColor(state).copy(alpha = if (state == "idle") .45f else .85f)
    if (vertical) {
        Column(horizontalAlignment = Alignment.CenterHorizontally) {
            Box(Modifier.width(2.dp).height(14.dp), contentAlignment = Alignment.Center) {
                Surface(Modifier.fillMaxSize(), color = color) {}
            }
            Text(label, style = MaterialTheme.typography.labelSmall, color = color)
            Box(Modifier.width(2.dp).height(14.dp), contentAlignment = Alignment.Center) {
                Surface(Modifier.fillMaxSize(), color = color) {}
            }
        }
    } else {
        Column(Modifier.width(74.dp), horizontalAlignment = Alignment.CenterHorizontally) {
            Text(label, style = MaterialTheme.typography.labelSmall, color = color, maxLines = 1)
            Surface(Modifier.fillMaxWidth().height(2.dp), color = color) {}
        }
    }
}

@Composable
private fun LiveChip() {
    Surface(shape = CircleShape, color = MaterialTheme.colorScheme.surfaceContainerHigh) {
        Row(Modifier.padding(horizontal = 10.dp, vertical = 7.dp), verticalAlignment = Alignment.CenterVertically) {
            Surface(Modifier.size(7.dp), shape = CircleShape, color = Color(0xFF287D4F)) {}
            Spacer(Modifier.width(6.dp))
            Text("Live", style = MaterialTheme.typography.labelSmall, fontWeight = FontWeight.SemiBold)
        }
    }
}

@Composable
private fun TopologyOverallChip(state: String) {
    val color = topologyColor(state)
    Surface(shape = CircleShape, color = color.copy(alpha = .12f), contentColor = color) {
        Row(Modifier.padding(horizontal = 11.dp, vertical = 7.dp), verticalAlignment = Alignment.CenterVertically) {
            Surface(Modifier.size(8.dp), shape = CircleShape, color = color) {}
            Spacer(Modifier.width(6.dp))
            Text(topologyStatusLabel(state), style = MaterialTheme.typography.labelMedium, fontWeight = FontWeight.SemiBold)
        }
    }
}

@Composable
private fun TopologyInsight(topology: ConnectionTopology) {
    val imsState = topologyVisualState(topology.ims.state, topology.epdg.state)
    val state: String
    val message: String
    when {
        topology.sim.state == "failed" -> {
            state = "failed"
            message = "SIM authentication is unavailable. An established carrier session may continue, but a fresh authentication challenge or new operation cannot."
        }
        topology.epdg.state == "failed" -> {
            state = "failed"
            message = "Clients remain connected to the server, but the line will be unavailable until the ePDG tunnel recovers."
        }
        topology.ims.state == "idle" -> {
            state = "idle"
            message = "IMS registration is paused; calls and SMS will not reach the carrier."
        }
        imsState != "healthy" -> {
            state = imsState
            message = "Clients remain connected to the server, but the line will be unavailable until IMS registration recovers."
        }
        else -> {
            state = "healthy"
            message = "Every segment of the line is operational."
        }
    }
    val color = topologyColor(state)
    Surface(
        shape = RoundedCornerShape(14.dp),
        color = MaterialTheme.colorScheme.primaryContainer.copy(alpha = .45f),
        border = androidx.compose.foundation.BorderStroke(1.dp, color.copy(alpha = .35f)),
    ) {
        Row(Modifier.fillMaxWidth().padding(14.dp), verticalAlignment = Alignment.CenterVertically) {
            Surface(Modifier.size(24.dp), shape = CircleShape, color = color, contentColor = Color.White) {
                Box(contentAlignment = Alignment.Center) { Text("i", fontWeight = FontWeight.Black, style = MaterialTheme.typography.labelMedium) }
            }
            Spacer(Modifier.width(10.dp))
            Text(message, Modifier.weight(1f), style = MaterialTheme.typography.bodySmall)
        }
    }
}

@Composable
private fun TopologyLegend() {
    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        HorizontalDivider()
        Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
            TopologyLegendItem("Operational", "healthy")
            TopologyLegendItem("Connected", "peer")
            TopologyLegendItem("Inactive", "idle")
        }
        Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(18.dp)) {
            TopologyLegendItem("Retrying", "warning")
            TopologyLegendItem("Problem", "failed")
        }
    }
}

@Composable
private fun TopologyLegendItem(label: String, state: String) {
    Row(verticalAlignment = Alignment.CenterVertically) {
        Surface(Modifier.size(8.dp), shape = CircleShape, color = topologyColor(state)) {}
        Spacer(Modifier.width(5.dp))
        Text(label, style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@Composable
private fun topologyColor(state: String): Color = when (state) {
    "healthy" -> Color(0xFF287D4F)
    "warning" -> Color(0xFFB96A00)
    "failed" -> MaterialTheme.colorScheme.error
    "peer" -> MaterialTheme.colorScheme.onSurface
    else -> MaterialTheme.colorScheme.outline
}

private fun topologyStatusLabel(state: String): String = when (state) {
    "healthy" -> "Operational"
    "failed" -> "Problem"
    "peer" -> "Connected"
    "warning" -> "Retrying"
    else -> "Inactive"
}

private fun topologyVisualState(state: String, upstreamState: String): String =
    if (state == "failed" && upstreamState == "healthy") "warning" else state

private fun topologyOverallState(topology: ConnectionTopology): String {
    val imsState = topologyVisualState(topology.ims.state, topology.epdg.state)
    val states = listOf(topology.sim.state, topology.epdg.state, imsState)
    return when {
        states.all { it == "healthy" } -> "healthy"
        states.any { it == "failed" } -> "failed"
        states.any { it == "warning" } -> "warning"
        else -> "idle"
    }
}

private fun simConnectorLabel(topology: ConnectionTopology): String =
    when (topology.sim.mode) {
        "remote" -> "Tailscale · AKA authentication"
        "openstick" -> "4G modem · USB / APDU"
        "command" -> "External adapter"
        else -> "No authentication source"
    }

private fun simDetail(topology: ConnectionTopology): String =
    if (topology.sim.mode == "remote") "The SIM responds from the phone" else topology.sim.detail

@Composable
private fun StatusGrid(status: RuntimeStatus) {
    BoxWithConstraints(Modifier.fillMaxWidth()) {
        val wide = maxWidth >= 600.dp
        val cards = listOf("SIM" to status.simReady, "ePDG tunnel" to status.tunnelReady, "IMS" to status.imsReady)
        if (wide) {
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                cards.forEach { (label, ready) -> StatusCard(label, ready, Modifier.weight(1f)) }
            }
        } else {
            Column(verticalArrangement = Arrangement.spacedBy(10.dp)) {
                cards.forEach { (label, ready) -> StatusCard(label, ready) }
            }
        }
    }
}

@Composable
private fun StatusCard(label: String, ready: Boolean, modifier: Modifier = Modifier) {
    Card(modifier, colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
        Row(Modifier.fillMaxWidth().padding(16.dp), verticalAlignment = Alignment.CenterVertically) {
            Icon(if (ready) Icons.Default.CheckCircle else Icons.Default.CloudOff, null, tint = if (ready) Color(0xFF287D4F) else MaterialTheme.colorScheme.error)
            Spacer(Modifier.width(10.dp))
            Column {
                Text(label, style = MaterialTheme.typography.labelLarge)
                Text(if (ready) "Available" else "Unavailable", style = MaterialTheme.typography.bodySmall)
            }
        }
    }
}

@Composable
private fun SectionTitle(title: String, subtitle: String, modifier: Modifier = Modifier) {
    Column(modifier) {
        Text(title, style = MaterialTheme.typography.headlineSmall, fontWeight = FontWeight.SemiBold)
        Text(subtitle, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@Composable
private fun EmptyCard(text: String, modifier: Modifier = Modifier) {
    Surface(modifier.fillMaxWidth(), shape = RoundedCornerShape(20.dp), color = MaterialTheme.colorScheme.surfaceContainer) {
        Text(text, Modifier.padding(24.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

private fun messageStateLabel(state: String): String = when (state) {
    "sending" -> "Sending"
    "submitted" -> "Unconfirmed"
    "accepted" -> "Accepted by network"
    "delivered" -> "Delivered"
    "received" -> "Received"
    "failed" -> "Failed"
    else -> state
}

private fun messageStatusLabel(message: Message): String = when {
    message.state == "failed" -> "Failed"
    message.deliveryState == "delivered" -> "Delivered"
    message.deliveryState == "unknown" -> "Delivery unconfirmed"
    message.state == "accepted" -> "Accepted by carrier"
    message.state == "submitted" -> "Submitted"
    else -> messageStateLabel(message.state)
}

private fun messageStatusDetail(message: Message): String = when {
    message.failureReason.isNotBlank() && message.failureCode.isNotBlank() ->
        "${message.failureReason} (${message.failureCode})"
    message.failureReason.isNotBlank() -> message.failureReason
    message.statusDetail.isNotBlank() -> message.statusDetail
    message.deliveryState == "unknown" -> "The carrier did not provide destination delivery confirmation."
    else -> ""
}

private fun callStateLabel(state: String): String = when (state) {
    "dialing" -> "Dialing"
    "ringing" -> "Ringing"
    "active" -> "In progress"
    "ended" -> "Ended"
    "failed" -> "Failed"
    else -> state
}

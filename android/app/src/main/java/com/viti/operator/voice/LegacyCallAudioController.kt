package com.viti.operator.voice

import android.content.Context
import android.content.Intent
import android.media.AudioDeviceCallback
import android.media.AudioDeviceInfo
import android.media.AudioManager
import android.media.session.MediaSession
import android.media.session.PlaybackState
import android.os.Handler
import android.os.Looper
import android.view.KeyEvent

/** Capability fallback for devices where self-managed Telecom is unavailable. */
@Suppress("DEPRECATION")
class LegacyCallAudioController(
    context: Context,
    private val onInactiveChanged: (Boolean) -> Unit,
    private val onAudioState: (PlatformAudioState) -> Unit,
    private val onAnswer: () -> Unit,
    private val onHangup: () -> Unit,
) : LegacyAudioControl {
    private val audioManager = context.applicationContext.getSystemService(Context.AUDIO_SERVICE) as AudioManager
    private val handler = Handler(Looper.getMainLooper())
    private var active = false
    private var incoming = false
    private var previousMode = AudioManager.MODE_NORMAL
    private var focusOwned = false
    private var selectedEndpointId = ""
    private val focusListener = AudioManager.OnAudioFocusChangeListener { change ->
        when (change) {
            AudioManager.AUDIOFOCUS_GAIN -> onInactiveChanged(false)
            AudioManager.AUDIOFOCUS_LOSS,
            AudioManager.AUDIOFOCUS_LOSS_TRANSIENT,
            AudioManager.AUDIOFOCUS_LOSS_TRANSIENT_CAN_DUCK,
            -> onInactiveChanged(true)
        }
    }
    private val deviceCallback = object : AudioDeviceCallback() {
        override fun onAudioDevicesAdded(addedDevices: Array<out AudioDeviceInfo>) = publishAudioState()
        override fun onAudioDevicesRemoved(removedDevices: Array<out AudioDeviceInfo>) = publishAudioState()
    }
    private val mediaSession = MediaSession(context.applicationContext, "VitiLegacyCall").apply {
        setFlags(MediaSession.FLAG_HANDLES_MEDIA_BUTTONS or MediaSession.FLAG_HANDLES_TRANSPORT_CONTROLS)
        setCallback(object : MediaSession.Callback() {
            override fun onMediaButtonEvent(mediaButtonIntent: Intent): Boolean {
                val event = mediaButtonIntent.getParcelableExtra<KeyEvent>(Intent.EXTRA_KEY_EVENT) ?: return false
                if (event.action != KeyEvent.ACTION_DOWN) return true
                return when (event.keyCode) {
                    KeyEvent.KEYCODE_CALL,
                    KeyEvent.KEYCODE_HEADSETHOOK,
                    KeyEvent.KEYCODE_MEDIA_PLAY,
                    KeyEvent.KEYCODE_MEDIA_PLAY_PAUSE,
                    -> {
                        if (incoming) onAnswer() else if (active) onHangup() else return false
                        true
                    }
                    KeyEvent.KEYCODE_ENDCALL,
                    KeyEvent.KEYCODE_MEDIA_STOP,
                    KeyEvent.KEYCODE_MEDIA_PAUSE,
                    -> {
                        if (!active && !incoming) return false
                        onHangup()
                        true
                    }
                    else -> false
                }
            }
        }, handler)
    }

    init {
        audioManager.registerAudioDeviceCallback(deviceCallback, handler)
    }

    override fun begin(direction: VoiceCallDirection) {
        incoming = direction == VoiceCallDirection.INCOMING
        active = direction == VoiceCallDirection.OUTGOING
        updateMediaSession()
        if (active) acquireAudio()
        publishAudioState()
    }

    override fun setActive(value: Boolean) {
        active = value
        incoming = false
        if (value) acquireAudio() else releaseAudio()
        updateMediaSession()
        publishAudioState()
    }

    override fun requestEndpoint(endpointId: String) {
        val endpoint = endpoints().firstOrNull { it.id == endpointId }
            ?: throw IllegalStateException("The selected audio endpoint is no longer available")
        selectedEndpointId = endpoint.id
        when (endpoint.type) {
            VoiceAudioEndpointType.SPEAKER -> {
                audioManager.stopBluetoothSco()
                audioManager.isBluetoothScoOn = false
                audioManager.isSpeakerphoneOn = true
            }
            VoiceAudioEndpointType.BLUETOOTH -> {
                audioManager.isSpeakerphoneOn = false
                audioManager.startBluetoothSco()
                audioManager.isBluetoothScoOn = true
            }
            else -> {
                audioManager.isSpeakerphoneOn = false
                audioManager.stopBluetoothSco()
                audioManager.isBluetoothScoOn = false
            }
        }
        publishAudioState()
    }

    override fun end() {
        incoming = false
        active = false
        selectedEndpointId = ""
        releaseAudio()
        updateMediaSession()
        publishAudioState()
    }

    override fun close() {
        end()
        audioManager.unregisterAudioDeviceCallback(deviceCallback)
        mediaSession.release()
    }

    private fun acquireAudio() {
        if (!focusOwned) {
            previousMode = audioManager.mode
            focusOwned = audioManager.requestAudioFocus(
                focusListener,
                AudioManager.STREAM_VOICE_CALL,
                AudioManager.AUDIOFOCUS_GAIN_TRANSIENT,
            ) == AudioManager.AUDIOFOCUS_REQUEST_GRANTED
        }
        audioManager.mode = AudioManager.MODE_IN_COMMUNICATION
    }

    private fun releaseAudio() {
        if (focusOwned) audioManager.abandonAudioFocus(focusListener)
        focusOwned = false
        audioManager.stopBluetoothSco()
        audioManager.isBluetoothScoOn = false
        audioManager.isSpeakerphoneOn = false
        if (audioManager.mode == AudioManager.MODE_IN_COMMUNICATION) audioManager.mode = previousMode
    }

    private fun endpoints(): List<VoiceAudioEndpoint> {
        val devices = audioManager.getDevices(AudioManager.GET_DEVICES_OUTPUTS)
        val result = linkedMapOf<String, VoiceAudioEndpoint>()
        fun add(id: String, name: String, type: VoiceAudioEndpointType) {
            result.putIfAbsent(id, VoiceAudioEndpoint(id, name, type))
        }
        if (devices.any { it.type == AudioDeviceInfo.TYPE_BUILTIN_EARPIECE }) add("legacy-earpiece", "Phone earpiece", VoiceAudioEndpointType.EARPIECE)
        add("legacy-speaker", "Speaker", VoiceAudioEndpointType.SPEAKER)
        if (devices.any { it.type in WIRED_TYPES }) add("legacy-wired", "Wired headset", VoiceAudioEndpointType.WIRED_HEADSET)
        if (devices.any { it.type in BLUETOOTH_TYPES }) add("legacy-bluetooth", "Bluetooth headset", VoiceAudioEndpointType.BLUETOOTH)
        return result.values.toList()
    }

    private fun publishAudioState() {
        val available = endpoints()
        if (selectedEndpointId.isNotBlank() && available.none { it.id == selectedEndpointId }) {
            selectedEndpointId = ""
        }
        val current = available.firstOrNull {
            audioManager.isSpeakerphoneOn && it.type == VoiceAudioEndpointType.SPEAKER
        } ?: available.firstOrNull {
            audioManager.isBluetoothScoOn && it.type == VoiceAudioEndpointType.BLUETOOTH
        } ?: available.firstOrNull { it.id == selectedEndpointId }
            ?: available.firstOrNull { it.type == VoiceAudioEndpointType.WIRED_HEADSET }
            ?: available.firstOrNull { it.type == VoiceAudioEndpointType.EARPIECE }
            ?: available.firstOrNull()
        onAudioState(PlatformAudioState(current = current, available = available))
    }

    private fun updateMediaSession() {
        val actions = PlaybackState.ACTION_PLAY or PlaybackState.ACTION_PAUSE or PlaybackState.ACTION_STOP or PlaybackState.ACTION_PLAY_PAUSE
        mediaSession.setPlaybackState(
            PlaybackState.Builder()
                .setActions(actions)
                .setState(if (active || incoming) PlaybackState.STATE_PLAYING else PlaybackState.STATE_STOPPED, 0, 1f)
                .build(),
        )
        mediaSession.isActive = active || incoming
    }

    private companion object {
        val WIRED_TYPES = setOf(AudioDeviceInfo.TYPE_WIRED_HEADSET, AudioDeviceInfo.TYPE_WIRED_HEADPHONES)
        val BLUETOOTH_TYPES = setOf(AudioDeviceInfo.TYPE_BLUETOOTH_SCO)
    }
}

package com.viti.operator.data

import android.content.Context
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.booleanPreferencesKey
import androidx.datastore.preferences.core.longPreferencesKey
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.map

private val Context.vitiDataStore by preferencesDataStore(name = "viti_settings")

class SettingsStore(private val context: Context) {
    private object Keys {
        val serverUrl = stringPreferencesKey("server_url")
        val apiKey = stringPreferencesKey("api_key")
		val remoteSimModeEnabled = booleanPreferencesKey("remote_sim_mode_enabled")
		val restoreNetworkMask = stringPreferencesKey("restore_network_mask")
        val contactSnapshotRevision = longPreferencesKey("contact_snapshot_revision")
        val contactSyncConsent = booleanPreferencesKey("contact_sync_consent")
    }

	val phoneRouting: Flow<PhoneRoutingSettings> = context.vitiDataStore.data.map { values ->
		PhoneRoutingSettings(
			remoteSimModeEnabled = values[Keys.remoteSimModeEnabled] ?: false,
			restoreNetworkMask = values[Keys.restoreNetworkMask] ?: "",
		)
	}

    val settings: Flow<ServerSettings> = context.vitiDataStore.data.map { values ->
        ServerSettings(
            serverUrl = normalizeServerUrl(values[Keys.serverUrl] ?: ServerSettings().serverUrl),
            apiKey = values[Keys.apiKey] ?: "",
        )
    }

    val contactSyncConsent: Flow<Boolean> = context.vitiDataStore.data.map { values ->
        values[Keys.contactSyncConsent] ?: false
    }

    suspend fun save(settings: ServerSettings) {
        context.vitiDataStore.edit { values ->
            values[Keys.serverUrl] = normalizeServerUrl(settings.serverUrl)
            values[Keys.apiKey] = settings.apiKey.trim()
        }
    }

    suspend fun nextContactSnapshotRevision(): Long {
        var revision = 0L
        context.vitiDataStore.edit { values ->
            revision = maxOf((values[Keys.contactSnapshotRevision] ?: 0L) + 1L, System.currentTimeMillis())
            values[Keys.contactSnapshotRevision] = revision
        }
        return revision
    }

    suspend fun setContactSyncConsent(enabled: Boolean) {
        context.vitiDataStore.edit { values -> values[Keys.contactSyncConsent] = enabled }
    }

	suspend fun savePhoneRouting(settings: PhoneRoutingSettings) {
		context.vitiDataStore.edit { values ->
			values[Keys.remoteSimModeEnabled] = settings.remoteSimModeEnabled
			values[Keys.restoreNetworkMask] = settings.restoreNetworkMask
		}
	}
}

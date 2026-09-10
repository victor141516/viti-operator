package com.viti.operator.data

import android.Manifest
import android.content.Context
import android.content.pm.PackageManager
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.telephony.PhoneStateListener
import android.telephony.ServiceState
import android.telephony.SignalStrength
import android.telephony.SubscriptionManager
import android.telephony.TelephonyCallback
import android.telephony.TelephonyManager
import androidx.annotation.RequiresApi
import androidx.core.content.ContextCompat
import java.time.Instant
import java.util.UUID

/**
 * One process-owned listener for the subscription used by [SimAuthEngine].
 * Heartbeats serialize its latest OS callbacks; they never register a listener
 * or claim that sending an HTTP request caused a new radio measurement.
 */
class CellularTelemetryMonitor private constructor(context: Context) {
    private val appContext = context.applicationContext
    private val baseTelephony = appContext.getSystemService(TelephonyManager::class.java)
    private val mainHandler = Handler(Looper.getMainLooper())
    private val lock = Any()
    private val generation = UUID.randomUUID().toString()
    private val generationStartedAt = Instant.now()
    private val snapshotGuard = CellularSnapshotGuard()
    private var sequence = 0L

    private var subscriptionId = SubscriptionManager.INVALID_SUBSCRIPTION_ID
    private var bindRevision = 0L
    private var registeredTelephony: TelephonyManager? = null
    private var registeredWithPhonePermission = false
    private var bindingActive = false
    private var modernCallback: TelephonyCallback? = null
    @Suppress("DEPRECATION")
    private var legacyListener: PhoneStateListener? = null
    private var latestService: TimedService? = null
    private var latestDataAccess: TimedDataAccess? = null
    private var latestSignal: TimedSignal? = null

    fun snapshot(): CellularObservation? = snapshotGuard.serialized {
        val telephony = baseTelephony ?: return@serialized null
        val selectedSubscription = simAuthSubscriptionId()
        val phonePermission = hasPhoneStatePermission()
        ensureBinding(selectedSubscription, phonePermission)
        val networkMeasuredAt = Instant.now()
        val selectedTelephony = if (selectedSubscription == SubscriptionManager.INVALID_SUBSCRIPTION_ID) {
            null
        } else {
            runCatching { telephony.createForSubscriptionId(selectedSubscription) }.getOrNull()
        }
        val home = selectedTelephony?.networkDetails(home = true, now = networkMeasuredAt)
            ?: CellularNetworkDetails(availability = if (selectedSubscriptionValid(selectedSubscription)) "unknown" else "unsupported")
        val serving = selectedTelephony?.networkDetails(home = false, now = networkMeasuredAt)
            ?: CellularNetworkDetails(availability = if (selectedSubscriptionValid(selectedSubscription)) "unknown" else "unsupported")
        val callbackSnapshot = synchronized(lock) {
            Triple(latestService, latestDataAccess, latestSignal)
        }
        val service = callbackSnapshot.first?.let {
            CellularServiceDetails(
                availability = "available",
                registration = serviceRegistrationName(it.state),
                roaming = it.roaming,
                measuredAt = it.at.toString(),
            )
        } ?: CellularServiceDetails(availability = protectedAvailability(selectedSubscription, phonePermission))
        val dataAccess = callbackSnapshot.second?.let {
            val technology = connectedDataTechnologyName(it.state, it.networkType)
            CellularDataAccessDetails(
                availability = if (technology.isBlank()) "unknown" else "available",
                technology = technology,
                measuredAt = if (technology.isBlank()) "" else it.at.toString(),
            )
        } ?: CellularDataAccessDetails(availability = protectedAvailability(selectedSubscription, phonePermission))
        val signal = callbackSnapshot.third?.let {
            CellularSignalDetails(
                availability = "available",
                level = it.level,
                dbm = it.dbm,
                measuredAt = it.at.toString(),
            )
        } ?: CellularSignalDetails(availability = protectedAvailability(selectedSubscription, phonePermission))
        val measuredAt = Instant.now()
        CellularObservation(
            generation = generation,
            generationStartedAt = generationStartedAt.toString(),
            sequence = ++sequence,
            measuredAt = measuredAt.toString(),
            subscriptionRole = if (selectedSubscriptionValid(selectedSubscription)) "default_data" else "none",
            homeNetwork = home,
            servingNetwork = serving,
            service = service,
            dataAccess = dataAccess,
            signal = signal,
        )
    }

    /** Re-evaluates permission/subscription binding after an OS permission result. */
    fun refreshRegistration() {
        ensureBinding(simAuthSubscriptionId(), hasPhoneStatePermission(), force = true)
    }

    private fun ensureBinding(selectedSubscription: Int, phonePermission: Boolean, force: Boolean = false) {
        val revision = synchronized(lock) {
            if (!force && selectedSubscription == subscriptionId && phonePermission == registeredWithPhonePermission && bindingActive) return
            subscriptionId = selectedSubscription
            registeredWithPhonePermission = phonePermission
            bindingActive = false
            latestService = null
            latestDataAccess = null
            latestSignal = null
            ++bindRevision
        }
        mainHandler.post { rebind(selectedSubscription, phonePermission, revision) }
    }

    @Suppress("DEPRECATION")
    private fun rebind(selectedSubscription: Int, phonePermission: Boolean, revision: Long) {
        synchronized(lock) {
            if (revision != bindRevision) return
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            modernCallback?.let { callback -> registeredTelephony?.runCatching { unregisterTelephonyCallback(callback) } }
        }
        legacyListener?.let { listener -> registeredTelephony?.runCatching { listen(listener, PhoneStateListener.LISTEN_NONE) } }
        modernCallback = null
        legacyListener = null
        registeredTelephony = null
        if (!selectedSubscriptionValid(selectedSubscription)) {
            synchronized(lock) { if (revision == bindRevision) bindingActive = true }
            return
        }
        val telephony = baseTelephony?.runCatching { createForSubscriptionId(selectedSubscription) }?.getOrNull() ?: return
        registeredTelephony = telephony
        runCatching {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
                val callback = if (phonePermission) FullTelephonyCallback(revision) else SignalOnlyTelephonyCallback(revision)
                modernCallback = callback
                telephony.registerTelephonyCallback(appContext.mainExecutor, callback)
            } else {
                val listener = LegacyTelephonyListener(revision)
                legacyListener = listener
                var events = PhoneStateListener.LISTEN_SIGNAL_STRENGTHS
                if (phonePermission) {
                    events = events or PhoneStateListener.LISTEN_SERVICE_STATE or PhoneStateListener.LISTEN_DATA_CONNECTION_STATE
                }
                telephony.listen(listener, events)
            }
            synchronized(lock) { if (revision == bindRevision) bindingActive = true }
        }.onFailure {
            modernCallback = null
            legacyListener = null
            registeredTelephony = null
            synchronized(lock) { if (revision == bindRevision) bindingActive = false }
        }
    }

    private fun recordService(revision: Long, serviceState: ServiceState) = synchronized(lock) {
        if (revision == bindRevision) {
            latestService = TimedService(serviceState.state, serviceState.roaming, Instant.now())
        }
    }

    private fun recordDataAccess(revision: Long, state: Int, networkType: Int) = synchronized(lock) {
        if (revision == bindRevision) latestDataAccess = TimedDataAccess(state, networkType, Instant.now())
    }

    private fun recordSignal(revision: Long, signalStrength: SignalStrength) = synchronized(lock) {
        if (revision != bindRevision) return@synchronized
        val level = signalStrength.level.takeIf { it in 0..4 }
        val dbm = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            signalStrength.cellSignalStrengths
                .mapNotNull { strength -> strength.dbm.takeIf { it in -200..0 } }
                .maxOrNull()
        } else {
            null
        }
        if (level != null || dbm != null) latestSignal = TimedSignal(level, dbm, Instant.now())
    }

    @RequiresApi(Build.VERSION_CODES.S)
    private inner class FullTelephonyCallback(private val revision: Long) : TelephonyCallback(),
        TelephonyCallback.ServiceStateListener,
        TelephonyCallback.DataConnectionStateListener,
        TelephonyCallback.SignalStrengthsListener {
        override fun onServiceStateChanged(serviceState: ServiceState) = recordService(revision, serviceState)
        override fun onDataConnectionStateChanged(state: Int, networkType: Int) = recordDataAccess(revision, state, networkType)
        override fun onSignalStrengthsChanged(signalStrength: SignalStrength) = recordSignal(revision, signalStrength)
    }

    @RequiresApi(Build.VERSION_CODES.S)
    private inner class SignalOnlyTelephonyCallback(private val revision: Long) : TelephonyCallback(),
        TelephonyCallback.SignalStrengthsListener {
        override fun onSignalStrengthsChanged(signalStrength: SignalStrength) = recordSignal(revision, signalStrength)
    }

    @Suppress("DEPRECATION")
    private inner class LegacyTelephonyListener(private val revision: Long) : PhoneStateListener() {
        override fun onServiceStateChanged(serviceState: ServiceState?) {
            serviceState?.let { recordService(revision, it) }
        }

        override fun onDataConnectionStateChanged(state: Int, networkType: Int) = recordDataAccess(revision, state, networkType)
        override fun onSignalStrengthsChanged(signalStrength: SignalStrength?) {
            signalStrength?.let { recordSignal(revision, it) }
        }
    }

    private fun TelephonyManager.networkDetails(home: Boolean, now: Instant): CellularNetworkDetails {
        val operator = runCatching { if (home) simOperatorName else networkOperatorName }.getOrDefault("").trim()
        val numeric = runCatching { if (home) simOperator else networkOperator }.getOrDefault("").trim()
        val (mcc, mnc) = splitMccMnc(numeric)
        val available = operator.isNotBlank() || mcc.isNotBlank()
        return CellularNetworkDetails(
            availability = if (available) "available" else "unknown",
            operator = operator.take(80),
            mcc = mcc,
            mnc = mnc,
            measuredAt = if (available) now.toString() else "",
        )
    }

    private fun protectedAvailability(selectedSubscription: Int, phonePermission: Boolean): String = when {
        !selectedSubscriptionValid(selectedSubscription) -> "unsupported"
        !phonePermission -> "permission_denied"
        else -> "unknown"
    }

    private fun hasPhoneStatePermission(): Boolean =
        ContextCompat.checkSelfPermission(appContext, Manifest.permission.READ_PHONE_STATE) == PackageManager.PERMISSION_GRANTED

    private data class TimedService(val state: Int, val roaming: Boolean, val at: Instant)
    private data class TimedDataAccess(val state: Int, val networkType: Int, val at: Instant)
    private data class TimedSignal(val level: Int?, val dbm: Int?, val at: Instant)

    companion object {
        @Volatile private var instance: CellularTelemetryMonitor? = null

        fun get(context: Context): CellularTelemetryMonitor = instance ?: synchronized(this) {
            instance ?: CellularTelemetryMonitor(context).also { instance = it }
        }

        fun currentSnapshot(): CellularObservation? = instance?.snapshot()
    }
}

internal class CellularSnapshotGuard {
    private val lock = Any()

    fun <T> serialized(block: () -> T): T = synchronized(lock, block)
}

internal fun simAuthSubscriptionId(): Int = SubscriptionManager.getDefaultDataSubscriptionId()

internal fun selectedSubscriptionValid(subscriptionId: Int): Boolean =
    subscriptionId != SubscriptionManager.INVALID_SUBSCRIPTION_ID

internal fun splitMccMnc(value: String): Pair<String, String> {
    val digits = value.trim()
    if (digits.length !in 5..6 || digits.any { !it.isDigit() }) return "" to ""
    return digits.take(3) to digits.drop(3)
}

internal fun serviceRegistrationName(state: Int): String = when (state) {
    ServiceState.STATE_IN_SERVICE -> "in_service"
    ServiceState.STATE_OUT_OF_SERVICE -> "out_of_service"
    ServiceState.STATE_EMERGENCY_ONLY -> "emergency_only"
    ServiceState.STATE_POWER_OFF -> "power_off"
    else -> ""
}

internal fun networkTechnologyName(networkType: Int): String = when (networkType) {
    TelephonyManager.NETWORK_TYPE_GPRS -> "GPRS"
    TelephonyManager.NETWORK_TYPE_EDGE -> "EDGE"
    TelephonyManager.NETWORK_TYPE_UMTS -> "UMTS"
    TelephonyManager.NETWORK_TYPE_CDMA -> "CDMA"
    TelephonyManager.NETWORK_TYPE_EVDO_0 -> "EVDO 0"
    TelephonyManager.NETWORK_TYPE_EVDO_A -> "EVDO A"
    TelephonyManager.NETWORK_TYPE_1xRTT -> "1xRTT"
    TelephonyManager.NETWORK_TYPE_HSDPA -> "HSDPA"
    TelephonyManager.NETWORK_TYPE_HSUPA -> "HSUPA"
    TelephonyManager.NETWORK_TYPE_HSPA -> "HSPA"
    TelephonyManager.NETWORK_TYPE_IDEN -> "iDEN"
    TelephonyManager.NETWORK_TYPE_EVDO_B -> "EVDO B"
    TelephonyManager.NETWORK_TYPE_LTE -> "LTE"
    TelephonyManager.NETWORK_TYPE_EHRPD -> "eHRPD"
    TelephonyManager.NETWORK_TYPE_HSPAP -> "HSPA+"
    TelephonyManager.NETWORK_TYPE_GSM -> "GSM"
    TelephonyManager.NETWORK_TYPE_TD_SCDMA -> "TD-SCDMA"
    TelephonyManager.NETWORK_TYPE_IWLAN -> "IWLAN"
    TelephonyManager.NETWORK_TYPE_NR -> "5G NR"
    else -> ""
}

internal fun connectedDataTechnologyName(state: Int, networkType: Int): String =
    if (state == TelephonyManager.DATA_CONNECTED) networkTechnologyName(networkType) else ""

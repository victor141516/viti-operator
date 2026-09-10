package com.viti.operator.data

import android.telephony.ServiceState
import android.telephony.TelephonyManager
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.CopyOnWriteArrayList

class CellularTelemetryTest {
    @Test
    fun `snapshot guard preserves capture and sequence order under concurrency`() {
        val guard = CellularSnapshotGuard()
        val firstEntered = CountDownLatch(1)
        val releaseFirst = CountDownLatch(1)
        val order = CopyOnWriteArrayList<String>()
        val first = Thread {
            guard.serialized {
                firstEntered.countDown()
                releaseFirst.await(2, TimeUnit.SECONDS)
                order += "first"
            }
        }
        val second = Thread {
            firstEntered.await(2, TimeUnit.SECONDS)
            guard.serialized { order += "second" }
        }
        first.start()
        second.start()
        assertTrue(firstEntered.await(2, TimeUnit.SECONDS))
        releaseFirst.countDown()
        first.join(2_000)
        second.join(2_000)
        assertEquals(listOf("first", "second"), order.toList())
    }

    @Test
    fun `identity SIM state follows the exact observed authentication subscription`() {
        val base = ClientIdentity("android-one", "Phone", hasSim = false)
        assertTrue(base.withCellularObservation(observationWithRole("default_data")).hasSim)
        assertFalse(base.copy(hasSim = true).withCellularObservation(observationWithRole("none")).hasSim)
    }

    @Test
    fun `MCC and MNC retain two or three digit MNC`() {
        assertEquals("214" to "07", splitMccMnc("21407"))
        assertEquals("310" to "260", splitMccMnc("310260"))
        assertEquals("" to "", splitMccMnc("not-a-network"))
    }

    @Test
    fun `network types use concise user-facing names`() {
        assertEquals("LTE", networkTechnologyName(TelephonyManager.NETWORK_TYPE_LTE))
        assertEquals("5G NR", networkTechnologyName(TelephonyManager.NETWORK_TYPE_NR))
        assertEquals("IWLAN", networkTechnologyName(TelephonyManager.NETWORK_TYPE_IWLAN))
        assertEquals("", networkTechnologyName(TelephonyManager.NETWORK_TYPE_UNKNOWN))
    }

    @Test
    fun `data technology is exposed only while Android reports a connected data session`() {
        assertEquals(
            "LTE",
            connectedDataTechnologyName(TelephonyManager.DATA_CONNECTED, TelephonyManager.NETWORK_TYPE_LTE),
        )
        assertEquals(
            "",
            connectedDataTechnologyName(TelephonyManager.DATA_DISCONNECTED, TelephonyManager.NETWORK_TYPE_LTE),
        )
        assertEquals(
            "",
            connectedDataTechnologyName(TelephonyManager.DATA_CONNECTING, TelephonyManager.NETWORK_TYPE_NR),
        )
    }

    @Test
    fun `service registration does not turn unknown values into success`() {
        assertEquals("in_service", serviceRegistrationName(ServiceState.STATE_IN_SERVICE))
        assertEquals("out_of_service", serviceRegistrationName(ServiceState.STATE_OUT_OF_SERVICE))
        assertEquals("emergency_only", serviceRegistrationName(ServiceState.STATE_EMERGENCY_ONLY))
        assertEquals("power_off", serviceRegistrationName(ServiceState.STATE_POWER_OFF))
        assertEquals("", serviceRegistrationName(999))
    }

    private fun observationWithRole(role: String) = CellularObservation(
        generation = "process-one",
        generationStartedAt = "2026-01-01T00:00:00Z",
        sequence = 1,
        measuredAt = "2026-01-01T00:00:00Z",
        subscriptionRole = role,
        homeNetwork = CellularNetworkDetails(),
        servingNetwork = CellularNetworkDetails(),
        service = CellularServiceDetails(),
        dataAccess = CellularDataAccessDetails(),
        signal = CellularSignalDetails(),
    )
}

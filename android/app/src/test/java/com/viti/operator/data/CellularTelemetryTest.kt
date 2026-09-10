package com.viti.operator.data

import android.telephony.ServiceState
import android.telephony.TelephonyManager
import org.junit.Assert.assertEquals
import org.junit.Test

class CellularTelemetryTest {
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
}

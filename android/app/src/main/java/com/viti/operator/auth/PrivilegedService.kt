package com.viti.operator.auth

import android.content.Context
import androidx.annotation.Keep
import org.json.JSONObject
import java.io.IOException

class PrivilegedService() : IPrivilegedService.Stub() {
    @Keep
    constructor(@Suppress("UNUSED_PARAMETER") context: Context) : this()

    override fun destroy() {
        System.exit(0)
    }

    override fun grantSimAuthentication(packageName: String): String {
        require(packageName.matches(Regex("[A-Za-z0-9_.]+"))) { "Invalid package name" }
        val process = ProcessBuilder(
            "appops",
            "set",
            packageName,
            "USE_ICC_AUTH_WITH_DEVICE_IDENTIFIER",
            "allow",
        ).start()
        val output = process.inputStream.bufferedReader().use { it.readText() }
        val error = process.errorStream.bufferedReader().use { it.readText() }
        val exit = process.waitFor()
        if (exit != 0) throw IOException(error.ifBlank { output.ifBlank { "appops exited with code $exit" } })
        return "SIM authentication permission prepared"
    }

    override fun enableRemoteSimMode(): String {
        val previousMask = parseNetworkMask(runPhoneCommand("get-allowed-network-types-for-users"))
        try {
            runPhoneCommand("set-allowed-network-types-for-users", LTE_NR_ONLY_MASK)
            runPhoneCommand("ims", "disable")
        } catch (error: Throwable) {
            runCatching { runPhoneCommand("set-allowed-network-types-for-users", previousMask) }
            throw error
        }
        return JSONObject()
            .put("previous_network_mask", previousMask)
            .put("network_mask", LTE_NR_ONLY_MASK)
            .put("ims_enabled", false)
            .toString()
    }

    override fun disableRemoteSimMode(restoreNetworkMask: String): String {
        require(restoreNetworkMask.matches(Regex("[01]{10,}"))) { "Invalid network restore mask" }
        runPhoneCommand("set-allowed-network-types-for-users", restoreNetworkMask)
        runPhoneCommand("ims", "enable")
        return JSONObject()
            .put("network_mask", restoreNetworkMask)
            .put("ims_enabled", true)
            .toString()
    }

    private fun runPhoneCommand(vararg args: String): String {
        val process = ProcessBuilder(listOf("cmd", "phone") + args).start()
        val output = process.inputStream.bufferedReader().use { it.readText() }
        val error = process.errorStream.bufferedReader().use { it.readText() }
        val exit = process.waitFor()
        if (exit != 0) {
            throw IOException(error.ifBlank { output.ifBlank { "cmd phone exited with code $exit" } })
        }
        return output
    }

	private fun parseNetworkMask(output: String): String {
		Regex("[01]{10,}").find(output)?.value?.let { return it }
		val bitByLabel = mapOf(
			"GPRS" to 0,
			"EDGE" to 1,
			"UMTS" to 2,
			"CDMA" to 3,
			"CDMA - EvDo rev. 0" to 4,
			"CDMA - EvDo rev. A" to 5,
			"CDMA - 1xRTT" to 6,
			"HSDPA" to 7,
			"HSUPA" to 8,
			"HSPA" to 9,
			"iDEN" to 10,
			"CDMA - EvDo rev. B" to 11,
			"LTE" to 12,
			"CDMA - eHRPD" to 13,
			"HSPA+" to 14,
			"GSM" to 15,
			"TD_SCDMA" to 16,
			"IWLAN" to 17,
			"LTE_CA" to 18,
			"NR" to 19,
		)
		var mask = 0L
		var recognized = 0
		output.trim().split('|').map(String::trim).forEach { label ->
			bitByLabel[label]?.let { bit ->
				mask = mask or (1L shl bit)
				recognized++
			}
		}
		if (recognized == 0) throw IOException("Android did not report the currently allowed networks: ${output.trim()}")
		return java.lang.Long.toBinaryString(mask).padStart(20, '0')
	}

    companion object {
        // AOSP TelephonyShellCommand's documented NR|LTE mask. This removes
        // GSM/WCDMA/CDMA/EVDO fallback while preserving packet data on 4G/5G.
        private const val LTE_NR_ONLY_MASK = "11000001000000000000"
    }
}

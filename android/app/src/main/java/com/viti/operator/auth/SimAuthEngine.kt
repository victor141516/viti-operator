package com.viti.operator.auth

import android.content.Context
import android.telephony.SubscriptionManager
import android.telephony.TelephonyManager
import android.util.Base64
import com.viti.operator.data.AuthChallenge
import com.viti.operator.data.AuthMaterial
import java.io.IOException

class SimAuthEngine(context: Context) {
    private val telephony = context.getSystemService(TelephonyManager::class.java)

    fun authenticate(challenge: AuthChallenge): AuthMaterial {
        val rand = Base64.decode(challenge.rand, Base64.NO_WRAP)
        val autn = Base64.decode(challenge.autn, Base64.NO_WRAP)
        require(rand.size == 16 && autn.size == 16) { "RAND and AUTN must be 16 bytes" }

        val subscriptionId = SubscriptionManager.getDefaultDataSubscriptionId()
        if (subscriptionId == SubscriptionManager.INVALID_SUBSCRIPTION_ID) {
            throw IOException("There is no active data subscription")
        }
        // 3GPP TS 31.102 AUTHENTICATE input is length-prefixed:
        // L_RAND | RAND | L_AUTN | AUTN. Passing RAND | AUTN directly makes
        // Android's radio layer reject an otherwise valid AKA challenge.
        val formatted = byteArrayOf(rand.size.toByte()) + rand + byteArrayOf(autn.size.toByte()) + autn
        val request = Base64.encodeToString(formatted, Base64.NO_WRAP)
        val response = telephony
            .createForSubscriptionId(subscriptionId)
            .getIccAuthentication(
                TelephonyManager.APPTYPE_USIM,
                TelephonyManager.AUTHTYPE_EAP_AKA,
                request,
            ) ?: throw IOException("The SIM did not return AKA material")

        val bytes = Base64.decode(response, Base64.DEFAULT)
        return parseResponse(bytes)
    }

    private fun parseResponse(bytes: ByteArray): AuthMaterial {
        if (bytes.isEmpty()) throw IOException("Empty AKA response")
        var offset = 1
        fun field(): ByteArray {
            if (offset >= bytes.size) throw IOException("Truncated AKA response")
            val length = bytes[offset++].toInt() and 0xff
            if (offset + length > bytes.size) throw IOException("Truncated AKA field")
            return bytes.copyOfRange(offset, offset + length).also { offset += length }
        }
        return when (bytes[0].toInt() and 0xff) {
            0xdb -> AuthMaterial(
                res = field().rawBase64(),
                ck = field().rawBase64(),
                ik = field().rawBase64(),
            )
            0xdc -> AuthMaterial(auts = field().rawBase64())
            else -> throw IOException("Unknown AKA tag: 0x${(bytes[0].toInt() and 0xff).toString(16)}")
        }
    }

    private fun ByteArray.rawBase64(): String =
        Base64.encodeToString(this, Base64.NO_WRAP or Base64.NO_PADDING)
}

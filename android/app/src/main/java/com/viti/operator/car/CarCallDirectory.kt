package com.viti.operator.car

import com.viti.operator.data.Call
import com.viti.operator.data.PhoneContact
import com.viti.operator.data.SessionCountryDetection

internal data class CarDialTarget(
    val name: String,
    val number: String,
)

internal suspend fun normalizeCarDialTarget(
    target: CarDialTarget,
    detection: SessionCountryDetection,
    normalize: suspend (number: String, country: String) -> String,
): CarDialTarget {
    val number = target.number.trim()
    val country = detection.country.takeUnless { number.startsWith("+") }.orEmpty()
    return target.copy(number = normalize(number, country))
}

/** Produces a bounded, deterministic driving-safe directory with no free-form dialer. */
internal fun buildCarDialTargets(
    calls: List<Call>,
    contacts: List<PhoneContact>,
    limit: Int = 24,
): List<CarDialTarget> {
    if (limit <= 0) return emptyList()
    val result = mutableListOf<CarDialTarget>()
    val seen = mutableSetOf<String>()

    fun add(name: String, number: String) {
        val trimmed = number.trim()
        val key = trimmed.filter(Char::isDigit).ifBlank { trimmed.lowercase() }
        if (trimmed.isBlank() || !seen.add(key) || result.size >= limit) return
        result += CarDialTarget(name.trim().ifBlank { trimmed }, trimmed)
    }

    calls.forEach { add(it.contactName, it.peer) }
    contacts
        .sortedWith(compareBy<PhoneContact>({ it.name.lowercase() }, { it.number }))
        .forEach { add(it.name, it.number) }
    return result
}

internal fun limitCarDialTargets(targets: List<CarDialTarget>, hostLimit: Int): List<CarDialTarget> =
    targets.take(hostLimit.coerceAtLeast(0))

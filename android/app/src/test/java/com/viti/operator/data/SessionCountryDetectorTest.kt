package com.viti.operator.data

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Test
import kotlinx.coroutines.runBlocking
import java.time.Instant

class SessionCountryDetectorTest {
    @Test
    fun servingNetworkIsTheOnlySingleStrongSignal() {
        val result = resolveSessionCountry(
            "session-a",
            SessionCountrySignals(servingCountry = "gb", ipCountry = "US", timezoneCountry = "US", localeCountry = "US"),
        )
        assertEquals(SessionCountryDetection("session-a", "GB", "serving-network"), result)
    }

    @Test
    fun matchingIpAndTimezoneResolveCurrentCountry() {
        val result = resolveSessionCountry(
            "session-b",
            SessionCountrySignals(ipCountry = "ES", timezoneCountry = "ES", localeCountry = "US"),
        )
        assertEquals(SessionCountryDetection("session-b", "ES", "ip+timezone"), result)
    }

    @Test
    fun vpnConflictAndMissingIpFailClosed() {
        assertEquals(
            "ip-timezone-conflict",
            resolveSessionCountry("session-c", SessionCountrySignals(ipCountry = "US", timezoneCountry = "ES", localeCountry = "ES")).reason,
        )
        assertEquals(
            "ip-unavailable",
            resolveSessionCountry("session-d", SessionCountrySignals(timezoneCountry = "ES", localeCountry = "ES")).reason,
        )
    }

    @Test
    fun onlyCanonicalInternationalNumbersCanBypassManagerNormalization() {
        assertEquals(true, isCanonicalE164("+34612345678"))
        assertFalse(isCanonicalE164("+34 612 345 678"))
        assertFalse(isCanonicalE164("612345678"))
        assertFalse(isCanonicalE164("*44"))
    }

    @Test
    fun freshSessionGenerationCanResolveAChangedCountry() {
        val first = resolveSessionCountry(
            "session-in-spain",
            SessionCountrySignals(ipCountry = "ES", timezoneCountry = "ES"),
        )
        val next = resolveSessionCountry(
            "session-in-britain",
            SessionCountrySignals(ipCountry = "GB", timezoneCountry = "GB"),
        )

        assertEquals(SessionCountryDetection("session-in-spain", "ES", "ip+timezone"), first)
        assertEquals(SessionCountryDetection("session-in-britain", "GB", "ip+timezone"), next)
    }

    @Test
    fun initialPendingCellularObservationWaitsForTheStrongSignal() = runBlocking {
        val observations = ArrayDeque(
            listOf(
                ServingCountryObservation(pending = true),
                ServingCountryObservation(pending = true),
                ServingCountryObservation(country = "US"),
            ),
        )
        var fallbackRequested = false

        val result = detectSessionCountryWithProbes(
            sessionGeneration = "cold-start",
            servingAttempts = 3,
            servingRetryDelayMs = 0,
            servingProbe = { observations.removeFirst() },
            fallbackSignals = {
                fallbackRequested = true
                SessionCountrySignals(ipCountry = "RU", timezoneCountry = "RU")
            },
            pause = {},
        )

        assertEquals(SessionCountryDetection("cold-start", "US", "serving-network"), result)
        assertFalse(fallbackRequested)
    }

    @Test
    fun unresolvedCellularInitializationFailsClosedInsteadOfUsingWeakSignals() = runBlocking {
        var fallbackRequested = false
        val result = detectSessionCountryWithProbes(
            sessionGeneration = "cold-start-timeout",
            servingAttempts = 2,
            servingRetryDelayMs = 0,
            servingProbe = { ServingCountryObservation(pending = true) },
            fallbackSignals = {
                fallbackRequested = true
                SessionCountrySignals(ipCountry = "RU", timezoneCountry = "RU")
            },
            pause = {},
        )

        assertEquals("serving-network-pending", result.reason)
        assertEquals("", result.country)
        assertFalse(fallbackRequested)
    }

    @Test
    fun deniedOptionalPhoneStatePermissionIsTerminalAndAllowsConservativeFallback() = runBlocking {
        val denied = classifyServingCountryObservation(
            CellularServiceDetails(availability = "permission_denied"),
            country = "",
        )
        val initializing = classifyServingCountryObservation(
            CellularServiceDetails(availability = "unknown"),
            country = "",
        )
        var fallbackRequested = false

        val result = detectSessionCountryWithProbes(
            sessionGeneration = "no-phone-state-permission",
            servingProbe = { denied },
            fallbackSignals = {
                fallbackRequested = true
                SessionCountrySignals(ipCountry = "ES", timezoneCountry = "ES")
            },
            pause = {},
        )

        assertEquals(ServingCountryObservation(), denied)
        assertEquals(ServingCountryObservation(pending = true), initializing)
        assertEquals(SessionCountryDetection("no-phone-state-permission", "ES", "ip+timezone"), result)
        assertEquals(true, fallbackRequested)
    }

    @Test
    fun currentInServiceObservationStillWinsWhenFresh() {
        val now = Instant.parse("2026-09-10T10:00:00Z")
        val result = classifyServingCountryObservation(
            CellularServiceDetails(
                availability = "available",
                registration = "in_service",
                measuredAt = now.minusSeconds(30).toString(),
            ),
            country = "us",
            now = now,
        )

        assertEquals(ServingCountryObservation("US"), result)
    }
}

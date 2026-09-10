package com.viti.operator.car

import com.viti.operator.data.Call
import com.viti.operator.data.PhoneContact
import com.viti.operator.data.SessionCountryDetection
import org.junit.Assert.assertEquals
import org.junit.Test
import kotlinx.coroutines.runBlocking

class CarCallDirectoryTest {
    @Test
    fun recentCallsComeFirstAndDuplicateContactsAreRemoved() {
        val calls = listOf(
            call(peer = "+34 600 000 001", contactName = "Recent Alice"),
            call(peer = "+34 600 000 002", contactName = ""),
        )
        val contacts = listOf(
            contact(name = "Alice", number = "+34600000001"),
            contact(name = "Charlie", number = "+34600000003"),
            contact(name = "Bob", number = "+34600000002"),
        )

        assertEquals(
            listOf(
                CarDialTarget("Recent Alice", "+34 600 000 001"),
                CarDialTarget("+34 600 000 002", "+34 600 000 002"),
                CarDialTarget("Charlie", "+34600000003"),
            ),
            buildCarDialTargets(calls, contacts),
        )
    }

    @Test
    fun directoryIsBoundedAndRejectsNonPositiveLimits() {
        val contacts = listOf(
            contact(name = "A", number = "+34600000001"),
            contact(name = "B", number = "+34600000002"),
            contact(name = "C", number = "+34600000003"),
        )

        assertEquals(listOf(CarDialTarget("A", "+34600000001")), buildCarDialTargets(emptyList(), contacts, limit = 1))
        assertEquals(emptyList<CarDialTarget>(), buildCarDialTargets(emptyList(), contacts, limit = 0))
    }

    @Test
    fun renderedDirectoryRespectsTheHostContentLimit() {
        val targets = (1..10).map { CarDialTarget("Contact $it", "+3460000000$it") }

        assertEquals(targets.take(6), limitCarDialTargets(targets, hostLimit = 6))
        assertEquals(emptyList<CarDialTarget>(), limitCarDialTargets(targets, hostLimit = 0))
    }

    @Test
    fun androidAutoNormalizesNationalTargetsBeforeDialing() = runBlocking {
        var request = "" to ""
        val normalized = normalizeCarDialTarget(
            CarDialTarget("Alice", "612 345 678"),
            SessionCountryDetection("car-session", "ES", "serving-network"),
        ) { number, country ->
            request = number to country
            "+34612345678"
        }

        assertEquals("612 345 678" to "ES", request)
        assertEquals(CarDialTarget("Alice", "+34612345678"), normalized)
    }

    @Test
    fun androidAutoDoesNotApplySessionCountryToExplicitInternationalTargets() = runBlocking {
        var requestedCountry = "not-called"
        normalizeCarDialTarget(
            CarDialTarget("Alice", "+34 612 345 678"),
            SessionCountryDetection("car-session", "US", "serving-network"),
        ) { _, country ->
            requestedCountry = country
            "+34612345678"
        }

        assertEquals("", requestedCountry)
    }

    private fun call(peer: String, contactName: String) = Call(
        id = peer,
        direction = "outgoing",
        peer = peer,
        state = "ended",
        startedAt = "2026-09-10T10:00:00Z",
        endedAt = "2026-09-10T10:01:00Z",
        contactName = contactName,
    )

    private fun contact(name: String, number: String) = PhoneContact(
        id = number,
        name = name,
        number = number,
    )
}

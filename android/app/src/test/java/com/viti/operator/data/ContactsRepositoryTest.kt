package com.viti.operator.data

import org.junit.Assert.assertEquals
import org.junit.Test

class ContactsRepositoryTest {
    @Test
    fun flattenedContactsPreserveEveryNumberAndAlternativeName() {
        val snapshot = DeviceContactsSnapshot(
            region = "ES",
            contacts = listOf(
                DeviceContact(
                    id = "42",
                    names = listOf("Alice", "Alicia"),
                    numbers = listOf(
                        DeviceContactNumber("612 34 56 78", "+34612345678", "Mobile"),
                        DeviceContactNumber("91 123 45 67", "+34911234567", "Work"),
                    ),
                ),
            ),
        )

        val contacts = snapshot.asPhoneContacts()

        assertEquals(listOf("+34612345678", "+34911234567"), contacts.map(PhoneContact::number))
        assertEquals(listOf("Alicia"), contacts.first().alternativeNames)
        assertEquals(listOf("42:0", "42:1"), contacts.map(PhoneContact::id))
    }
}

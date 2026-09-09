package com.viti.operator.data

import android.content.Context
import android.provider.ContactsContract

data class PhoneContact(val id: Long, val name: String, val number: String)

class ContactsRepository(private val context: Context) {
    fun load(): List<PhoneContact> {
        val projection = arrayOf(
            ContactsContract.CommonDataKinds.Phone.CONTACT_ID,
            ContactsContract.CommonDataKinds.Phone.DISPLAY_NAME_PRIMARY,
            ContactsContract.CommonDataKinds.Phone.NORMALIZED_NUMBER,
            ContactsContract.CommonDataKinds.Phone.NUMBER,
        )
        val contacts = mutableListOf<PhoneContact>()
        context.contentResolver.query(
            ContactsContract.CommonDataKinds.Phone.CONTENT_URI,
            projection,
            null,
            null,
            ContactsContract.CommonDataKinds.Phone.DISPLAY_NAME_PRIMARY + " COLLATE LOCALIZED ASC",
        )?.use { cursor ->
            val idIndex = cursor.getColumnIndexOrThrow(ContactsContract.CommonDataKinds.Phone.CONTACT_ID)
            val nameIndex = cursor.getColumnIndexOrThrow(ContactsContract.CommonDataKinds.Phone.DISPLAY_NAME_PRIMARY)
            val normalizedIndex = cursor.getColumnIndexOrThrow(ContactsContract.CommonDataKinds.Phone.NORMALIZED_NUMBER)
            val numberIndex = cursor.getColumnIndexOrThrow(ContactsContract.CommonDataKinds.Phone.NUMBER)
            while (cursor.moveToNext()) {
                val number = cursor.getString(normalizedIndex)
                    ?: cursor.getString(numberIndex)?.filter { it == '+' || it.isDigit() }
                    ?: continue
                if (number.isBlank()) continue
                contacts += PhoneContact(
                    id = cursor.getLong(idIndex),
                    name = cursor.getString(nameIndex).orEmpty().ifBlank { number },
                    number = number,
                )
            }
        }
        return contacts.distinctBy { it.id to it.number }
    }
}

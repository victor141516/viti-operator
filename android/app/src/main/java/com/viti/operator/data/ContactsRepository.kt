package com.viti.operator.data

import android.content.Context
import android.provider.ContactsContract
import android.provider.ContactsContract.CommonDataKinds.Phone
import android.telephony.TelephonyManager
import java.util.Locale

data class PhoneContact(
    val id: String,
    val name: String,
    val number: String,
    val alternativeNames: List<String> = emptyList(),
    val sourceNames: List<String> = emptyList(),
)

data class DeviceContactNumber(val original: String, val normalized: String, val label: String)
data class DeviceContact(val id: String, val names: List<String>, val numbers: List<DeviceContactNumber>)
data class DeviceContactsSnapshot(val region: String, val contacts: List<DeviceContact>)

class ContactsRepository(private val context: Context) {
    fun load(): DeviceContactsSnapshot {
        val projection = arrayOf(
            Phone.CONTACT_ID,
            Phone.DISPLAY_NAME_PRIMARY,
            ContactsContract.Contacts.DISPLAY_NAME_ALTERNATIVE,
            Phone.NORMALIZED_NUMBER,
            Phone.NUMBER,
            Phone.TYPE,
            Phone.LABEL,
        )
        data class ContactBuilder(val names: LinkedHashSet<String> = linkedSetOf(), val numbers: MutableList<DeviceContactNumber> = mutableListOf())
        val contacts = linkedMapOf<Long, ContactBuilder>()
        context.contentResolver.query(
            Phone.CONTENT_URI,
            projection,
            null,
            null,
            Phone.DISPLAY_NAME_PRIMARY + " COLLATE LOCALIZED ASC",
        )?.use { cursor ->
            val idIndex = cursor.getColumnIndexOrThrow(Phone.CONTACT_ID)
            val nameIndex = cursor.getColumnIndexOrThrow(Phone.DISPLAY_NAME_PRIMARY)
            val alternativeNameIndex = cursor.getColumnIndexOrThrow(ContactsContract.Contacts.DISPLAY_NAME_ALTERNATIVE)
            val normalizedIndex = cursor.getColumnIndexOrThrow(Phone.NORMALIZED_NUMBER)
            val numberIndex = cursor.getColumnIndexOrThrow(Phone.NUMBER)
            val typeIndex = cursor.getColumnIndexOrThrow(Phone.TYPE)
            val labelIndex = cursor.getColumnIndexOrThrow(Phone.LABEL)
            while (cursor.moveToNext()) {
                val original = cursor.getString(numberIndex)?.trim().orEmpty()
                val normalized = cursor.getString(normalizedIndex)?.trim().orEmpty()
                if (original.isBlank() && normalized.isBlank()) continue
                val builder = contacts.getOrPut(cursor.getLong(idIndex), ::ContactBuilder)
                listOf(cursor.getString(nameIndex), cursor.getString(alternativeNameIndex))
                    .mapNotNull { it?.trim()?.takeIf(String::isNotBlank) }
                    .forEach(builder.names::add)
                val label = Phone.getTypeLabel(context.resources, cursor.getInt(typeIndex), cursor.getString(labelIndex)).toString()
                builder.numbers += DeviceContactNumber(original = original.ifBlank { normalized }, normalized = normalized, label = label)
            }
        }
        val region = runCatching {
            (context.getSystemService(Context.TELEPHONY_SERVICE) as TelephonyManager).let { manager ->
                manager.networkCountryIso.ifBlank { manager.simCountryIso }
            }
        }.getOrNull().orEmpty().ifBlank { Locale.getDefault().country }.uppercase(Locale.ROOT)
        return DeviceContactsSnapshot(
            region = region.takeIf { it.length == 2 }.orEmpty(),
            contacts = contacts.map { (id, value) ->
                DeviceContact(
                    id = id.toString(),
                    names = value.names.toList(),
                    numbers = value.numbers.distinctBy { it.normalized.ifBlank { it.original } },
                )
            },
        )
    }
}

fun DeviceContactsSnapshot.asPhoneContacts(): List<PhoneContact> = contacts.flatMap { contact ->
    val displayName = contact.names.firstOrNull().orEmpty()
    contact.numbers.mapIndexed { index, number ->
        PhoneContact(
            id = "${contact.id}:$index",
            name = displayName.ifBlank { number.original },
            number = number.normalized.ifBlank { number.original },
            alternativeNames = contact.names.drop(1),
        )
    }
}

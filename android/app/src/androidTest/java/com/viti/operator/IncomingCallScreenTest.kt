package com.viti.operator

import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.assertIsNotEnabled
import androidx.compose.ui.test.junit4.v2.createComposeRule
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import com.viti.operator.ui.VitiTheme
import org.junit.Assert.assertEquals
import org.junit.Rule
import org.junit.Test

class IncomingCallScreenTest {
    @get:Rule
    val compose = createComposeRule()

    @Test
    fun callIdentityAndBothActionsAreImmediatelyAvailable() {
        var answered = 0
        var declined = 0
        compose.setContent {
            VitiTheme {
                IncomingCallScreen(
                    peer = "Ada Lovelace",
                    detail = "+34 600 000 000",
                    busy = false,
                    notice = "",
                    onAnswer = { answered += 1 },
                    onDecline = { declined += 1 },
                )
            }
        }

        compose.onNodeWithText("Ada Lovelace").assertIsDisplayed()
        compose.onNodeWithText("+34 600 000 000").assertIsDisplayed()
        compose.onNodeWithText("Answer").performClick()
        compose.onNodeWithText("Decline").performClick()
        assertEquals(1, answered)
        assertEquals(1, declined)
    }

    @Test
    fun actionsAreDisabledAfterOneIsSubmitted() {
        compose.setContent {
            VitiTheme {
                IncomingCallScreen(
                    peer = "Private number",
                    detail = "Incoming service call",
                    busy = true,
                    notice = "Answering…",
                    onAnswer = {},
                    onDecline = {},
                )
            }
        }

        compose.onNodeWithText("Answer").assertIsNotEnabled()
        compose.onNodeWithText("Decline").assertIsNotEnabled()
        compose.onNodeWithText("Answering…").assertIsDisplayed()
    }
}

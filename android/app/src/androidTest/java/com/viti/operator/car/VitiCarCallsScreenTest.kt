package com.viti.operator.car

import androidx.car.app.model.ListTemplate
import androidx.car.app.model.MessageTemplate
import androidx.car.app.model.Row
import androidx.car.app.testing.TestCarContext
import androidx.test.core.app.ApplicationProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import com.viti.operator.voice.VoiceCallSnapshot
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class VitiCarCallsScreenTest {
    @Test
    fun directoryTemplateShowsBoundedSafeTargets() = onMain {
        val controller = FakeController(
            CarCallState(
                coreTelecomAvailable = true,
                serviceConnected = true,
                loadingDirectory = false,
                targets = listOf(CarDialTarget("Alice", "+34600000001")),
            ),
        )

        val template = screen(controller).onGetTemplate() as ListTemplate
        val row = template.singleList!!.items.single() as Row

        assertEquals("Viti Operator", template.header!!.title.toString())
        assertEquals("Alice", row.title.toString())
        assertTrue(row.texts.any { it.toString().contains("+34600000001") })
    }

    @Test
    fun activeCallDefersControlsToTheAndroidAutoSystemSurface() = onMain {
        val controller = FakeController(
            CarCallState(
                coreTelecomAvailable = true,
                serviceConnected = true,
                loadingDirectory = false,
                call = VoiceCallSnapshot(legToken = "active-leg"),
            ),
        )

        val template = screen(controller).onGetTemplate() as MessageTemplate

        assertTrue(template.message.toString().contains("system in-call controls"))
    }

    @Test
    fun unsupportedTelecomIsReportedWithoutPretendingCallsWillWork() = onMain {
        val template = screen(FakeController(CarCallState())).onGetTemplate() as MessageTemplate

        assertTrue(template.message.toString().contains("Core-Telecom is unavailable"))
    }

    private fun screen(controller: CarCallController): VitiCarCallsScreen {
        val context = TestCarContext.createCarContext(ApplicationProvider.getApplicationContext())
        return VitiCarCallsScreen(context, controller)
    }

    private fun onMain(action: () -> Unit) = InstrumentationRegistry.getInstrumentation().runOnMainSync { action() }

    private class FakeController(initial: CarCallState) : CarCallController {
        override val state: StateFlow<CarCallState> = MutableStateFlow(initial)
        override fun start() = Unit
        override fun refresh() = Unit
        override fun call(target: CarDialTarget) = Unit
        override fun clearNotice() = Unit
        override fun close() = Unit
    }
}

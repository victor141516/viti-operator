package com.viti.operator.voice

import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.Job
import kotlinx.coroutines.awaitCancellation
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertTrue
import org.junit.Test

class CoreTelecomGatewayLifecycleTest {
    @OptIn(ExperimentalCoroutinesApi::class)
    @Test
    fun endingTheCallScopeCancelsAnInfiniteAudioCollector() = runTest(UnconfinedTestDispatcher()) {
        lateinit var collector: Job

        withDetachedCallScope { scope ->
            collector = scope.launch { awaitCancellation() }
            assertTrue(collector.isActive)
        }

        assertTrue(collector.isCancelled)
    }
}

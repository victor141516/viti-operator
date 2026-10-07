package com.viti.operator.voice

import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class VoiceSignalingSupervisorTest {
    @Test
    fun nativeChecksContinueWithoutAnActivityAndReleaseTheirLeaseOnShutdown() = runTest(UnconfinedTestDispatcher()) {
        var checks = 0
        var released = 0
        val job = launchVoiceSignalingSupervisor(maintain = { checks++ }, onFailure = { throw it }, release = { released++ })
        advanceTimeBy(90_000)
        runCurrent()
        assertEquals(4, checks)
        job.cancel()
        runCurrent()
        assertEquals(1, released)
    }

    @Test
    fun failuresAreReportedAndRetriedWithoutTerminatingTheSupervisor() = runTest(UnconfinedTestDispatcher()) {
        var checks = 0
        var failures = 0
        val job = launchVoiceSignalingSupervisor(maintain = { if (++checks == 1) error("unresponsive") }, onFailure = { failures++ })
        advanceTimeBy(30_000)
        runCurrent()
        assertEquals(2, checks)
        assertEquals(1, failures)
        job.cancel()
        assertEquals(1, failures)
    }
}

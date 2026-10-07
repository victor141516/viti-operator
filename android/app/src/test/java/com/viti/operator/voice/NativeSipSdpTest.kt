package com.viti.operator.voice

import org.junit.Assert.*
import org.junit.Test

class NativeSipSdpTest {
    @Test fun gatheredCandidatesAreIncludedOnceInTheirMediaSection() {
        val original = "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 0\r\na=mid:0\r\na=candidate:one\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\na=mid:1\r\n"
        val merged = NativeSipSdp.withCandidates(original, listOf(0 to "candidate:one", 0 to "candidate:two", 1 to "candidate:data"))
        assertEquals(1, Regex("a=candidate:one").findAll(merged).count())
        assertTrue(merged.indexOf("a=candidate:two") < merged.indexOf("m=application"))
        assertTrue(merged.indexOf("a=candidate:data") > merged.indexOf("m=application"))
        assertTrue(merged.endsWith("\r\n"))
    }
}

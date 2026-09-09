package app

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// StartRTPProxy exposes the isolated Asterisk RTP range through the same host
// that serves the clients. One client per RTP port is sufficient because
// Asterisk allocates a distinct port for each active media stream.
func (a *App) StartRTPProxy(ctx context.Context) error {
	cfg := a.cfg.Voice
	if !cfg.Enabled || strings.TrimSpace(cfg.RTPListenAddress) == "" || strings.TrimSpace(cfg.RTPBackendAddress) == "" {
		return nil
	}
	if cfg.RTPStart <= 0 || cfg.RTPEnd < cfg.RTPStart || cfg.RTPEnd-cfg.RTPStart > 512 {
		return fmt.Errorf("invalid RTP port range")
	}
	backendIP := net.ParseIP(cfg.RTPBackendAddress)
	if backendIP == nil {
		return fmt.Errorf("invalid RTP backend address")
	}
	opened := make([]*net.UDPConn, 0, cfg.RTPEnd-cfg.RTPStart+1)
	for port := cfg.RTPStart; port <= cfg.RTPEnd; port++ {
		listen := &net.UDPAddr{IP: net.ParseIP(cfg.RTPListenAddress), Port: port}
		conn, err := net.ListenUDP("udp", listen)
		if err != nil {
			for _, item := range opened {
				_ = item.Close()
			}
			return fmt.Errorf("listen RTP %s: %w", listen, err)
		}
		opened = append(opened, conn)
		go relayRTP(ctx, conn, &net.UDPAddr{IP: backendIP, Port: port}, a.diag)
	}
	a.logger.Info("RTP proxy listening", "address", cfg.RTPListenAddress, "ports", fmt.Sprintf("%d-%d", cfg.RTPStart, cfg.RTPEnd))
	return nil
}

func relayRTP(ctx context.Context, conn *net.UDPConn, backend *net.UDPAddr, diagnostics *DiagnosticRecorder) {
	go func() { <-ctx.Done(); _ = conn.Close() }()
	buffer := make([]byte, 64<<10)
	var mu sync.RWMutex
	var client *net.UDPAddr
	for {
		n, source, err := conn.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		if source.IP.Equal(backend.IP) && source.Port == backend.Port {
			mu.RLock()
			destination := client
			mu.RUnlock()
			if destination != nil {
				diagnostics.RecordPacket(conn.LocalAddr().(*net.UDPAddr).Port, "backend_to_client", source.String(), destination.String(), buffer[:n])
				_, _ = conn.WriteToUDP(buffer[:n], destination)
			}
			continue
		}
		mu.Lock()
		client = source
		mu.Unlock()
		diagnostics.RecordPacket(conn.LocalAddr().(*net.UDPAddr).Port, "client_to_backend", source.String(), backend.String(), buffer[:n])
		_, _ = conn.WriteToUDP(buffer[:n], backend)
		_ = conn.SetReadDeadline(time.Time{})
	}
}

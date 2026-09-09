// viti-adb-bridge exposes the Windows localhost ADB server only on a selected
// private host interface so a Docker Desktop container can reach it. It refuses
// wildcard/public listeners and non-loopback upstreams by design.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

func main() {
	listenAddress := flag.String("listen", "", "private host address and port, for example 172.23.192.1:5038")
	upstreamAddress := flag.String("upstream", "127.0.0.1:5037", "loopback ADB server address")
	flag.Parse()
	if err := validateAddresses(*listenAddress, *upstreamAddress); err != nil {
		slog.Error("unsafe bridge configuration", "error", err)
		os.Exit(2)
	}

	listener, err := net.Listen("tcp", *listenAddress)
	if err != nil {
		slog.Error("could not listen", "error", err)
		os.Exit(1)
	}
	defer listener.Close()
	slog.Info("ADB bridge listening", "address", listener.Addr(), "upstream", *upstreamAddress)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		_ = listener.Close()
	}()

	for {
		client, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Warn("accept failed", "error", err)
			continue
		}
		go proxy(client, *upstreamAddress)
	}
}

func validateAddresses(listenAddress, upstreamAddress string) error {
	listenHost, _, err := net.SplitHostPort(listenAddress)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	listenIP := net.ParseIP(listenHost)
	if listenIP == nil || listenIP.IsUnspecified() || (!listenIP.IsPrivate() && !listenIP.IsLoopback()) {
		return errors.New("listen address must be one explicit private or loopback IP")
	}
	upstreamHost, _, err := net.SplitHostPort(upstreamAddress)
	if err != nil {
		return fmt.Errorf("invalid upstream address: %w", err)
	}
	upstreamIP := net.ParseIP(upstreamHost)
	if upstreamIP == nil || !upstreamIP.IsLoopback() {
		return errors.New("upstream must be an explicit loopback IP")
	}
	return nil
}

func proxy(client net.Conn, upstreamAddress string) {
	defer client.Close()
	upstream, err := net.Dial("tcp", upstreamAddress)
	if err != nil {
		slog.Warn("ADB upstream unavailable", "error", err)
		return
	}
	defer upstream.Close()

	var wait sync.WaitGroup
	wait.Add(2)
	copyHalf := func(dst, src net.Conn) {
		defer wait.Done()
		_, _ = io.Copy(dst, src)
		if tcp, ok := dst.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}
	go copyHalf(upstream, client)
	go copyHalf(client, upstream)
	wait.Wait()
}

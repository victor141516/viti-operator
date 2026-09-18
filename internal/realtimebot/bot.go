package realtimebot

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const pendingCallAudioPackets = 1024

type Bot struct {
	config Config
	logger *slog.Logger
}

func New(config Config, logger *slog.Logger) *Bot {
	if logger == nil {
		logger = slog.Default()
	}
	return &Bot{config: config, logger: logger}
}

func (bot *Bot) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := bot.runConnection(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		bot.logger.Warn("Viti control connection ended", "error", err, "retry_in", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (bot *Bot) runConnection(ctx context.Context) error {
	client, err := dialViti(ctx, bot.config)
	if err != nil {
		return err
	}
	defer client.close()
	connectionCtx, cancelConnection := context.WithCancel(ctx)
	defer cancelConnection()
	var callsMu sync.Mutex
	calls := make(map[string]context.CancelFunc)
	cancelAllCalls := func() {
		callsMu.Lock()
		cancellations := make([]context.CancelFunc, 0, len(calls))
		for callID, cancel := range calls {
			cancellations = append(cancellations, cancel)
			delete(calls, callID)
		}
		callsMu.Unlock()
		for _, cancel := range cancellations {
			cancel()
		}
	}
	startCall := func(callID string) {
		callsMu.Lock()
		if _, exists := calls[callID]; exists {
			callsMu.Unlock()
			return
		}
		callCtx, cancel := context.WithCancel(connectionCtx)
		calls[callID] = cancel
		callsMu.Unlock()
		go bot.superviseCall(callCtx, client, callID)
	}
	onActive := func(callID string) {
		if bot.config.JoinMode == "all" {
			startCall(callID)
		}
	}
	onDelegated := func(callID, targetService string) {
		if bot.config.JoinMode == "targeted" && targetService == bot.config.ServiceName {
			startCall(callID)
		}
	}
	onEnded := func(callID string) {
		callsMu.Lock()
		cancel := calls[callID]
		delete(calls, callID)
		callsMu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	onAccessChanged := func(active bool) {
		if !active {
			cancelAllCalls()
		}
	}
	onDelegationEnded := func(callID string) {
		if bot.config.JoinMode == "targeted" {
			onEnded(callID)
		}
	}
	bot.logger.Info("connected to Viti integration endpoint", "service_name", bot.config.ServiceName)
	err = client.serve(connectionCtx, vitiHandlers{
		Active: onActive, Ended: onEnded, AccessChanged: onAccessChanged,
		Delegated: onDelegated, DelegationEnded: onDelegationEnded,
	})
	cancelConnection()
	cancelAllCalls()
	return err
}

func (bot *Bot) superviseCall(ctx context.Context, client *vitiClient, callID string) {
	backoff := time.Second
	for {
		err := bot.runCall(ctx, client, callID)
		if ctx.Err() != nil {
			bot.logger.Info("OpenAI Realtime left call", "call_id", callID)
			return
		}
		bot.logger.Warn("OpenAI Realtime call bridge ended", "call_id", callID, "error", err, "retry_in", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (bot *Bot) runCall(ctx context.Context, client *vitiClient, callID string) error {
	bridgeCtx, cancelBridge := context.WithCancel(ctx)
	defer cancelBridge()
	// Prove that Viti can attach this service to the Asterisk conference before
	// opening a billable Realtime session. Keep the short setup window in memory
	// so the caller's first words are not lost while Realtime connects.
	callAudio := make(chan []byte, pendingCallAudioPackets)
	mediaSession, err := attachMedia(bridgeCtx, client, callID, "auxiliary", func(audio []byte) error {
		packet := append([]byte(nil), audio...)
		select {
		case callAudio <- packet:
			return nil
		case <-bridgeCtx.Done():
			return bridgeCtx.Err()
		default:
			return errors.New("pending Viti call audio exceeded the setup buffer")
		}
	})
	if err != nil {
		return err
	}
	defer mediaSession.close()
	realtime, err := connectRealtime(bridgeCtx, bot.config)
	if err != nil {
		return err
	}
	defer realtime.close()
	bot.logger.Info("OpenAI Realtime joined active call", "call_id", callID, "model", bot.config.Model, "voice", bot.config.Voice)
	realtimeErrors := make(chan error, 1)
	mediaErrors := make(chan error, 1)
	forwardErrors := make(chan error, 1)
	go func() { realtimeErrors <- realtime.run(bridgeCtx, mediaSession.player) }()
	go func() { mediaErrors <- mediaSession.wait(bridgeCtx) }()
	go func() {
		for {
			select {
			case audio := <-callAudio:
				if err := realtime.appendAudio(audio); err != nil {
					forwardErrors <- err
					return
				}
			case <-bridgeCtx.Done():
				return
			}
		}
	}()
	select {
	case err := <-realtimeErrors:
		return err
	case err := <-mediaErrors:
		return err
	case err := <-forwardErrors:
		return err
	case <-bridgeCtx.Done():
		return bridgeCtx.Err()
	}
}

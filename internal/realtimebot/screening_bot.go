package realtimebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type screeningDecisionReceipt struct {
	Revision uint64 `json:"revision"`
	State    string `json:"state"`
}

type screeningResult struct {
	Classification string `json:"classification"`
	Commercial     *bool  `json:"commercial,omitempty"`
	Reason         string `json:"reason"`
	TargetService  string `json:"target_service,omitempty"`
}

type screeningJob struct {
	cancel          context.CancelFunc
	active          chan struct{}
	handlerJoined   chan struct{}
	handlerLeft     chan struct{}
	activeOnce      sync.Once
	handlerJoinOnce sync.Once
	handlerLeftOnce sync.Once
}

func newScreeningJob(cancel context.CancelFunc, alreadyActive bool) *screeningJob {
	job := &screeningJob{
		cancel: cancel, active: make(chan struct{}),
		handlerJoined: make(chan struct{}), handlerLeft: make(chan struct{}),
	}
	if alreadyActive {
		job.markActive()
	}
	return job
}

func (job *screeningJob) markActive() { job.activeOnce.Do(func() { close(job.active) }) }

func (job *screeningJob) markHandlerJoined() {
	job.handlerJoinOnce.Do(func() { close(job.handlerJoined) })
}

func (job *screeningJob) markHandlerLeft() {
	job.handlerLeftOnce.Do(func() { close(job.handlerLeft) })
}

type CommercialScreeningBot struct {
	config ScreeningConfig
	logger *slog.Logger
}

func NewCommercialScreeningBot(config ScreeningConfig, logger *slog.Logger) *CommercialScreeningBot {
	if logger == nil {
		logger = slog.Default()
	}
	return &CommercialScreeningBot{config: config, logger: logger}
}

func (bot *CommercialScreeningBot) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := bot.runConnection(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		bot.logger.Warn("Viti screening control connection ended", "error", err, "retry_in", backoff)
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

func (bot *CommercialScreeningBot) runConnection(ctx context.Context) error {
	capabilities := map[string]any{
		"call.media": map[string]any{},
		"call.screen": map[string]any{
			"decision_timeout_ms":   bot.config.DecisionTimeout.Milliseconds(),
			"media_timeout_ms":      bot.config.MediaTimeout.Milliseconds(),
			"processing_timeout_ms": -1,
		},
	}
	client, err := dialVitiCapabilities(ctx, bot.config.VitiURL, bot.config.VitiAPIKey, bot.config.ServiceName, capabilities)
	if err != nil {
		return err
	}
	defer client.close()
	connectionCtx, cancelConnection := context.WithCancel(ctx)
	defer cancelConnection()
	var jobsMu sync.Mutex
	jobs := make(map[string]*screeningJob)
	cancelJob := func(callID string) {
		jobsMu.Lock()
		job := jobs[callID]
		delete(jobs, callID)
		jobsMu.Unlock()
		if job != nil {
			job.cancel()
		}
	}
	cancelAll := func() {
		jobsMu.Lock()
		current := make([]*screeningJob, 0, len(jobs))
		for callID, job := range jobs {
			current = append(current, job)
			delete(jobs, callID)
		}
		jobsMu.Unlock()
		for _, job := range current {
			job.cancel()
		}
	}
	finishJob := func(callID string, expected *screeningJob) {
		jobsMu.Lock()
		if jobs[callID] == expected {
			delete(jobs, callID)
		}
		jobsMu.Unlock()
	}
	onOffer := func(offer vitiScreeningOffer) {
		if offer.TargetService != bot.config.ServiceName {
			return
		}
		jobCtx, cancel := context.WithCancel(connectionCtx)
		job := newScreeningJob(cancel, offer.Context == "active")
		jobsMu.Lock()
		previous := jobs[offer.CallID]
		jobs[offer.CallID] = job
		jobsMu.Unlock()
		if previous != nil {
			previous.cancel()
		}
		go func() {
			defer finishJob(offer.CallID, job)
			bot.handleOffer(jobCtx, client, job, offer)
		}()
	}
	onActive := func(callID string) {
		jobsMu.Lock()
		job := jobs[callID]
		jobsMu.Unlock()
		if job != nil {
			job.markActive()
		}
	}
	onParticipantJoined := func(callID, serviceName string) {
		if serviceName != bot.config.HandlerServiceName {
			return
		}
		jobsMu.Lock()
		job := jobs[callID]
		jobsMu.Unlock()
		if job != nil {
			job.markHandlerJoined()
		}
	}
	onParticipantLeft := func(callID, serviceName string) {
		if serviceName != bot.config.HandlerServiceName {
			return
		}
		jobsMu.Lock()
		job := jobs[callID]
		jobsMu.Unlock()
		if job != nil {
			job.markHandlerLeft()
		}
	}
	bot.logger.Info("connected to Viti screening endpoint", "service_name", bot.config.ServiceName)
	err = client.serve(connectionCtx, vitiHandlers{
		Active: onActive, ActiveWhileInactive: true, Ended: cancelJob, ScreeningOffer: onOffer,
		ParticipantJoined: onParticipantJoined, ParticipantLeft: onParticipantLeft,
	})
	cancelConnection()
	cancelAll()
	return err
}

func (bot *CommercialScreeningBot) handleOffer(ctx context.Context, client *vitiClient, job *screeningJob, offer vitiScreeningOffer) {
	revision := offer.Revision
	if offer.Call.ContactID != "" || offer.Call.ContactName != "" {
		commercial := false
		_, err := screeningDecision(ctx, client, offer.CallID, revision, "ring_clients", screeningResult{
			Classification: "known_contact", Commercial: &commercial, Reason: "contact_match",
		})
		if err != nil && ctx.Err() == nil {
			bot.logger.Warn("could not pass known contact to clients", "call_id", offer.CallID, "error", err)
		}
		return
	}
	greeting, err := loadGreetingPCMU(bot.config.GreetingPath)
	if err != nil {
		bot.failOpen(ctx, client, offer.CallID, revision, "greeting_unavailable", err)
		return
	}
	if bot.config.OpenAIAPIKey == "" {
		bot.failOpen(ctx, client, offer.CallID, revision, "openai_not_configured", errClassificationUnavailable)
		return
	}
	receipt, err := screeningDecision(ctx, client, offer.CallID, revision, "take_control", nil)
	if err != nil {
		if ctx.Err() == nil {
			bot.logger.Warn("could not reserve screening call", "call_id", offer.CallID, "error", err)
		}
		return
	}
	revision = receipt.Revision
	callAudio := make(chan []byte, pendingCallAudioPackets)
	mediaSession, err := attachMedia(ctx, client, offer.CallID, "controller", func(audio []byte) error {
		packet := append([]byte(nil), audio...)
		select {
		case callAudio <- packet:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			return errors.New("pending screening audio exceeded the setup buffer")
		}
	})
	if err != nil {
		if ctx.Err() == nil {
			bot.logger.Warn("could not attach screening media", "call_id", offer.CallID, "error", err)
		}
		return
	}
	defer mediaSession.close()
	mediaErrors := make(chan error, 1)
	go func() { mediaErrors <- mediaSession.wait(ctx) }()
	if offer.Context != "active" {
		select {
		case <-job.active:
		case err := <-mediaErrors:
			bot.logger.Warn("screening media ended before carrier answer", "call_id", offer.CallID, "error", err)
			return
		case <-ctx.Done():
			return
		}
	}
	classifierCtx, cancelClassifier := context.WithCancel(ctx)
	classifier, err := connectClassifier(classifierCtx, bot.config.classifierConfig())
	if err != nil {
		cancelClassifier()
		bot.failOpen(ctx, client, offer.CallID, revision, "openai_connection_failed", err)
		return
	}
	classificationResults := make(chan struct {
		classification commercialClassification
		err            error
	}, 1)
	go func() {
		classification, classifyErr := classifier.classify(classifierCtx)
		classificationResults <- struct {
			classification commercialClassification
			err            error
		}{classification: classification, err: classifyErr}
	}()
	forwardErrors := make(chan error, 1)
	go func() {
		for {
			select {
			case audio := <-callAudio:
				if err := classifier.appendAudio(audio); err != nil {
					forwardErrors <- err
					return
				}
			case <-classifierCtx.Done():
				return
			}
		}
	}()
	if err := mediaSession.playPCMU(ctx, greeting); err != nil {
		cancelClassifier()
		bot.failOpen(ctx, client, offer.CallID, revision, "greeting_playback_failed", err)
		return
	}
	timer := time.NewTimer(bot.config.ClassificationTimeout)
	defer timer.Stop()
	var classification commercialClassification
	select {
	case result := <-classificationResults:
		if result.err != nil {
			cancelClassifier()
			bot.failOpen(ctx, client, offer.CallID, revision, "classification_failed", result.err)
			return
		}
		classification = result.classification
	case err := <-forwardErrors:
		cancelClassifier()
		bot.failOpen(ctx, client, offer.CallID, revision, "classification_audio_failed", err)
		return
	case err := <-mediaErrors:
		cancelClassifier()
		bot.logger.Warn("screening media ended during classification", "call_id", offer.CallID, "error", err)
		return
	case <-timer.C:
		cancelClassifier()
		bot.failOpen(ctx, client, offer.CallID, revision, "classification_timeout", errClassificationUnavailable)
		return
	case <-ctx.Done():
		cancelClassifier()
		return
	}
	cancelClassifier()
	if classification != classificationCommercial {
		commercial := false
		_, err := screeningDecision(ctx, client, offer.CallID, revision, "ring_clients", screeningResult{
			Classification: string(classificationNonCommercial), Commercial: &commercial, Reason: "model_classification",
		})
		if err != nil && ctx.Err() == nil {
			bot.logger.Warn("could not pass non-commercial call to clients", "call_id", offer.CallID, "error", err)
		}
		return
	}
	commercial := true
	receipt, err = screeningDecision(ctx, client, offer.CallID, revision, "delegate", screeningResult{
		Classification: string(classificationCommercial), Commercial: &commercial,
		Reason: "model_classification", TargetService: bot.config.HandlerServiceName,
	})
	if err != nil {
		bot.failOpen(ctx, client, offer.CallID, revision, "delegation_failed", err)
		return
	}
	revision = receipt.Revision
	handoffTimer := time.NewTimer(bot.config.HandlerJoinTimeout)
	defer handoffTimer.Stop()
	select {
	case <-job.handlerJoined:
		bot.logger.Info("commercial call delegated", "call_id", offer.CallID, "target_service", bot.config.HandlerServiceName)
		select {
		case <-job.handlerLeft:
			bot.failOpen(ctx, client, offer.CallID, revision, "handler_left", errClassificationUnavailable)
		case err := <-mediaErrors:
			if ctx.Err() == nil {
				bot.logger.Warn("screening controller left delegated call", "call_id", offer.CallID, "error", err)
			}
		case <-ctx.Done():
		}
	case <-handoffTimer.C:
		bot.failOpen(ctx, client, offer.CallID, revision, "handler_join_timeout", errClassificationUnavailable)
	case <-job.handlerLeft:
		bot.failOpen(ctx, client, offer.CallID, revision, "handler_left_before_ready", errClassificationUnavailable)
	case err := <-mediaErrors:
		if ctx.Err() == nil {
			bot.logger.Warn("screening controller ended during delegation", "call_id", offer.CallID, "error", err)
		}
	case <-ctx.Done():
	}
}

func (bot *CommercialScreeningBot) failOpen(ctx context.Context, client *vitiClient, callID string, revision uint64, reason string, cause error) {
	_, err := screeningDecision(ctx, client, callID, revision, "ring_clients", screeningResult{
		Classification: "uncertain", Reason: reason,
	})
	if err != nil && ctx.Err() == nil {
		bot.logger.Warn("commercial screening could not fail open", "call_id", callID, "reason", reason, "error", err)
		return
	}
	bot.logger.Warn("commercial screening failed open to clients", "call_id", callID, "reason", reason, "error", cause)
}

func screeningDecision(ctx context.Context, client *vitiClient, callID string, revision uint64, action string, output any) (screeningDecisionReceipt, error) {
	payload := map[string]any{"revision": revision, "action": action}
	if output != nil {
		payload["output"] = output
	}
	raw, err := client.command(ctx, "screening.decision", callID, payload)
	if err != nil {
		return screeningDecisionReceipt{}, err
	}
	var receipt screeningDecisionReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return screeningDecisionReceipt{}, fmt.Errorf("decode Viti screening receipt: %w", err)
	}
	if receipt.Revision == 0 {
		return screeningDecisionReceipt{}, errors.New("Viti returned a screening receipt without a revision")
	}
	return receipt, nil
}

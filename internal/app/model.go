package app

import (
	"encoding/json"
	"time"
)

type Direction string

const (
	DirectionIncoming Direction = "incoming"
	DirectionOutgoing Direction = "outgoing"
)

type Message struct {
	ID                      string          `json:"id"`
	Direction               Direction       `json:"direction"`
	From                    string          `json:"from"`
	To                      string          `json:"to"`
	Text                    string          `json:"text"`
	ProtocolDebug           bool            `json:"protocol_debug,omitempty"`
	State                   string          `json:"state"`
	DeliveryState           string          `json:"delivery_state,omitempty"`
	DeliveryReportRequested bool            `json:"delivery_report_requested,omitempty"`
	DeliveryReportDeadline  *time.Time      `json:"delivery_report_deadline,omitempty"`
	StatusDetail            string          `json:"status_detail,omitempty"`
	FailureCode             string          `json:"failure_code,omitempty"`
	FailureReason           string          `json:"failure_reason,omitempty"`
	Parts                   []SMSPartStatus `json:"parts,omitempty"`
	CreatedAt               time.Time       `json:"created_at"`
	UpdatedAt               time.Time       `json:"updated_at"`
	ContactID               string          `json:"contact_id,omitempty"`
	ContactName             string          `json:"contact_name,omitempty"`
}

type SMSPartStatus struct {
	Part              int        `json:"part"`
	Parts             int        `json:"parts"`
	Instance          string     `json:"instance,omitempty"`
	Recipient         string     `json:"recipient,omitempty"`
	RPReference       *int       `json:"rp_reference,omitempty"`
	TPReference       *int       `json:"tp_reference,omitempty"`
	State             string     `json:"state"`
	StatusCode        string     `json:"status_code,omitempty"`
	FailureCode       string     `json:"failure_code,omitempty"`
	FailureReason     string     `json:"failure_reason,omitempty"`
	Detail            string     `json:"detail,omitempty"`
	SubmittedAt       *time.Time `json:"submitted_at,omitempty"`
	ServiceCentreTime *time.Time `json:"service_centre_time,omitempty"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type Call struct {
	ID                    string                 `json:"id"`
	TraceID               string                 `json:"trace_id,omitempty"`
	EngineCallID          string                 `json:"engine_call_id,omitempty"`
	Instance              string                 `json:"instance,omitempty"`
	Direction             Direction              `json:"direction"`
	Peer                  string                 `json:"peer"`
	State                 string                 `json:"state"`
	StartedAt             time.Time              `json:"started_at"`
	AnsweredAt            *time.Time             `json:"answered_at,omitempty"`
	UpdatedAt             time.Time              `json:"updated_at"`
	EndedAt               *time.Time             `json:"ended_at,omitempty"`
	RecordingTracks       []RecordingTrack       `json:"recording_tracks,omitempty"`
	RecordingParticipants []RecordingParticipant `json:"recording_participants,omitempty"`
	ContactID             string                 `json:"contact_id,omitempty"`
	ContactName           string                 `json:"contact_name,omitempty"`
	Screening             *CallScreeningRecord   `json:"screening,omitempty"`
}

type CallScreeningRecord struct {
	Pipeline        []string               `json:"pipeline"`
	StartedAt       time.Time              `json:"started_at"`
	ContextRevision uint64                 `json:"context_revision"`
	CurrentStage    int                    `json:"current_stage"`
	State           string                 `json:"state"`
	Answered        bool                   `json:"answered"`
	Stages          []ScreeningStageRecord `json:"stages,omitempty"`
	Outcome         string                 `json:"outcome,omitempty"`
	TerminatedBy    string                 `json:"terminated_by,omitempty"`
	Reason          string                 `json:"reason,omitempty"`
	CompletedAt     *time.Time             `json:"completed_at,omitempty"`
}

type ScreeningStageRecord struct {
	Index               int               `json:"index"`
	ServiceName         string            `json:"service_name"`
	Context             string            `json:"context"`
	ContextRevision     uint64            `json:"context_revision"`
	DecisionTimeoutMS   int64             `json:"decision_timeout_ms"`
	ProcessingTimeoutMS int64             `json:"processing_timeout_ms"`
	MediaTimeoutMS      int64             `json:"media_timeout_ms"`
	OfferedAt           *time.Time        `json:"offered_at,omitempty"`
	Updates             []ScreeningUpdate `json:"updates,omitempty"`
	Outcome             string            `json:"outcome,omitempty"`
	CompletedAt         *time.Time        `json:"completed_at,omitempty"`
}

type ScreeningUpdate struct {
	ServiceName string          `json:"service_name"`
	CommandID   string          `json:"command_id"`
	Action      string          `json:"action"`
	Output      json.RawMessage `json:"output,omitempty"`
	At          time.Time       `json:"at"`
}

type CallScreeningSummary struct {
	Pipeline        []string   `json:"pipeline"`
	StartedAt       time.Time  `json:"started_at"`
	ContextRevision uint64     `json:"context_revision"`
	CurrentStage    int        `json:"current_stage"`
	State           string     `json:"state"`
	Answered        bool       `json:"answered"`
	Outcome         string     `json:"outcome,omitempty"`
	TerminatedBy    string     `json:"terminated_by,omitempty"`
	Reason          string     `json:"reason,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

func (record *CallScreeningRecord) Summary() *CallScreeningSummary {
	if record == nil {
		return nil
	}
	return &CallScreeningSummary{
		Pipeline: append([]string(nil), record.Pipeline...), StartedAt: record.StartedAt,
		ContextRevision: record.ContextRevision, CurrentStage: record.CurrentStage,
		State: record.State, Answered: record.Answered, Outcome: record.Outcome,
		TerminatedBy: record.TerminatedBy, Reason: record.Reason, CompletedAt: cloneTime(record.CompletedAt),
	}
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneCallScreening(record *CallScreeningRecord) *CallScreeningRecord {
	if record == nil {
		return nil
	}
	cloned := *record
	cloned.Pipeline = append([]string(nil), record.Pipeline...)
	cloned.CompletedAt = cloneTime(record.CompletedAt)
	cloned.Stages = make([]ScreeningStageRecord, len(record.Stages))
	for index, stage := range record.Stages {
		cloned.Stages[index] = stage
		cloned.Stages[index].OfferedAt = cloneTime(stage.OfferedAt)
		cloned.Stages[index].CompletedAt = cloneTime(stage.CompletedAt)
		cloned.Stages[index].Updates = make([]ScreeningUpdate, len(stage.Updates))
		for updateIndex, update := range stage.Updates {
			cloned.Stages[index].Updates[updateIndex] = update
			cloned.Stages[index].Updates[updateIndex].Output = append(json.RawMessage(nil), update.Output...)
		}
	}
	return &cloned
}

type ContactNumber struct {
	Original   string `json:"original"`
	Normalized string `json:"normalized,omitempty"`
	Label      string `json:"label,omitempty"`
}

type ContactSourceRecord struct {
	SourceClientID   string          `json:"source_client_id"`
	SourceClientName string          `json:"source_client_name"`
	SourceContactID  string          `json:"source_contact_id"`
	Region           string          `json:"region,omitempty"`
	Names            []string        `json:"names"`
	Numbers          []ContactNumber `json:"numbers"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type ContactSource struct {
	ClientID   string `json:"client_id"`
	ClientName string `json:"client_name"`
}

type Contact struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Names   []string        `json:"names"`
	Numbers []ContactNumber `json:"numbers"`
	Sources []ContactSource `json:"sources"`
}

type RecordingTrack struct {
	ID            string    `json:"id"`
	Role          string    `json:"role"`
	ParticipantID string    `json:"participant_id,omitempty"`
	ClientID      string    `json:"client_id,omitempty"`
	Label         string    `json:"label"`
	ContentType   string    `json:"content_type"`
	File          string    `json:"file,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	OffsetMS      int64     `json:"offset_ms"`
	DurationMS    int64     `json:"duration_ms"`
}

type RecordingParticipant struct {
	ParticipantID string `json:"participant_id"`
	ClientID      string `json:"client_id"`
	Name          string `json:"name"`
}

type RecordingSettings struct {
	Enabled bool `json:"enabled"`
}

type RuntimeStatus struct {
	Ready           bool      `json:"ready"`
	SIMReady        bool      `json:"sim_ready"`
	TunnelReady     bool      `json:"tunnel_ready"`
	IMSReady        bool      `json:"ims_ready"`
	ContactsVersion uint64    `json:"contacts_version"`
	Provider        string    `json:"provider"`
	Detail          string    `json:"detail,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type IMSRegistrationState struct {
	Enabled    bool   `json:"enabled"`
	Registered bool   `json:"registered"`
	Detail     string `json:"detail,omitempty"`
}

type Event struct {
	Type       string    `json:"type"`
	Version    int       `json:"version"`
	EventID    string    `json:"event_id"`
	StreamID   string    `json:"stream_id"`
	Sequence   uint64    `json:"sequence"`
	OccurredAt time.Time `json:"occurred_at"`
	CallID     string    `json:"call_id,omitempty"`
	Payload    any       `json:"payload"`
	// At and Data keep the established long-poll wire contract while clients
	// migrate to OccurredAt and Payload.
	At   time.Time `json:"at"`
	Data any       `json:"data"`
}

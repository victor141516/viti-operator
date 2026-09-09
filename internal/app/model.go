package app

import "time"

type Direction string

const (
	DirectionIncoming Direction = "incoming"
	DirectionOutgoing Direction = "outgoing"
)

type Message struct {
	ID        string    `json:"id"`
	Direction Direction `json:"direction"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Text      string    `json:"text"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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
	Ready       bool      `json:"ready"`
	SIMReady    bool      `json:"sim_ready"`
	TunnelReady bool      `json:"tunnel_ready"`
	IMSReady    bool      `json:"ims_ready"`
	Provider    string    `json:"provider"`
	Detail      string    `json:"detail,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type IMSRegistrationState struct {
	Enabled    bool   `json:"enabled"`
	Registered bool   `json:"registered"`
	Detail     string `json:"detail,omitempty"`
}

type Event struct {
	Sequence uint64    `json:"sequence"`
	Type     string    `json:"type"`
	At       time.Time `json:"at"`
	Data     any       `json:"data"`
}

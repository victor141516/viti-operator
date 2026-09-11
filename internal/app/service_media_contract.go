package app

import (
	"context"
	"encoding/json"
)

type serviceMediaAttachRequest struct {
	ServiceName  string
	ConnectionID string
	Call         Call
	OfferSDP     string
	Purpose      string
}

type serviceMediaDescription struct {
	SessionID string `json:"session_id"`
	CallID    string `json:"call_id"`
	AnswerSDP string `json:"sdp_answer"`
	Purpose   string `json:"purpose"`
}

type serviceMediaICERequest struct {
	ServiceName  string
	ConnectionID string
	SessionID    string
	Candidate    string
	SDPMid       *string
	SDPMLine     *uint16
}

type serviceMediaDTMFRequest struct {
	ServiceName  string
	ConnectionID string
	CallID       string
	SessionID    string
	Digit        string
	DurationMS   int
}

type serviceMediaController interface {
	Attach(context.Context, serviceMediaAttachRequest) (serviceMediaDescription, error)
	PrepareOutgoing(context.Context, serviceMediaAttachRequest) (serviceMediaOutgoingPreparation, error)
	AddICECandidate(context.Context, serviceMediaICERequest) error
	SendDTMF(context.Context, serviceMediaDTMFRequest) error
	CloseConnection(connectionID, reason string)
	CloseCall(callID, reason string)
}

type serviceMediaOutgoingPreparation struct {
	Description serviceMediaDescription
	Activate    func(context.Context, string) error
}

type serviceMediaBridgeRequest struct {
	SessionID    string
	CallID       string
	EngineCallID string
	TraceID      string
	ServiceName  string
	ConnectionID string
	RTPHost      string
	RTPPort      int
	Codec        string
}

type serviceMediaBridge interface {
	JoinServiceMedia(context.Context, serviceMediaBridgeRequest) error
	StartServiceCall(context.Context, serviceMediaBridgeRequest, string) error
	EndServiceCall(context.Context, string) error
	LeaveServiceMedia(context.Context, string) error
	SendServiceDTMF(context.Context, string, string, int) error
}

type integrationMediaAttachPayload struct {
	OfferSDP string `json:"sdp_offer"`
	Purpose  string `json:"purpose,omitempty"`
}

type integrationMediaICEPayload struct {
	SessionID string  `json:"session_id"`
	Candidate string  `json:"candidate"`
	SDPMid    *string `json:"sdp_mid,omitempty"`
	SDPMLine  *uint16 `json:"sdp_mline_index,omitempty"`
}

type integrationDTMFPayload struct {
	SessionID string `json:"session_id,omitempty"`
	Digit     string `json:"digit"`
	Duration  int    `json:"duration_ms,omitempty"`
}

func decodeIntegrationPayload(raw json.RawMessage, destination any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	return json.Unmarshal(raw, destination)
}

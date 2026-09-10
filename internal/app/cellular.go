package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const cellularObservationFreshWindow = 2 * clientOnlineWindow

var (
	cellularMCCPattern = regexp.MustCompile(`^[0-9]{3}$`)
	cellularMNCPattern = regexp.MustCompile(`^[0-9]{2,3}$`)
)

// CellularObservation is a volatile, privacy-bounded snapshot reported by an
// Android client. It deliberately excludes subscription identifiers, slot
// numbers, IMSI, ICCID, MSISDN and cell identity/location.
type CellularObservation struct {
	Generation          string                    `json:"generation"`
	GenerationStartedAt time.Time                 `json:"generation_started_at"`
	Sequence            uint64                    `json:"sequence"`
	MeasuredAt          time.Time                 `json:"measured_at,omitempty"`
	ReceivedAt          time.Time                 `json:"received_at,omitempty"`
	SubscriptionRole    string                    `json:"subscription_role"`
	HomeNetwork         CellularNetworkDetails    `json:"home_network"`
	ServingNetwork      CellularNetworkDetails    `json:"serving_network"`
	Service             CellularServiceDetails    `json:"service"`
	DataAccess          CellularDataAccessDetails `json:"data_access"`
	Signal              CellularSignalDetails     `json:"signal"`
	Stale               bool                      `json:"stale"`
}

type CellularNetworkDetails struct {
	Availability string    `json:"availability"`
	Operator     string    `json:"operator,omitempty"`
	MCC          string    `json:"mcc,omitempty"`
	MNC          string    `json:"mnc,omitempty"`
	MeasuredAt   time.Time `json:"measured_at,omitempty"`
}

type CellularServiceDetails struct {
	Availability string    `json:"availability"`
	Registration string    `json:"registration,omitempty"`
	Roaming      *bool     `json:"roaming,omitempty"`
	MeasuredAt   time.Time `json:"measured_at,omitempty"`
}

type CellularDataAccessDetails struct {
	Availability string    `json:"availability"`
	Technology   string    `json:"technology,omitempty"`
	MeasuredAt   time.Time `json:"measured_at,omitempty"`
}

type CellularSignalDetails struct {
	Availability string    `json:"availability"`
	Level        *int      `json:"level,omitempty"`
	DBM          *int      `json:"dbm,omitempty"`
	MeasuredAt   time.Time `json:"measured_at,omitempty"`
}

func decodeCellularHeartbeat(raw json.RawMessage, receivedAt time.Time) (*CellularObservation, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, true, errors.New("cellular observation must be an ordered snapshot, not null")
	}
	var observation CellularObservation
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&observation); err != nil {
		return nil, true, fmt.Errorf("invalid cellular observation: %w", err)
	}
	if err := normalizeCellularObservation(&observation); err != nil {
		return nil, true, err
	}
	observation.ReceivedAt = receivedAt
	observation.Stale = false
	return &observation, true, nil
}

func normalizeCellularObservation(observation *CellularObservation) error {
	observation.Generation = strings.TrimSpace(observation.Generation)
	observation.SubscriptionRole = strings.TrimSpace(observation.SubscriptionRole)
	if len(observation.Generation) < 3 || len(observation.Generation) > 128 || !topologyIdentifierPattern.MatchString(observation.Generation) {
		return errors.New("cellular generation is invalid")
	}
	if observation.GenerationStartedAt.IsZero() || observation.Sequence == 0 {
		return errors.New("cellular generation_started_at and sequence are required")
	}
	if observation.MeasuredAt.IsZero() {
		return errors.New("cellular measured_at is required")
	}
	if observation.SubscriptionRole != "default_data" && observation.SubscriptionRole != "none" {
		return errors.New("cellular subscription_role must be default_data or none")
	}
	if err := normalizeCellularNetwork("home_network", &observation.HomeNetwork); err != nil {
		return err
	}
	if err := normalizeCellularNetwork("serving_network", &observation.ServingNetwork); err != nil {
		return err
	}
	if err := normalizeAvailability("service", observation.Service.Availability); err != nil {
		return err
	}
	if observation.Service.Registration != "" && !oneOf(observation.Service.Registration, "in_service", "out_of_service", "emergency_only", "power_off") {
		return errors.New("cellular service registration is invalid")
	}
	if observation.Service.Availability == "available" && (observation.Service.Registration == "" || observation.Service.MeasuredAt.IsZero()) {
		return errors.New("available cellular service requires registration evidence and measured_at")
	}
	if observation.Service.Availability != "available" {
		observation.Service.Registration, observation.Service.Roaming, observation.Service.MeasuredAt = "", nil, time.Time{}
	}
	if err := normalizeAvailability("data_access", observation.DataAccess.Availability); err != nil {
		return err
	}
	observation.DataAccess.Technology = strings.TrimSpace(observation.DataAccess.Technology)
	if len(observation.DataAccess.Technology) > 24 {
		return errors.New("cellular data access technology is too long")
	}
	if observation.DataAccess.Availability == "available" && (observation.DataAccess.Technology == "" || observation.DataAccess.MeasuredAt.IsZero()) {
		return errors.New("available cellular data access requires technology evidence and measured_at")
	}
	if observation.DataAccess.Availability != "available" {
		observation.DataAccess.Technology, observation.DataAccess.MeasuredAt = "", time.Time{}
	}
	if err := normalizeAvailability("signal", observation.Signal.Availability); err != nil {
		return err
	}
	if observation.Signal.Level != nil && (*observation.Signal.Level < 0 || *observation.Signal.Level > 4) {
		return errors.New("cellular signal level must be between 0 and 4")
	}
	if observation.Signal.DBM != nil && (*observation.Signal.DBM < -200 || *observation.Signal.DBM > 0) {
		return errors.New("cellular signal dBm is outside the supported range")
	}
	if observation.Signal.Availability == "available" && (observation.Signal.Level == nil && observation.Signal.DBM == nil || observation.Signal.MeasuredAt.IsZero()) {
		return errors.New("available cellular signal requires level or dBm evidence and measured_at")
	}
	if observation.Signal.Availability != "available" {
		observation.Signal.Level, observation.Signal.DBM, observation.Signal.MeasuredAt = nil, nil, time.Time{}
	}
	return nil
}

func normalizeCellularNetwork(field string, network *CellularNetworkDetails) error {
	if err := normalizeAvailability(field, network.Availability); err != nil {
		return err
	}
	network.Operator = strings.TrimSpace(network.Operator)
	network.MCC, network.MNC = strings.TrimSpace(network.MCC), strings.TrimSpace(network.MNC)
	if len(network.Operator) > 80 {
		return fmt.Errorf("cellular %s operator is too long", field)
	}
	if network.MCC != "" && !cellularMCCPattern.MatchString(network.MCC) {
		return fmt.Errorf("cellular %s MCC is invalid", field)
	}
	if network.MNC != "" && !cellularMNCPattern.MatchString(network.MNC) {
		return fmt.Errorf("cellular %s MNC is invalid", field)
	}
	if (network.MCC == "") != (network.MNC == "") {
		return fmt.Errorf("cellular %s MCC and MNC must be reported together", field)
	}
	if network.Availability == "available" && (network.Operator == "" && network.MCC == "" || network.MeasuredAt.IsZero()) {
		return fmt.Errorf("available cellular %s requires operator evidence and measured_at", field)
	}
	if network.Availability != "available" {
		network.Operator, network.MCC, network.MNC, network.MeasuredAt = "", "", "", time.Time{}
	}
	return nil
}

func normalizeAvailability(field, availability string) error {
	if oneOf(availability, "available", "unknown", "permission_denied", "unsupported") {
		return nil
	}
	return fmt.Errorf("cellular %s availability is invalid", field)
}

func oneOf(candidate string, values ...string) bool {
	for _, value := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func shouldReplaceCellular(previous, next *CellularObservation) bool {
	if next == nil {
		return false
	}
	if previous == nil {
		return true
	}
	if next.Generation == previous.Generation {
		return next.Sequence > previous.Sequence
	}
	return next.GenerationStartedAt.After(previous.GenerationStartedAt)
}

func cellularSnapshot(observation *CellularObservation, now time.Time, clientOffline bool) *CellularObservation {
	if observation == nil {
		return nil
	}
	result := *observation
	result.Stale = clientOffline || result.ReceivedAt.IsZero() || now.Sub(result.ReceivedAt) > cellularObservationFreshWindow
	return &result
}

func cellularStatus(observation *CellularObservation) string {
	if observation == nil {
		return "unknown"
	}
	if observation.Stale {
		return "stale"
	}
	return "observed"
}

func runtimeEvidence(provider string) string {
	switch strings.TrimSpace(provider) {
	case "asterisk_ami":
		return "Asterisk AMI"
	case "command":
		return "External telephony command"
	case "memory":
		return "In-memory test runtime"
	case "":
		return "Telephony runtime"
	default:
		return strings.TrimSpace(provider)
	}
}

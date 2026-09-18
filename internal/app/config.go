package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

type CommandSpec struct {
	Executable string            `json:"executable"`
	Args       []string          `json:"args"`
	Env        map[string]string `json:"env,omitempty"`
	Stdin      string            `json:"stdin,omitempty"`
}

type TelephonyConfig struct {
	Type        string      `json:"type"`
	Status      CommandSpec `json:"status,omitempty"`
	SendSMS     CommandSpec `json:"send_sms,omitempty"`
	StartCall   CommandSpec `json:"start_call,omitempty"`
	HangupCall  CommandSpec `json:"hangup_call,omitempty"`
	IncomingSMS CommandSpec `json:"incoming_sms,omitempty"`
	PollSeconds int         `json:"poll_seconds,omitempty"`
	AMI         AMIConfig   `json:"ami,omitempty"`
}

type AMIConfig struct {
	Address           string `json:"address,omitempty"`
	Username          string `json:"username,omitempty"`
	Secret            string `json:"secret,omitempty"`
	FromNumber        string `json:"from_number,omitempty"`
	Realm             string `json:"realm,omitempty"`
	SMSC              string `json:"smsc,omitempty"`
	AccessNetworkInfo string `json:"access_network_info,omitempty"`
}

type SIMAuthConfig struct {
	Type          string      `json:"type"`
	ADB           string      `json:"adb,omitempty"`
	ADBSerial     string      `json:"adb_serial,omitempty"`
	RemoteTimeout int         `json:"remote_timeout_seconds,omitempty"`
	Command       CommandSpec `json:"command,omitempty"`
}

type VoiceConfig struct {
	Enabled             bool               `json:"enabled"`
	WebSocketURL        string             `json:"websocket_url,omitempty"`
	BackendWebSocketURL string             `json:"backend_websocket_url,omitempty"`
	Domain              string             `json:"domain,omitempty"`
	Username            string             `json:"username,omitempty"`
	Password            string             `json:"password,omitempty"`
	RTPListenAddress    string             `json:"rtp_listen_address,omitempty"`
	RTPBackendAddress   string             `json:"rtp_backend_address,omitempty"`
	RTPStart            int                `json:"rtp_start,omitempty"`
	RTPEnd              int                `json:"rtp_end,omitempty"`
	ICEServers          []ICEServerConfig  `json:"ice_servers,omitempty"`
	EngineEventToken    string             `json:"engine_event_token,omitempty"`
	ServiceMedia        ServiceMediaConfig `json:"service_media,omitempty"`
}

// ServiceMediaConfig exposes a manager-owned WebRTC endpoint for trusted
// integrations. The fixed ICE port keeps container and host firewall rules
// predictable while each Asterisk bridge receives a separate ephemeral RTP
// socket on the private Docker network.
type ServiceMediaConfig struct {
	Enabled          bool   `json:"enabled"`
	ListenAddress    string `json:"listen_address,omitempty"`
	AdvertiseAddress string `json:"advertise_address,omitempty"`
	AsteriskRTPHost  string `json:"asterisk_rtp_host,omitempty"`
}

type ICEServerConfig struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type DiagnosticsConfig struct {
	Enabled   bool   `json:"enabled"`
	Directory string `json:"directory,omitempty"`
}

type RecordingsConfig struct {
	Directory string `json:"directory,omitempty"`
}

type Config struct {
	Listen            string            `json:"listen"`
	ExtraListen       []string          `json:"extra_listen,omitempty"`
	DataFile          string            `json:"data_file"`
	APIKey            string            `json:"api_key,omitempty"`
	Telephony         TelephonyConfig   `json:"telephony"`
	SIMAuth           SIMAuthConfig     `json:"sim_auth"`
	Voice             VoiceConfig       `json:"voice,omitempty"`
	ScreeningPipeline []string          `json:"screening_pipeline,omitempty"`
	Diagnostics       DiagnosticsConfig `json:"diagnostics,omitempty"`
	Recordings        RecordingsConfig  `json:"recordings,omitempty"`
}

func LoadConfig(path string) (Config, error) {
	cfg := Config{
		Listen:     "127.0.0.1:8787",
		DataFile:   "./data/state.json",
		Recordings: RecordingsConfig{Directory: "./data/recordings"},
		Telephony: TelephonyConfig{
			Type:        "memory",
			PollSeconds: 2,
		},
		SIMAuth: SIMAuthConfig{Type: "auto", RemoteTimeout: 35},
	}
	if strings.TrimSpace(path) != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, err
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if value := strings.TrimSpace(os.Getenv("VITI_LISTEN")); value != "" {
		cfg.Listen = value
	}
	if value := os.Getenv("VITI_API_KEY"); value != "" {
		cfg.APIKey = value
	}
	if value := os.Getenv("VITI_AMI_ADDRESS"); value != "" {
		cfg.Telephony.AMI.Address = value
	}
	if value := os.Getenv("VITI_AMI_USERNAME"); value != "" {
		cfg.Telephony.AMI.Username = value
	}
	if value := os.Getenv("VITI_AMI_SECRET"); value != "" {
		cfg.Telephony.AMI.Secret = value
	}
	if value := os.Getenv("VITI_AMI_FROM_NUMBER"); value != "" {
		cfg.Telephony.AMI.FromNumber = value
	}
	if value := os.Getenv("VITI_AMI_REALM"); value != "" {
		cfg.Telephony.AMI.Realm = value
	}
	if value := os.Getenv("VITI_AMI_SMSC"); value != "" {
		cfg.Telephony.AMI.SMSC = value
	}
	if value := os.Getenv("VITI_AMI_ACCESS_NETWORK_INFO"); value != "" {
		cfg.Telephony.AMI.AccessNetworkInfo = value
	}
	if strings.TrimSpace(cfg.Telephony.AMI.SMSC) == "" {
		if path := strings.TrimSpace(os.Getenv("VITI_AMI_INSTANCE_CONFIG")); path != "" {
			data, err := os.ReadFile(path)
			if err != nil {
				return Config{}, fmt.Errorf("read AMI carrier defaults: %w", err)
			}
			var carrier struct {
				SMSC string `json:"smsc"`
			}
			if err := json.Unmarshal(data, &carrier); err != nil {
				return Config{}, fmt.Errorf("parse AMI carrier defaults: %w", err)
			}
			cfg.Telephony.AMI.SMSC = strings.TrimSpace(carrier.SMSC)
		}
	}
	if value := os.Getenv("VITI_VOICE_WS_URL"); value != "" {
		cfg.Voice.WebSocketURL = value
	}
	if value := os.Getenv("VITI_VOICE_DOMAIN"); value != "" {
		cfg.Voice.Domain = value
	}
	if value := os.Getenv("VITI_VOICE_PASSWORD"); value != "" {
		cfg.Voice.Password = value
	}
	if value := strings.TrimSpace(os.Getenv("VITI_VOICE_ICE_URLS")); value != "" {
		urls := strings.Split(value, ",")
		cleaned := urls[:0]
		for _, url := range urls {
			if url = strings.TrimSpace(url); url != "" {
				cleaned = append(cleaned, url)
			}
		}
		cfg.Voice.ICEServers = []ICEServerConfig{{
			URLs:       cleaned,
			Username:   os.Getenv("VITI_VOICE_ICE_USERNAME"),
			Credential: os.Getenv("VITI_VOICE_ICE_CREDENTIAL"),
		}}
	}
	if value := strings.TrimSpace(os.Getenv("VITI_SERVICE_MEDIA_ENABLED")); value != "" {
		cfg.Voice.ServiceMedia.Enabled = envBool(value)
	}
	if value := strings.TrimSpace(os.Getenv("VITI_SERVICE_MEDIA_LISTEN")); value != "" {
		cfg.Voice.ServiceMedia.ListenAddress = value
	}
	if value := strings.TrimSpace(os.Getenv("VITI_SERVICE_MEDIA_ADVERTISE")); value != "" {
		cfg.Voice.ServiceMedia.AdvertiseAddress = value
	}
	if value := strings.TrimSpace(os.Getenv("VITI_SERVICE_MEDIA_ASTERISK_HOST")); value != "" {
		cfg.Voice.ServiceMedia.AsteriskRTPHost = value
	}
	if value := strings.TrimSpace(os.Getenv("VITI_SCREENING_PIPELINE")); value != "" {
		cfg.ScreeningPipeline = strings.Split(value, ",")
	}
	if value := os.Getenv("VITI_ENGINE_EVENT_TOKEN"); value != "" {
		cfg.Voice.EngineEventToken = value
	}
	if value := strings.TrimSpace(os.Getenv("VITI_DIAGNOSTICS_DIR")); value != "" {
		cfg.Diagnostics.Directory = value
	}
	if value := strings.TrimSpace(os.Getenv("VITI_DIAGNOSTICS_ENABLED")); value != "" {
		cfg.Diagnostics.Enabled = envBool(value)
	}
	if value := strings.TrimSpace(os.Getenv("VITI_RECORDINGS_DIR")); value != "" {
		cfg.Recordings.Directory = value
	}
	if value := os.Getenv("VITI_SIM_AUTH_TYPE"); value != "" {
		cfg.SIMAuth.Type = value
	}
	if value := os.Getenv("VITI_ADB"); value != "" {
		cfg.SIMAuth.ADB = value
	}
	if value := os.Getenv("VITI_ADB_SERIAL"); value != "" {
		cfg.SIMAuth.ADBSerial = value
	}
	if strings.TrimSpace(cfg.Listen) == "" {
		return Config{}, errors.New("listen address is empty")
	}
	if strings.TrimSpace(cfg.DataFile) == "" {
		return Config{}, errors.New("data_file is empty")
	}
	if strings.TrimSpace(cfg.Recordings.Directory) == "" {
		return Config{}, errors.New("recordings directory is empty")
	}
	if cfg.Telephony.PollSeconds <= 0 {
		cfg.Telephony.PollSeconds = 2
	}
	if cfg.SIMAuth.RemoteTimeout <= 0 {
		cfg.SIMAuth.RemoteTimeout = 35
	}
	if cfg.Voice.ServiceMedia.Enabled {
		if strings.TrimSpace(cfg.Voice.ServiceMedia.ListenAddress) == "" {
			return Config{}, errors.New("voice.service_media.listen_address is required when service media is enabled")
		}
		if strings.TrimSpace(cfg.Voice.ServiceMedia.AsteriskRTPHost) == "" {
			cfg.Voice.ServiceMedia.AsteriskRTPHost = "manager"
		}
	}
	seenScreeners := make(map[string]struct{}, len(cfg.ScreeningPipeline))
	for index, service := range cfg.ScreeningPipeline {
		service = strings.TrimSpace(service)
		if !integrationNamePattern.MatchString(service) {
			return Config{}, fmt.Errorf("screening_pipeline[%d] is not a valid service name", index)
		}
		if _, duplicate := seenScreeners[service]; duplicate {
			return Config{}, fmt.Errorf("screening_pipeline contains duplicate service %q", service)
		}
		seenScreeners[service] = struct{}{}
		cfg.ScreeningPipeline[index] = service
	}
	return cfg, nil
}

func envBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

package app

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// SMSDebugRequest is deliberately separate from the ordinary send contract.
// It is an expert-only wire editor: normal SMS submission always keeps using
// the carrier-tested automatic encoder.
type SMSDebugRequest struct {
	To                    string   `json:"to"`
	Text                  string   `json:"text"`
	Mode                  string   `json:"mode,omitempty"`
	Encoding              string   `json:"encoding,omitempty"`
	SMSC                  string   `json:"smsc,omitempty"`
	RequestURI            string   `json:"request_uri,omitempty"`
	FromURI               string   `json:"from_uri,omitempty"`
	PreferredIdentity     string   `json:"preferred_identity,omitempty"`
	AccessNetworkInfo     string   `json:"access_network_info,omitempty"`
	PID                   *int     `json:"pid,omitempty"`
	DCS                   *int     `json:"dcs,omitempty"`
	Validity              *int     `json:"validity,omitempty"`
	RequestDeliveryReport *bool    `json:"request_delivery_report,omitempty"`
	RPReference           *int     `json:"rp_reference,omitempty"`
	TPReference           *int     `json:"tp_reference,omitempty"`
	ConcatReference       *int     `json:"concat_reference,omitempty"`
	RawRPDUs              []string `json:"raw_rpdus,omitempty"`
}

type SMSDebugPreview struct {
	Request      SMSDebugRequest    `json:"request"`
	Encoding     string             `json:"encoding"`
	Multipart    bool               `json:"multipart"`
	Parts        []SMSDebugPart     `json:"parts"`
	SIP          SMSDebugSIPPreview `json:"sip"`
	LockedFields []string           `json:"locked_fields"`
	Warning      string             `json:"warning"`
}

type SMSDebugPart struct {
	Part        int    `json:"part"`
	Parts       int    `json:"parts"`
	RPReference int    `json:"rp_reference"`
	TPReference int    `json:"tp_reference"`
	RPDUHex     string `json:"rpdu_hex"`
	Octets      int    `json:"octets"`
}

type SMSDebugSIPPreview struct {
	Method             string   `json:"method"`
	RequestURI         string   `json:"request_uri"`
	From               string   `json:"from"`
	PreferredIdentity  string   `json:"preferred_identity"`
	AccessNetworkInfo  string   `json:"access_network_info,omitempty"`
	ContentType        string   `json:"content_type"`
	Require            string   `json:"require"`
	ProxyRequire       string   `json:"proxy_require"`
	Supported          string   `json:"supported"`
	Allow              string   `json:"allow"`
	RequestDisposition string   `json:"request_disposition"`
	AcceptContact      string   `json:"accept_contact"`
	ContentLengths     []int    `json:"content_lengths"`
	RuntimeGenerated   []string `json:"runtime_generated"`
}

type SMSDebugDefaults struct {
	FromNumber        string
	Realm             string
	SMSC              string
	AccessNetworkInfo string
}

var gsmDefaultAlphabet = []rune{
	'@', '£', '$', '¥', 'è', 'é', 'ù', 'ì', 'ò', 'ç', '\n', 'Ø', 'ø', '\r', 'Å', 'å',
	'Δ', '_', 'Φ', 'Γ', 'Λ', 'Ω', 'Π', 'Ψ', 'Σ', 'Θ', 'Ξ', ' ', 'Æ', 'æ', 'ß', 'É',
	' ', '!', '"', '#', '¤', '%', '&', '\'', '(', ')', '*', '+', ',', '-', '.', '/',
	'0', '1', '2', '3', '4', '5', '6', '7', '8', '9', ':', ';', '<', '=', '>', '?',
	'¡', 'A', 'B', 'C', 'D', 'E', 'F', 'G', 'H', 'I', 'J', 'K', 'L', 'M', 'N', 'O',
	'P', 'Q', 'R', 'S', 'T', 'U', 'V', 'W', 'X', 'Y', 'Z', 'Ä', 'Ö', 'Ñ', 'Ü', '§',
	'¿', 'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j', 'k', 'l', 'm', 'n', 'o',
	'p', 'q', 'r', 's', 't', 'u', 'v', 'w', 'x', 'y', 'z', 'ä', 'ö', 'ñ', 'ü', 'à',
}

var gsmEscapes = map[rune]byte{
	'\f': 0x0a, '^': 0x14, '{': 0x28, '}': 0x29, '\\': 0x2f,
	'[': 0x3c, '~': 0x3d, ']': 0x3e, '|': 0x40, '€': 0x65,
}

func buildSMSDebugPreview(request SMSDebugRequest, defaults SMSDebugDefaults) (SMSDebugPreview, error) {
	request.To = strings.TrimSpace(request.To)
	request.Mode = strings.ToLower(strings.TrimSpace(request.Mode))
	if request.Mode == "" {
		request.Mode = "structured"
	}
	if request.Mode != "structured" && request.Mode != "raw" {
		return SMSDebugPreview{}, errors.New("mode must be structured or raw")
	}
	if !e164Pattern.MatchString(request.To) {
		return SMSDebugPreview{}, errors.New("use an E.164 destination")
	}
	if strings.TrimSpace(request.Text) == "" || len([]rune(request.Text)) > 1000 {
		return SMSDebugPreview{}, errors.New("use 1–1000 message characters")
	}
	if request.SMSC = strings.TrimSpace(request.SMSC); request.SMSC == "" {
		request.SMSC = strings.TrimSpace(defaults.SMSC)
	}
	if request.SMSC == "" {
		return SMSDebugPreview{}, errors.New("the SMSC is not configured for protocol debugging")
	}
	if !validSMSAddress(request.SMSC) {
		return SMSDebugPreview{}, errors.New("SMSC must be a telephone address")
	}
	if request.RequestURI = strings.TrimSpace(request.RequestURI); request.RequestURI == "" {
		request.RequestURI = "sip:" + request.SMSC + "@" + defaults.Realm
	}
	if request.FromURI = strings.TrimSpace(request.FromURI); request.FromURI == "" {
		request.FromURI = "sip:" + defaults.FromNumber + "@" + defaults.Realm
	}
	if request.PreferredIdentity = strings.TrimSpace(request.PreferredIdentity); request.PreferredIdentity == "" {
		request.PreferredIdentity = request.FromURI
	}
	if request.AccessNetworkInfo = strings.TrimSpace(request.AccessNetworkInfo); request.AccessNetworkInfo == "" {
		request.AccessNetworkInfo = strings.TrimSpace(defaults.AccessNetworkInfo)
	}
	for name, value := range map[string]string{
		"request URI": request.RequestURI, "From URI": request.FromURI,
		"preferred identity": request.PreferredIdentity, "access network info": request.AccessNetworkInfo,
	} {
		if strings.ContainsAny(value, "\r\n") {
			return SMSDebugPreview{}, fmt.Errorf("%s cannot contain a line break", name)
		}
	}
	request.PID = defaultOctet(request.PID, 0x00)
	request.Validity = defaultOctet(request.Validity, 0xff)
	request.RPReference = defaultOctet(request.RPReference, 1)
	request.TPReference = defaultOctet(request.TPReference, 0)
	request.ConcatReference = defaultOctet(request.ConcatReference, 0)
	if request.RequestDeliveryReport == nil {
		value := true
		request.RequestDeliveryReport = &value
	}

	var parts []SMSDebugPart
	encodingName := "raw"
	if request.Mode == "raw" {
		if len(request.RawRPDUs) == 0 || len(request.RawRPDUs) > 16 {
			return SMSDebugPreview{}, errors.New("raw mode requires 1–16 RPDUs")
		}
		for index, raw := range request.RawRPDUs {
			data, err := decodeDebugHex(raw)
			if err != nil {
				return SMSDebugPreview{}, fmt.Errorf("RPDU %d: %w", index+1, err)
			}
			if len(data) < 4 || len(data) > 1024 {
				return SMSDebugPreview{}, fmt.Errorf("RPDU %d must contain 4–1024 octets", index+1)
			}
			parts = append(parts, SMSDebugPart{Part: index + 1, Parts: len(request.RawRPDUs), RPReference: int(data[1]), TPReference: debugTPReference(data), RPDUHex: strings.ToUpper(hex.EncodeToString(data)), Octets: len(data)})
		}
	} else {
		var err error
		parts, encodingName, err = buildStructuredRPDUs(request)
		if err != nil {
			return SMSDebugPreview{}, err
		}
		request.RawRPDUs = make([]string, len(parts))
		for index := range parts {
			request.RawRPDUs[index] = parts[index].RPDUHex
		}
		request.Encoding = encodingName
		if request.DCS == nil {
			value := map[string]int{"gsm7": 0x00, "8bit": 0x04, "ucs2": 0x08}[encodingName]
			request.DCS = &value
		}
	}

	lengths := make([]int, len(parts))
	for index := range parts {
		lengths[index] = parts[index].Octets
	}
	preview := SMSDebugPreview{
		Request: request, Encoding: encodingName, Multipart: len(parts) > 1, Parts: parts,
		SIP: SMSDebugSIPPreview{
			Method: "MESSAGE", RequestURI: request.RequestURI, From: request.FromURI,
			PreferredIdentity: request.PreferredIdentity, AccessNetworkInfo: request.AccessNetworkInfo,
			ContentType: "application/vnd.3gpp.sms", Require: "sec-agree", ProxyRequire: "sec-agree",
			Supported: "path, sec-agree", Allow: "MESSAGE", RequestDisposition: "no-fork",
			AcceptContact: "*;+g.3gpp.smsip", ContentLengths: lengths,
			RuntimeGenerated: []string{"Via", "Route", "Security-Verify", "Call-ID", "CSeq", "Contact", "Content-Length", "User-Agent"},
		},
		LockedFields: []string{"Via", "Route", "Security-Verify", "Call-ID", "CSeq", "Contact", "Content-Length", "User-Agent"},
		Warning:      "This sends the edited protocol bytes to the carrier. Invalid values may be rejected or billed as SMS traffic.",
	}
	return preview, nil
}

func defaultOctet(value *int, fallback int) *int {
	if value == nil {
		value = new(int)
		*value = fallback
	}
	return value
}

func validSMSAddress(value string) bool {
	digits := strings.TrimPrefix(strings.TrimSpace(value), "+")
	if digits == "" || len(digits) > 20 {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func validateOctet(name string, value *int) error {
	if value == nil || *value < 0 || *value > 255 {
		return fmt.Errorf("%s must be an octet (0–255)", name)
	}
	return nil
}

func buildStructuredRPDUs(request SMSDebugRequest) ([]SMSDebugPart, string, error) {
	for name, value := range map[string]*int{"PID": request.PID, "validity": request.Validity, "RP reference": request.RPReference, "TP reference": request.TPReference, "concatenation reference": request.ConcatReference} {
		if err := validateOctet(name, value); err != nil {
			return nil, "", err
		}
	}
	encodingName := strings.ToLower(strings.TrimSpace(request.Encoding))
	units := utf16.Encode([]rune(request.Text))
	if encodingName == "" || encodingName == "auto" {
		switch {
		case gsmCompatible(units):
			encodingName = "gsm7"
		case eightBitCompatible(units):
			encodingName = "8bit"
		default:
			encodingName = "ucs2"
		}
	}
	dcsDefault := map[string]int{"gsm7": 0x00, "8bit": 0x04, "ucs2": 0x08}
	if _, found := dcsDefault[encodingName]; !found {
		return nil, "", errors.New("encoding must be auto, gsm7, 8bit, or ucs2")
	}
	if request.DCS == nil {
		value := dcsDefault[encodingName]
		request.DCS = &value
	}
	if err := validateOctet("DCS", request.DCS); err != nil {
		return nil, "", err
	}
	segments, err := splitSMSUnits(units, encodingName)
	if err != nil {
		return nil, "", err
	}
	if len(segments) > 255 {
		return nil, "", errors.New("message requires more than 255 segments")
	}
	parts := make([]SMSDebugPart, 0, len(segments))
	for index, segment := range segments {
		rpRef := (*request.RPReference + index) & 0xff
		tpRef := (*request.TPReference + index) & 0xff
		buf := []byte{0x00, byte(rpRef), 0x00}
		smsc := packSMSAddress(request.SMSC)
		// RP address length counts the type-of-number octet plus BCD octets.
		smsc[0] = byte((int(smsc[0]) + 3) / 2)
		buf = append(buf, smsc...)
		tpdu := make([]byte, 0, 160)
		firstOctet := byte(0x11)
		if *request.RequestDeliveryReport {
			firstOctet |= 0x20
		}
		if len(segments) > 1 {
			firstOctet |= 0x40
		}
		tpdu = append(tpdu, firstOctet, byte(tpRef))
		tpdu = append(tpdu, packSMSAddress(request.To)...)
		tpdu = append(tpdu, byte(*request.PID), byte(*request.DCS), byte(*request.Validity))
		var udh []byte
		if len(segments) > 1 {
			udh = []byte{0x00, 0x03, byte(*request.ConcatReference), byte(len(segments)), byte(index + 1)}
		}
		userData, err := packSMSUserData(encodingName, segment, udh)
		if err != nil {
			return nil, "", err
		}
		tpdu = append(tpdu, userData...)
		if len(tpdu) > 255 {
			return nil, "", errors.New("TPDU exceeds one-octet RP length")
		}
		buf = append(buf, byte(len(tpdu)))
		buf = append(buf, tpdu...)
		parts = append(parts, SMSDebugPart{Part: index + 1, Parts: len(segments), RPReference: rpRef, TPReference: tpRef, RPDUHex: strings.ToUpper(hex.EncodeToString(buf)), Octets: len(buf)})
	}
	return parts, encodingName, nil
}

func packSMSAddress(value string) []byte {
	international := strings.HasPrefix(value, "+")
	digits := strings.TrimPrefix(value, "+")
	out := []byte{byte(len(digits)), 0x81}
	if international {
		out[1] = 0x91
	}
	for index := 0; index < len(digits); index += 2 {
		low := digits[index] - '0'
		high := byte(0x0f)
		if index+1 < len(digits) {
			high = digits[index+1] - '0'
		}
		out = append(out, low|(high<<4))
	}
	return out
}

func gsmCompatible(units []uint16) bool {
	for _, unit := range units {
		if _, _, ok := gsmCode(rune(unit)); !ok {
			return false
		}
	}
	return true
}

func eightBitCompatible(units []uint16) bool {
	for _, unit := range units {
		if unit > 255 {
			return false
		}
	}
	return true
}

func gsmCode(value rune) (byte, bool, bool) {
	for index, candidate := range gsmDefaultAlphabet {
		if candidate == value {
			return byte(index), false, true
		}
	}
	code, found := gsmEscapes[value]
	return code, true, found
}

func splitSMSUnits(units []uint16, encodingName string) ([][]uint16, error) {
	measure := func(unit uint16) (int, error) {
		switch encodingName {
		case "gsm7":
			_, escaped, ok := gsmCode(rune(unit))
			if !ok {
				return 0, fmt.Errorf("character U+%04X is not available in GSM 7-bit", unit)
			}
			if escaped {
				return 2, nil
			}
			return 1, nil
		case "8bit":
			if unit > 255 {
				return 0, fmt.Errorf("character U+%04X does not fit 8-bit data", unit)
			}
			return 1, nil
		default:
			return 1, nil
		}
	}
	singleLimit := map[string]int{"gsm7": 160, "8bit": 140, "ucs2": 70}[encodingName]
	multipartLimit := map[string]int{"gsm7": 153, "8bit": 134, "ucs2": 67}[encodingName]
	total := 0
	for _, unit := range units {
		amount, err := measure(unit)
		if err != nil {
			return nil, err
		}
		total += amount
	}
	if total <= singleLimit {
		return [][]uint16{units}, nil
	}
	var segments [][]uint16
	for start := 0; start < len(units); {
		end, used := start, 0
		for end < len(units) {
			amount, err := measure(units[end])
			if err != nil {
				return nil, err
			}
			if used+amount > multipartLimit {
				break
			}
			used += amount
			end++
		}
		if end == start {
			return nil, errors.New("could not split SMS text")
		}
		segments = append(segments, units[start:end])
		start = end
	}
	return segments, nil
}

func packSMSUserData(encodingName string, units []uint16, udh []byte) ([]byte, error) {
	switch encodingName {
	case "gsm7":
		septets := make([]byte, 0, len(units)+len(udh)+2)
		for _, unit := range units {
			code, escaped, ok := gsmCode(rune(unit))
			if !ok {
				return nil, fmt.Errorf("character U+%04X is not available in GSM 7-bit", unit)
			}
			if escaped {
				septets = append(septets, 0x1b)
			}
			septets = append(septets, code)
		}
		prefix := []byte(nil)
		fillBits := 0
		udl := len(septets)
		if len(udh) != 0 {
			prefix = append([]byte{byte(len(udh))}, udh...)
			headerSeptets := (len(prefix)*8 + 6) / 7
			udl += headerSeptets
			fillBits = headerSeptets*7 - len(prefix)*8
		}
		packed := append([]byte(nil), prefix...)
		bitPos := len(prefix)*8 + fillBits
		needed := (bitPos + len(septets)*7 + 7) / 8
		if needed > len(packed) {
			packed = append(packed, make([]byte, needed-len(packed))...)
		}
		for _, septet := range septets {
			byteIndex, shift := bitPos/8, bitPos%8
			packed[byteIndex] |= septet << shift
			if shift > 1 {
				packed[byteIndex+1] |= septet >> (8 - shift)
			}
			bitPos += 7
		}
		return append([]byte{byte(udl)}, packed...), nil
	case "8bit":
		data := append([]byte(nil), byte(len(udh)))
		if len(udh) == 0 {
			data = data[:0]
		} else {
			data = append(data, udh...)
		}
		for _, unit := range units {
			if unit > 255 {
				return nil, fmt.Errorf("character U+%04X does not fit 8-bit data", unit)
			}
			data = append(data, byte(unit))
		}
		return append([]byte{byte(len(data))}, data...), nil
	default:
		data := append([]byte(nil), byte(len(udh)))
		if len(udh) == 0 {
			data = data[:0]
		} else {
			data = append(data, udh...)
		}
		for _, unit := range units {
			data = append(data, byte(unit>>8), byte(unit))
		}
		return append([]byte{byte(len(data))}, data...), nil
	}
}

func decodeDebugHex(value string) ([]byte, error) {
	value = strings.NewReplacer(" ", "", "\n", "", "\r", "", "\t", "", ":", "").Replace(value)
	if value == "" || len(value)%2 != 0 {
		return nil, errors.New("hex must contain complete octets")
	}
	data, err := hex.DecodeString(value)
	if err != nil {
		return nil, errors.New("hex contains a non-hexadecimal character")
	}
	return data, nil
}

func debugTPReference(rpdu []byte) int {
	if len(rpdu) < 5 {
		return -1
	}
	rpDestinationLength := int(rpdu[3])
	tpduStart := 5 + rpDestinationLength
	if tpduStart+1 >= len(rpdu) {
		return -1
	}
	return int(rpdu[tpduStart+1])
}

package app

import (
	"errors"
	"regexp"
	"strings"

	"github.com/nyaruka/phonenumbers"
)

var publicDialCharacters = regexp.MustCompile(`^[+]?[0-9().\-\s]+$`)

var (
	errDialCountryRequired = errors.New("the current country could not be determined; enter the full international number beginning with +")
	errDialUnsupported     = errors.New("emergency numbers, short codes, and service codes are not supported")
	errDialInvalid         = errors.New("enter a valid phone number")
)

// normalizeDialDestination converts a user-entered public telephone number to
// the one canonical representation admitted to SIP. The country is transient
// client evidence, never a server default. Private extensions such as *43 and
// *44 deliberately bypass this function at their internal call sites.
func normalizeDialDestination(input, country string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", errDialInvalid
	}
	if strings.ContainsAny(input, "*#") {
		return "", errDialUnsupported
	}
	if !publicDialCharacters.MatchString(input) || strings.Count(input, "+") > 1 {
		return "", errDialInvalid
	}

	international := strings.HasPrefix(input, "+")
	country = strings.ToUpper(strings.TrimSpace(country))
	if !international {
		if len(country) != 2 || phonenumbers.GetCountryCodeForRegion(country) == 0 {
			return "", errDialCountryRequired
		}
		if phonenumbers.ConnectsToEmergencyNumber(input, country) {
			return "", errDialUnsupported
		}
	} else {
		// A leading plus already carries its own country context. Ignoring a stale
		// or malformed client hint keeps explicit international dialing available.
		country = ""
	}

	parsed, err := phonenumbers.Parse(input, country)
	if err != nil {
		return "", errDialInvalid
	}
	if country != "" && (phonenumbers.IsPossibleShortNumberForRegion(parsed, country) || phonenumbers.IsValidShortNumberForRegion(parsed, country)) {
		return "", errDialUnsupported
	}
	if !phonenumbers.IsValidNumber(parsed) {
		return "", errDialInvalid
	}
	normalized := phonenumbers.Format(parsed, phonenumbers.E164)
	if !e164Pattern.MatchString(normalized) {
		return "", errDialInvalid
	}
	return normalized, nil
}

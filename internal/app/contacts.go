package app

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"unicode"

	"github.com/nyaruka/phonenumbers"
)

const maxContactSourceRecords = 20_000

func normalizeContactNumber(number, region string) string {
	number = strings.TrimSpace(number)
	if number == "" {
		return ""
	}
	region = strings.ToUpper(strings.TrimSpace(region))
	parsed, err := phonenumbers.Parse(number, region)
	if err != nil || !phonenumbers.IsValidNumber(parsed) {
		return ""
	}
	return phonenumbers.Format(parsed, phonenumbers.E164)
}

func compactPhoneNumber(number string) string {
	var result strings.Builder
	for _, value := range number {
		if (value == '+' && result.Len() == 0) || unicode.IsDigit(value) {
			result.WriteRune(value)
		}
	}
	return result.String()
}

func normalizedSourceNumber(number ContactNumber, region string) ContactNumber {
	number.Original = strings.TrimSpace(number.Original)
	number.Normalized = normalizeContactNumber(number.Normalized, region)
	if number.Normalized == "" {
		number.Normalized = normalizeContactNumber(number.Original, region)
	}
	number.Label = strings.TrimSpace(number.Label)
	return number
}

func mergedContacts(records []ContactSourceRecord, query string) []Contact {
	if len(records) == 0 {
		return []Contact{}
	}
	sort.Slice(records, func(i, j int) bool {
		left := records[i].SourceClientName + "\x00" + records[i].SourceClientID + "\x00" + records[i].SourceContactID
		right := records[j].SourceClientName + "\x00" + records[j].SourceClientID + "\x00" + records[j].SourceContactID
		return left < right
	})
	parent := make([]int, len(records))
	for index := range parent {
		parent[index] = index
		for numberIndex, number := range records[index].Numbers {
			records[index].Numbers[numberIndex] = normalizedSourceNumber(number, records[index].Region)
		}
	}
	var find func(int) int
	find = func(value int) int {
		if parent[value] != value {
			parent[value] = find(parent[value])
		}
		return parent[value]
	}
	union := func(left, right int) {
		left, right = find(left), find(right)
		if left != right {
			parent[right] = left
		}
	}
	owners := map[string]int{}
	for index, record := range records {
		for _, number := range record.Numbers {
			if number.Normalized == "" {
				continue
			}
			if owner, exists := owners[number.Normalized]; exists {
				union(index, owner)
			} else {
				owners[number.Normalized] = index
			}
		}
	}
	groups := map[int][]ContactSourceRecord{}
	for index, record := range records {
		root := find(index)
		groups[root] = append(groups[root], record)
	}
	result := make([]Contact, 0, len(groups))
	for _, group := range groups {
		contact := mergeContactGroup(group)
		if contactMatches(contact, query) {
			result = append(result, contact)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if strings.EqualFold(result[i].Name, result[j].Name) {
			return result[i].ID < result[j].ID
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result
}

func mergeContactGroup(records []ContactSourceRecord) Contact {
	contact := Contact{}
	seenNames := map[string]bool{}
	seenNumbers := map[string]bool{}
	seenSources := map[string]bool{}
	identityParts := make([]string, 0)
	for _, record := range records {
		for _, name := range record.Names {
			name = strings.TrimSpace(name)
			key := strings.ToLower(name)
			if name != "" && !seenNames[key] {
				seenNames[key] = true
				contact.Names = append(contact.Names, name)
			}
		}
		for _, number := range record.Numbers {
			key := number.Normalized
			if key == "" {
				key = record.SourceClientID + "\x00" + record.SourceContactID + "\x00" + compactPhoneNumber(number.Original)
			}
			if key == "" || seenNumbers[key] {
				continue
			}
			seenNumbers[key] = true
			contact.Numbers = append(contact.Numbers, number)
			identityParts = append(identityParts, key)
		}
		if !seenSources[record.SourceClientID] {
			seenSources[record.SourceClientID] = true
			contact.Sources = append(contact.Sources, ContactSource{ClientID: record.SourceClientID, ClientName: record.SourceClientName})
		}
	}
	if len(contact.Names) > 0 {
		contact.Name = contact.Names[0]
	} else if len(contact.Numbers) > 0 {
		contact.Name = contact.Numbers[0].Original
	}
	sort.Strings(identityParts)
	digest := sha256.Sum256([]byte(strings.Join(identityParts, "\x00")))
	contact.ID = "contact-" + hex.EncodeToString(digest[:8])
	sort.Slice(contact.Sources, func(i, j int) bool {
		if contact.Sources[i].ClientName == contact.Sources[j].ClientName {
			return contact.Sources[i].ClientID < contact.Sources[j].ClientID
		}
		return contact.Sources[i].ClientName < contact.Sources[j].ClientName
	})
	return contact
}

func contactMatches(contact Contact, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	for _, name := range contact.Names {
		if strings.Contains(strings.ToLower(name), query) {
			return true
		}
	}
	compactQuery := compactPhoneNumber(query)
	for _, number := range contact.Numbers {
		if strings.Contains(strings.ToLower(number.Original), query) || compactQuery != "" && strings.Contains(number.Normalized, compactQuery) {
			return true
		}
	}
	return false
}

func (a *App) contacts(w http.ResponseWriter, r *http.Request) {
	contacts, version := a.store.ContactsSnapshot(r.URL.Query().Get("q"))
	writeJSON(w, http.StatusOK, map[string]any{
		"contacts": contacts,
		"version":  version,
	})
}

func (a *App) replaceContactSource(w http.ResponseWriter, r *http.Request) {
	clientID := strings.TrimSpace(r.PathValue("client"))
	var request struct {
		ClientName string                `json:"client_name"`
		Region     string                `json:"region"`
		Revision   int64                 `json:"snapshot_revision"`
		Contacts   []ContactSourceRecord `json:"contacts"`
	}
	if err := readJSON(r, &request, 8<<20); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if clientID == "" || len(request.Contacts) > maxContactSourceRecords {
		writeError(w, http.StatusBadRequest, "invalid contact source")
		return
	}
	request.ClientName = strings.TrimSpace(request.ClientName)
	if request.ClientName == "" {
		request.ClientName = a.knownClientName(clientID)
	}
	if request.ClientName == "" {
		request.ClientName = clientID
	}
	request.Region = strings.ToUpper(strings.TrimSpace(request.Region))
	if len(request.Region) != 2 {
		request.Region = ""
	}
	clean := make([]ContactSourceRecord, 0, len(request.Contacts))
	seen := map[string]bool{}
	for _, record := range request.Contacts {
		record.SourceContactID = strings.TrimSpace(record.SourceContactID)
		if record.SourceContactID == "" || seen[record.SourceContactID] {
			continue
		}
		seen[record.SourceContactID] = true
		record.Names = cleanStrings(record.Names, 12)
		numbers := make([]ContactNumber, 0, len(record.Numbers))
		for _, number := range record.Numbers {
			number.Original = strings.TrimSpace(number.Original)
			number.Normalized = strings.TrimSpace(number.Normalized)
			if number.Original != "" || number.Normalized != "" {
				numbers = append(numbers, number)
			}
		}
		if len(numbers) == 0 {
			continue
		}
		record.Numbers = numbers
		clean = append(clean, record)
	}
	changed, err := a.store.ReplaceContactSource(clientID, request.ClientName, request.Region, request.Revision, clean)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not persist contacts")
		return
	}
	if changed {
		a.events.Publish("contacts.updated", map[string]any{"client_id": clientID, "count": len(clean)})
	}
	w.WriteHeader(http.StatusNoContent)
}

func cleanStrings(values []string, limit int) []string {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if value != "" && !seen[key] {
			seen[key] = true
			result = append(result, value)
			if len(result) == limit {
				break
			}
		}
	}
	return result
}

func resolveContact(contacts []Contact, number string) (Contact, bool) {
	normalized := normalizeContactNumber(number, "")
	compact := compactPhoneNumber(number)
	for _, contact := range contacts {
		for _, candidate := range contact.Numbers {
			if (normalized != "" && candidate.Normalized == normalized) ||
				(normalized == "" && compact != "" && compactPhoneNumber(candidate.Original) == compact) {
				return contact, true
			}
		}
	}
	return Contact{}, false
}

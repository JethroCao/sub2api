package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openAIAccountInstructionsRedaction = "[redacted account instructions]"

func appendOpenAIAccountInstructions(account *Account, body []byte) ([]byte, bool, error) {
	suffix := account.GetOpenAICustomInstructions()
	if suffix == "" {
		return body, false, nil
	}

	current := gjson.GetBytes(body, "instructions")
	if current.Exists() && current.Type != gjson.String {
		return body, false, errors.New("OpenAI instructions must be a string")
	}

	existingRaw := current.String()
	existingTrimmed := strings.TrimSpace(existingRaw)
	if existingTrimmed == suffix || strings.HasSuffix(existingTrimmed, "\n\n"+suffix) {
		return body, false, nil
	}

	combined := suffix
	if existingTrimmed != "" {
		combined = existingRaw + "\n\n" + suffix
	}
	next, err := sjson.SetBytes(body, "instructions", combined)
	return next, err == nil, err
}

// redactOpenAIAccountInstructionsFromUpstreamBody removes only the configured
// account suffix from an upstream response. Upstreams can echo request
// instructions in error messages, so this must run before the response body is
// used for client errors, operations diagnostics, failover errors, or logs.
// Client-provided instructions are deliberately not used as redaction terms.
func redactOpenAIAccountInstructionsFromUpstreamBody(account *Account, body []byte) []byte {
	if account == nil || len(body) == 0 {
		return body
	}
	suffix := account.GetOpenAICustomInstructions()
	if suffix == "" {
		return body
	}

	// Decode valid JSON so equivalent escape forms (for example / vs \/ and a
	// literal Unicode rune vs \uXXXX) are redacted by their string value.
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err == nil {
		if redactedPayload, changed := redactOpenAIAccountInstructionsJSONValue(payload, suffix); changed {
			if redacted, marshalErr := json.Marshal(redactedPayload); marshalErr == nil {
				return redacted
			}
		}
	}

	// Preserve non-JSON error bodies while still removing exact literal and
	// JSON-escaped renderings of the configured suffix. WebSocket close errors
	// commonly wrap an escaped JSON reason inside otherwise non-JSON text.
	redacted := string(body)
	terms := []string{suffix}
	if quoted, err := json.Marshal(suffix); err == nil && len(quoted) >= 2 {
		terms = appendOpenAIAccountInstructionsEscapedRedactionTerms(terms, string(quoted[1:len(quoted)-1]))
	}
	if quoted := strconv.QuoteToASCII(suffix); len(quoted) >= 2 {
		terms = appendOpenAIAccountInstructionsEscapedRedactionTerms(terms, quoted[1:len(quoted)-1])
	}
	for _, term := range terms {
		redacted = strings.ReplaceAll(redacted, term, openAIAccountInstructionsRedaction)
	}
	return []byte(redacted)
}

func appendOpenAIAccountInstructionsEscapedRedactionTerms(terms []string, encoded string) []string {
	if encoded == "" {
		return terms
	}
	terms = append(terms, encoded)
	if slashEscaped := strings.ReplaceAll(encoded, "/", `\/`); slashEscaped != encoded {
		terms = append(terms, slashEscaped)
	}
	return terms
}

func redactOpenAIAccountInstructionsFromUpstreamText(account *Account, text string) string {
	if text == "" {
		return text
	}
	return string(redactOpenAIAccountInstructionsFromUpstreamBody(account, []byte(text)))
}

func redactOpenAIAccountInstructionsFromUpstreamError(account *Account, err error) error {
	if err == nil {
		return nil
	}
	redacted := redactOpenAIAccountInstructionsFromUpstreamText(account, err.Error())
	if redacted == err.Error() {
		return err
	}
	return errors.New(redacted)
}

func redactOpenAIAccountInstructionsJSONValue(value any, suffix string) (any, bool) {
	switch typed := value.(type) {
	case string:
		redacted := strings.ReplaceAll(typed, suffix, openAIAccountInstructionsRedaction)
		return redacted, redacted != typed
	case []any:
		changed := false
		for i, item := range typed {
			redacted, itemChanged := redactOpenAIAccountInstructionsJSONValue(item, suffix)
			if itemChanged {
				typed[i] = redacted
				changed = true
			}
		}
		return typed, changed
	case map[string]any:
		changed := false
		redactedMap := make(map[string]any, len(typed))
		for key, item := range typed {
			redactedKey := strings.ReplaceAll(key, suffix, openAIAccountInstructionsRedaction)
			redactedItem, itemChanged := redactOpenAIAccountInstructionsJSONValue(item, suffix)
			redactedMap[redactedKey] = redactedItem
			if redactedKey != key || itemChanged {
				changed = true
			}
		}
		if changed {
			return redactedMap, true
		}
		return typed, false
	default:
		return value, false
	}
}

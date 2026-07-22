package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

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
		redactedPayload, changed := redactOpenAIAccountInstructionsJSONValue(payload, suffix)
		if !changed {
			return body
		}
		if redacted, marshalErr := json.Marshal(redactedPayload); marshalErr == nil {
			return redacted
		}
	}

	// Preserve non-JSON error bodies. Replace the exact configured bytes before
	// interpreting JSON-style escapes: a configured literal `\n` must not be
	// mistaken for a newline while searching for the configured suffix. Then
	// retain semantic matching for upstreams that encoded the same string value
	// with JSON escapes, mixed literal runes, or surrogate pairs.
	redacted := strings.ReplaceAll(string(body), suffix, openAIAccountInstructionsRedaction)
	return []byte(redactOpenAIAccountInstructionsJSONStyleText(redacted, suffix))
}

func redactOpenAIAccountInstructionsJSONStyleText(text, suffix string) string {
	if text == "" || suffix == "" {
		return text
	}
	target := []rune(suffix)
	var redacted strings.Builder
	redacted.Grow(len(text))
	changed := false
	for offset := 0; offset < len(text); {
		if end, ok := matchOpenAIAccountInstructionsJSONStyleRunes(text, offset, target); ok {
			redacted.WriteString(openAIAccountInstructionsRedaction)
			offset = end
			changed = true
			continue
		}
		_, next := decodeOpenAIAccountInstructionsJSONStyleRune(text, offset)
		redacted.WriteString(text[offset:next])
		offset = next
	}
	if !changed {
		return text
	}
	return redacted.String()
}

func matchOpenAIAccountInstructionsJSONStyleRunes(text string, offset int, target []rune) (int, bool) {
	for _, want := range target {
		if offset >= len(text) {
			return offset, false
		}
		got, next := decodeOpenAIAccountInstructionsJSONStyleRune(text, offset)
		if got != want {
			return offset, false
		}
		offset = next
	}
	return offset, true
}

func decodeOpenAIAccountInstructionsJSONStyleRune(text string, offset int) (rune, int) {
	if offset >= len(text) {
		return utf8.RuneError, offset
	}
	if text[offset] != '\\' || offset+1 >= len(text) {
		r, size := utf8.DecodeRuneInString(text[offset:])
		return r, offset + size
	}

	switch text[offset+1] {
	case '"':
		return '"', offset + 2
	case '\\':
		return '\\', offset + 2
	case '/':
		return '/', offset + 2
	case 'b':
		return '\b', offset + 2
	case 'f':
		return '\f', offset + 2
	case 'n':
		return '\n', offset + 2
	case 'r':
		return '\r', offset + 2
	case 't':
		return '\t', offset + 2
	case 'u':
		first, ok := parseOpenAIAccountInstructionsJSONHexRune(text, offset)
		if !ok {
			return '\\', offset + 1
		}
		next := offset + 6
		if utf16.IsSurrogate(first) {
			if first >= 0xD800 && first <= 0xDBFF && next+6 <= len(text) && text[next] == '\\' && text[next+1] == 'u' {
				second, secondOK := parseOpenAIAccountInstructionsJSONHexRune(text, next)
				if secondOK && second >= 0xDC00 && second <= 0xDFFF {
					return utf16.DecodeRune(first, second), next + 6
				}
			}
			return utf8.RuneError, next
		}
		return first, next
	default:
		return '\\', offset + 1
	}
}

func parseOpenAIAccountInstructionsJSONHexRune(text string, offset int) (rune, bool) {
	if offset+6 > len(text) || text[offset] != '\\' || text[offset+1] != 'u' {
		return 0, false
	}
	value, err := strconv.ParseUint(text[offset+2:offset+6], 16, 16)
	if err != nil {
		return 0, false
	}
	return rune(value), true
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

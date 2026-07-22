package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	coderws "github.com/coder/websocket"
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

	// Validate the complete document before taking the JSON path. Decode every
	// raw JSON string independently instead of unmarshalling into maps: maps
	// collapse duplicate keys and could leave an earlier secret occurrence in
	// the original body. This also preserves an entirely nonmatching document
	// byte-for-byte, including duplicate keys and whitespace.
	if json.Valid(body) {
		if redacted, changed, err := redactOpenAIAccountInstructionsJSONStrings(body, suffix); err == nil {
			if !changed {
				return body
			}
			return redacted
		}
	}

	// Preserve non-JSON error bodies. Replace the exact configured bytes before
	// interpreting JSON-style escapes: a configured literal `\n` must not be
	// mistaken for a newline while searching for the configured suffix. Then
	// retain semantic matching for upstreams that encoded the same string value
	// with JSON escapes, mixed literal runes, or surrogate pairs.
	replacement := safeOpenAIAccountInstructionsRedaction(suffix)
	redacted := strings.ReplaceAll(string(body), suffix, replacement)
	return []byte(redactOpenAIAccountInstructionsJSONStyleText(redacted, suffix, replacement))
}

func redactOpenAIAccountInstructionsJSONStrings(body []byte, suffix string) ([]byte, bool, error) {
	replacement := safeOpenAIAccountInstructionsRedaction(suffix)
	var redacted bytes.Buffer
	lastWritten := 0
	changed := false

	for offset := 0; offset < len(body); {
		if body[offset] != '"' {
			offset++
			continue
		}

		start := offset
		offset++
		for offset < len(body) {
			switch body[offset] {
			case '\\':
				offset += 2
			case '"':
				offset++
				goto stringComplete
			default:
				offset++
			}
		}
		return nil, false, errors.New("unterminated JSON string")

	stringComplete:
		var decoded string
		if err := json.Unmarshal(body[start:offset], &decoded); err != nil {
			return nil, false, err
		}
		next := strings.ReplaceAll(decoded, suffix, replacement)
		if next == decoded {
			continue
		}

		encoded, err := json.Marshal(next)
		if err != nil {
			return nil, false, err
		}
		redacted.Write(body[lastWritten:start])
		redacted.Write(encoded)
		lastWritten = offset
		changed = true
	}

	if !changed {
		return body, false, nil
	}
	redacted.Write(body[lastWritten:])
	return redacted.Bytes(), true, nil
}

func redactOpenAIAccountInstructionsJSONStyleText(text, suffix, replacement string) string {
	if text == "" || suffix == "" {
		return text
	}
	target := []rune(suffix)
	if len(target) == 0 {
		return text
	}

	// Decode the source exactly once while retaining byte spans. KMP then finds
	// semantic matches in O(source+suffix), including mixed literal/JSON-escaped
	// text, without the repeated-prefix quadratic behavior of probing the full
	// suffix at every source byte.
	type decodedUnit struct {
		r     rune
		start int
		end   int
	}
	units := make([]decodedUnit, 0, utf8.RuneCountInString(text))
	for offset := 0; offset < len(text); {
		r, next := decodeOpenAIAccountInstructionsJSONStyleRune(text, offset)
		units = append(units, decodedUnit{r: r, start: offset, end: next})
		offset = next
	}

	failure := make([]int, len(target))
	for i, matched := 1, 0; i < len(target); i++ {
		for matched > 0 && target[i] != target[matched] {
			matched = failure[matched-1]
		}
		if target[i] == target[matched] {
			matched++
		}
		failure[i] = matched
	}

	var redacted strings.Builder
	redacted.Grow(len(text))
	lastWritten := 0
	matched := 0
	changed := false
	for i, unit := range units {
		for matched > 0 && unit.r != target[matched] {
			matched = failure[matched-1]
		}
		if unit.r == target[matched] {
			matched++
		}
		if matched != len(target) {
			continue
		}
		start := units[i-len(target)+1].start
		redacted.WriteString(text[lastWritten:start])
		redacted.WriteString(replacement)
		lastWritten = unit.end
		changed = true
		// Match strings.ReplaceAll semantics: matches never overlap.
		matched = 0
	}
	if !changed {
		return text
	}
	redacted.WriteString(text[lastWritten:])
	return redacted.String()
}

func safeOpenAIAccountInstructionsRedaction(suffix string) string {
	if suffix != "" && !strings.Contains(openAIAccountInstructionsRedaction, suffix) {
		return openAIAccountInstructionsRedaction
	}
	// The configured value is capped at 16 KiB, so at least one private-use
	// rune is absent. A one-rune marker that is not itself the one-rune suffix
	// cannot contain any nonempty multi-rune suffix either.
	for candidate := rune(0xE000); candidate <= 0xF8FF; candidate++ {
		marker := string(candidate)
		if marker != suffix {
			return marker
		}
	}
	return "" // unreachable for a validated configured suffix
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
	wrapped := &redactedOpenAIAccountInstructionsError{message: redacted, cause: err}
	var closeErr coderws.CloseError
	if errors.As(err, &closeErr) {
		closeErr.Reason = redactOpenAIAccountInstructionsFromUpstreamText(account, closeErr.Reason)
		wrapped.closeErr = &closeErr
	}
	return wrapped
}

// redactedOpenAIAccountInstructionsError changes only presentation. Unwrap
// preserves cancellation, timeout, and sentinel identity for retry/failover
// classification. As intercepts coderws.CloseError so consumers see the same
// close code with a sanitized reason rather than rediscovering the private
// reason through the original error chain.
type redactedOpenAIAccountInstructionsError struct {
	message  string
	cause    error
	closeErr *coderws.CloseError
}

func (e *redactedOpenAIAccountInstructionsError) Error() string { return e.message }
func (e *redactedOpenAIAccountInstructionsError) Unwrap() error { return e.cause }

func (e *redactedOpenAIAccountInstructionsError) As(target any) bool {
	if e == nil || e.closeErr == nil {
		return false
	}
	closeTarget, ok := target.(*coderws.CloseError)
	if !ok || closeTarget == nil {
		return false
	}
	*closeTarget = *e.closeErr
	return true
}

package service

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeOpenAIResponsesMessagePartialForAccount adds the explicit
// partial=false field required by some OpenAI-compatible Responses providers.
// Existing values are preserved so clients can still submit partial messages.
func normalizeOpenAIResponsesMessagePartialForAccount(
	account *Account,
	body []byte,
) ([]byte, bool, error) {
	if !account.ShouldEnableOpenAIResponsesMessagePartialCompat() {
		return body, false, nil
	}
	if !gjson.ValidBytes(body) {
		return body, false, fmt.Errorf("normalize OpenAI Responses message partial compatibility: invalid request JSON")
	}

	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, false, nil
	}

	normalized := body
	changed := false
	for index, item := range input.Array() {
		if !item.IsObject() ||
			!strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "message") ||
			item.Get("partial").Exists() {
			continue
		}
		var err error
		normalized, err = sjson.SetBytes(normalized, fmt.Sprintf("input.%d.partial", index), false)
		if err != nil {
			return body, false, fmt.Errorf(
				"normalize OpenAI Responses input.%d partial compatibility: %w",
				index,
				err,
			)
		}
		changed = true
	}
	return normalized, changed, nil
}

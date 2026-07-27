package service

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeOpenAIResponsesMessagePartialForAccount normalizes the explicit
// partial field required by some OpenAI-compatible Responses providers. Ark
// requires the final assistant message to be partial=true and every other
// message to be partial=false.
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

	items := input.Array()
	lastAssistantIndex := -1
	for index, item := range items {
		if isOpenAIResponsesMessage(item) &&
			strings.EqualFold(strings.TrimSpace(item.Get("role").String()), "assistant") {
			lastAssistantIndex = index
		}
	}

	normalized := body
	changed := false
	for index, item := range items {
		if !isOpenAIResponsesMessage(item) {
			continue
		}
		wantPartial := index == lastAssistantIndex
		partial := item.Get("partial")
		if (wantPartial && partial.Type == gjson.True) ||
			(!wantPartial && partial.Type == gjson.False) {
			continue
		}
		var err error
		normalized, err = sjson.SetBytes(normalized, fmt.Sprintf("input.%d.partial", index), wantPartial)
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

func isOpenAIResponsesMessage(item gjson.Result) bool {
	return item.IsObject() &&
		strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "message")
}

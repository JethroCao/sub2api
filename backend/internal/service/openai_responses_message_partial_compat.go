package service

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeOpenAIResponsesMessagePartialForAccount normalizes the explicit
// partial field required by some OpenAI-compatible Responses providers. Ark
// only permits the field on the final message item, where the field must be
// true. Non-message items may follow that message.
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
	lastMessageIndex := -1
	for index, item := range items {
		if isOpenAIResponsesMessage(item) {
			lastMessageIndex = index
		}
	}

	normalized := body
	changed := false
	for index, item := range items {
		if !isOpenAIResponsesMessage(item) {
			continue
		}
		wantPartial := index == lastMessageIndex
		partial := item.Get("partial")
		var err error
		switch {
		case wantPartial && partial.Type != gjson.True:
			normalized, err = sjson.SetBytes(normalized, fmt.Sprintf("input.%d.partial", index), true)
		case !wantPartial && partial.Exists():
			normalized, err = sjson.DeleteBytes(normalized, fmt.Sprintf("input.%d.partial", index))
		default:
			continue
		}
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
	if !item.IsObject() {
		return false
	}
	itemType := strings.TrimSpace(item.Get("type").String())
	if strings.EqualFold(itemType, "message") {
		return true
	}
	// Responses also accepts EasyInputMessage objects where type is omitted
	// and role/content alone identify the item as a message.
	return itemType == "" && strings.TrimSpace(item.Get("role").String()) != ""
}

package service

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var openAIResponsesAssistantPrefillContinuation = []byte(
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"Continue."}]}`,
)

// normalizeOpenAIResponsesAssistantPrefillForAccount preserves an assistant
// message that is the final Responses input item and appends a minimal user
// continuation for upstream providers that do not support assistant prefill.
func normalizeOpenAIResponsesAssistantPrefillForAccount(
	account *Account,
	body []byte,
) ([]byte, bool, error) {
	if !account.ShouldEnableOpenAIResponsesAssistantPrefillCompat() {
		return body, false, nil
	}
	if !gjson.ValidBytes(body) {
		return body, false, fmt.Errorf("normalize OpenAI Responses assistant prefill compatibility: invalid request JSON")
	}

	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, false, nil
	}
	items := input.Array()
	if len(items) == 0 {
		return body, false, nil
	}
	last := items[len(items)-1]
	if !isOpenAIResponsesMessage(last) ||
		!strings.EqualFold(strings.TrimSpace(last.Get("role").String()), "assistant") {
		return body, false, nil
	}

	normalized, err := sjson.SetRawBytes(body, "input.-1", openAIResponsesAssistantPrefillContinuation)
	if err != nil {
		return body, false, fmt.Errorf("append OpenAI Responses assistant prefill continuation: %w", err)
	}
	return normalized, true, nil
}

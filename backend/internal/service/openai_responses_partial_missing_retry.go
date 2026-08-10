package service

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func normalizeOpenAIResponsesPartialMissingRetryBody(
	account *Account,
	statusCode int,
	upstreamMsg string,
	requestBody []byte,
	upstreamBody []byte,
) ([]byte, bool, error) {
	if !account.ShouldEnableOpenAIResponsesMessagePartialCompat() ||
		!isOpenAIPartialMissingError(statusCode, upstreamMsg, upstreamBody) {
		return requestBody, false, nil
	}
	if !gjson.ValidBytes(requestBody) {
		return requestBody, false, fmt.Errorf("normalize partial-missing retry: invalid request JSON")
	}

	input := gjson.GetBytes(requestBody, "input")
	if !input.IsArray() {
		return requestBody, false, nil
	}

	lastAssistantIndex := -1
	for index, item := range input.Array() {
		if isOpenAIResponsesMessage(item) &&
			strings.EqualFold(strings.TrimSpace(item.Get("role").String()), "assistant") {
			lastAssistantIndex = index
		}
	}
	if lastAssistantIndex < 0 {
		return requestBody, false, nil
	}

	partialPath := fmt.Sprintf("input.%d.partial", lastAssistantIndex)
	if gjson.GetBytes(requestBody, partialPath).Type == gjson.True {
		return requestBody, false, nil
	}

	retryBody, err := sjson.SetBytes(requestBody, partialPath, true)
	if err != nil {
		return requestBody, false, fmt.Errorf(
			"normalize partial-missing retry input.%d: %w",
			lastAssistantIndex,
			err,
		)
	}
	return retryBody, true, nil
}

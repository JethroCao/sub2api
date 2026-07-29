package service

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeOpenAIResponsesMessageContentForAccount removes empty historical
// text parts before forwarding to providers with Ark-compatible Responses
// validation. OpenAI accepts empty output_text history entries, while Ark
// rejects them as a missing input.content.text parameter.
func normalizeOpenAIResponsesMessageContentForAccount(
	account *Account,
	body []byte,
) ([]byte, bool, error) {
	if account == nil || (!account.ShouldEnableOpenAIResponsesMessagePartialCompat() &&
		!account.ShouldEnableOpenAIResponsesAssistantPrefillCompat()) {
		return body, false, nil
	}
	if !gjson.ValidBytes(body) {
		return body, false, fmt.Errorf("normalize OpenAI Responses message content compatibility: invalid request JSON")
	}

	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, false, nil
	}

	normalized := body
	changed := false
	items := input.Array()
	for inputIndex := len(items) - 1; inputIndex >= 0; inputIndex-- {
		item := items[inputIndex]
		if !isOpenAIResponsesMessage(item) {
			continue
		}

		content := item.Get("content")
		if !content.Exists() || content.Type == gjson.Null {
			var err error
			normalized, err = sjson.DeleteBytes(normalized, fmt.Sprintf("input.%d", inputIndex))
			if err != nil {
				return body, false, fmt.Errorf("remove empty OpenAI Responses input.%d message: %w", inputIndex, err)
			}
			changed = true
			continue
		}
		if content.Type == gjson.String {
			if strings.TrimSpace(content.String()) != "" {
				continue
			}
			var err error
			normalized, err = sjson.DeleteBytes(normalized, fmt.Sprintf("input.%d", inputIndex))
			if err != nil {
				return body, false, fmt.Errorf("remove blank OpenAI Responses input.%d message: %w", inputIndex, err)
			}
			changed = true
			continue
		}
		if !content.IsArray() {
			continue
		}

		parts := content.Array()
		remainingParts := len(parts)
		for contentIndex := len(parts) - 1; contentIndex >= 0; contentIndex-- {
			part := parts[contentIndex]
			partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
			partPath := fmt.Sprintf("input.%d.content.%d", inputIndex, contentIndex)
			switch partType {
			case "input_text", "output_text", "text":
				if strings.TrimSpace(part.Get("text").String()) != "" {
					continue
				}
				var err error
				normalized, err = sjson.DeleteBytes(normalized, partPath)
				if err != nil {
					return body, false, fmt.Errorf("remove empty OpenAI Responses %s: %w", partPath, err)
				}
				remainingParts--
				changed = true
			case "refusal":
				refusal := strings.TrimSpace(part.Get("refusal").String())
				if refusal == "" {
					var err error
					normalized, err = sjson.DeleteBytes(normalized, partPath)
					if err != nil {
						return body, false, fmt.Errorf("remove empty OpenAI Responses %s refusal: %w", partPath, err)
					}
					remainingParts--
					changed = true
					continue
				}
				var err error
				normalized, err = sjson.SetBytes(normalized, partPath+".type", "output_text")
				if err == nil {
					normalized, err = sjson.SetBytes(normalized, partPath+".text", refusal)
				}
				if err == nil {
					normalized, err = sjson.DeleteBytes(normalized, partPath+".refusal")
				}
				if err != nil {
					return body, false, fmt.Errorf("normalize OpenAI Responses %s refusal: %w", partPath, err)
				}
				changed = true
			}
		}

		if remainingParts == 0 {
			var err error
			normalized, err = sjson.DeleteBytes(normalized, fmt.Sprintf("input.%d", inputIndex))
			if err != nil {
				return body, false, fmt.Errorf("remove empty OpenAI Responses input.%d message: %w", inputIndex, err)
			}
			changed = true
		}
	}

	return normalized, changed, nil
}

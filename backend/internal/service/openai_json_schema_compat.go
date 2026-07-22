package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

type openAIJSONSchemaProtocol string

const (
	openAIJSONSchemaProtocolChat      openAIJSONSchemaProtocol = "chat_completions"
	openAIJSONSchemaProtocolResponses openAIJSONSchemaProtocol = "responses"
)

const openAIJSONSchemaCompatibilityInstruction = "The upstream model does not support strict JSON Schema output. Return only one JSON object that matches the following schema exactly. Use the exact property names and value types, include every required property, and do not add properties that the schema forbids.\nJSON Schema:\n"

func normalizeOpenAIJSONSchemaForForward(
	account *Account,
	body []byte,
	protocol openAIJSONSchemaProtocol,
	upstreamModel string,
) ([]byte, error) {
	normalized, changed, err := normalizeOpenAIJSONSchemaForAccount(account, body, protocol)
	if err != nil {
		return nil, err
	}
	if changed {
		logger.L().Debug("openai json_schema downgraded",
			zap.Int64("account_id", account.ID),
			zap.String("upstream_model", upstreamModel),
			zap.String("protocol", string(protocol)),
		)
	}
	return normalized, nil
}

func normalizeOpenAIJSONSchemaForAccount(
	account *Account,
	body []byte,
	protocol openAIJSONSchemaProtocol,
) ([]byte, bool, error) {
	if account.GetOpenAIJSONSchemaMode() != OpenAIJSONSchemaModeForceJSONObject {
		return body, false, nil
	}
	if !gjson.ValidBytes(body) {
		return body, false, fmt.Errorf("normalize OpenAI JSON schema compatibility: invalid request JSON")
	}

	formatPath := ""
	schemaPath := ""
	switch protocol {
	case openAIJSONSchemaProtocolChat:
		formatPath = "response_format"
		schemaPath = "response_format.json_schema"
	case openAIJSONSchemaProtocolResponses:
		formatPath = "text.format"
		schemaPath = formatPath
	default:
		return body, false, nil
	}

	format := gjson.GetBytes(body, formatPath)
	if !format.IsObject() || !strings.EqualFold(strings.TrimSpace(format.Get("type").String()), "json_schema") {
		return body, false, nil
	}
	schema := gjson.GetBytes(body, schemaPath)
	if !schema.IsObject() {
		return body, false, nil
	}

	schemaJSON, err := compactOpenAIJSONSchema([]byte(schema.Raw))
	if err != nil {
		return body, false, err
	}
	hint := openAIJSONSchemaCompatibilityInstruction + string(schemaJSON)

	switch protocol {
	case openAIJSONSchemaProtocolChat:
		return downgradeOpenAIChatJSONSchema(body, formatPath, hint)
	case openAIJSONSchemaProtocolResponses:
		return downgradeOpenAIResponsesJSONSchema(body, formatPath, hint)
	default:
		return body, false, nil
	}
}

func compactOpenAIJSONSchema(raw []byte) ([]byte, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, fmt.Errorf("compact OpenAI JSON schema compatibility hint: %w", err)
	}
	return compact.Bytes(), nil
}

func downgradeOpenAIChatJSONSchema(body []byte, formatPath, hint string) ([]byte, bool, error) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body, false, fmt.Errorf("normalize OpenAI JSON schema compatibility: chat messages must be an array")
	}
	var existingMessages []json.RawMessage
	if err := json.Unmarshal([]byte(messages.Raw), &existingMessages); err != nil {
		return body, false, fmt.Errorf("decode OpenAI chat messages for JSON schema compatibility: %w", err)
	}

	messageJSON, err := json.Marshal(map[string]string{
		"role":    "system",
		"content": hint,
	})
	if err != nil {
		return body, false, fmt.Errorf("marshal OpenAI JSON schema compatibility message: %w", err)
	}
	normalizedMessages := make([]json.RawMessage, 0, len(existingMessages)+1)
	normalizedMessages = append(normalizedMessages, json.RawMessage(messageJSON))
	normalizedMessages = append(normalizedMessages, existingMessages...)
	messagesJSON, err := json.Marshal(normalizedMessages)
	if err != nil {
		return body, false, fmt.Errorf("marshal OpenAI chat messages for JSON schema compatibility: %w", err)
	}
	normalized, err := sjson.SetRawBytes(body, formatPath, []byte(`{"type":"json_object"}`))
	if err != nil {
		return body, false, fmt.Errorf("downgrade OpenAI chat response format: %w", err)
	}
	normalized, err = sjson.SetRawBytes(normalized, "messages", messagesJSON)
	if err != nil {
		return body, false, fmt.Errorf("append OpenAI JSON schema compatibility message: %w", err)
	}
	return normalized, true, nil
}

func downgradeOpenAIResponsesJSONSchema(body []byte, formatPath, hint string) ([]byte, bool, error) {
	instructions := gjson.GetBytes(body, "instructions")
	combinedInstructions := hint
	if instructions.Exists() {
		if instructions.Type != gjson.String {
			return body, false, fmt.Errorf("normalize OpenAI JSON schema compatibility: responses instructions must be a string")
		}
		if existing := strings.TrimSpace(instructions.String()); existing != "" {
			combinedInstructions = existing + "\n\n" + hint
		}
	}

	normalized, err := sjson.SetRawBytes(body, formatPath, []byte(`{"type":"json_object"}`))
	if err != nil {
		return body, false, fmt.Errorf("downgrade OpenAI responses text format: %w", err)
	}
	normalized, err = sjson.SetBytes(normalized, "instructions", combinedInstructions)
	if err != nil {
		return body, false, fmt.Errorf("append OpenAI JSON schema compatibility instructions: %w", err)
	}
	return normalized, true, nil
}

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func forceJSONObjectAccount() *Account {
	return &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra: map[string]any{
			OpenAIJSONSchemaModeExtraKey: OpenAIJSONSchemaModeForceJSONObject,
		},
	}
}

func TestNormalizeOpenAIJSONSchemaForAccountChat(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6-terra",
		"messages":[{"role":"user","content":"Return a person"}],
		"response_format":{
			"type":"json_schema",
			"json_schema":{
				"name":"person",
				"strict":true,
				"schema":{
					"type":"object",
					"properties":{"name":{"type":"string"},"age":{"type":"integer"}},
					"required":["name","age"],
					"additionalProperties":false
				}
			}
		}
	}`)

	got, changed, err := normalizeOpenAIJSONSchemaForAccount(forceJSONObjectAccount(), body, openAIJSONSchemaProtocolChat)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"type":"json_object"}`, gjson.GetBytes(got, "response_format").Raw)
	require.Equal(t, int64(2), gjson.GetBytes(got, "messages.#").Int())
	require.Equal(t, "system", gjson.GetBytes(got, "messages.1.role").String())
	hint := gjson.GetBytes(got, "messages.1.content").String()
	require.Contains(t, hint, "JSON Schema")
	require.Contains(t, hint, `"required":["name","age"]`)
	require.Contains(t, hint, "exact property names")
}

func TestNormalizeOpenAIJSONSchemaForAccountResponses(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6-terra",
		"instructions":"Keep this instruction.",
		"input":"Return a person",
		"text":{"format":{
			"type":"json_schema",
			"name":"person",
			"strict":true,
			"schema":{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}
		}}
	}`)

	got, changed, err := normalizeOpenAIJSONSchemaForAccount(forceJSONObjectAccount(), body, openAIJSONSchemaProtocolResponses)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"type":"json_object"}`, gjson.GetBytes(got, "text.format").Raw)
	instructions := gjson.GetBytes(got, "instructions").String()
	require.Contains(t, instructions, "Keep this instruction.")
	require.Contains(t, instructions, "JSON Schema")
	require.Contains(t, instructions, `"required":["name"]`)
}

func TestNormalizeOpenAIJSONSchemaForAccountDoesNotChangeIneligibleRequests(t *testing.T) {
	tests := []struct {
		name     string
		account  *Account
		body     string
		protocol openAIJSONSchemaProtocol
	}{
		{
			name:     "auto account",
			account:  &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
			body:     `{"messages":[],"response_format":{"type":"json_schema","json_schema":{"schema":{"type":"object"}}}}`,
			protocol: openAIJSONSchemaProtocolChat,
		},
		{
			name: "passthrough account",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{
				OpenAIJSONSchemaModeExtraKey: OpenAIJSONSchemaModePassthrough,
			}},
			body:     `{"text":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`,
			protocol: openAIJSONSchemaProtocolResponses,
		},
		{
			name:     "json object already supported",
			account:  forceJSONObjectAccount(),
			body:     `{"messages":[],"response_format":{"type":"json_object"}}`,
			protocol: openAIJSONSchemaProtocolChat,
		},
		{
			name:     "missing schema payload",
			account:  forceJSONObjectAccount(),
			body:     `{"messages":[],"response_format":{"type":"json_schema"}}`,
			protocol: openAIJSONSchemaProtocolChat,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed, err := normalizeOpenAIJSONSchemaForAccount(tt.account, []byte(tt.body), tt.protocol)
			require.NoError(t, err)
			require.False(t, changed)
			require.JSONEq(t, tt.body, string(got))
		})
	}
}

func TestNormalizeOpenAIJSONSchemaForAccountRejectsMalformedContainer(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		protocol openAIJSONSchemaProtocol
	}{
		{
			name:     "chat messages is not array",
			body:     `{"messages":"bad","response_format":{"type":"json_schema","json_schema":{"schema":{"type":"object"}}}}`,
			protocol: openAIJSONSchemaProtocolChat,
		},
		{
			name:     "responses instructions is not string",
			body:     `{"instructions":[],"text":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`,
			protocol: openAIJSONSchemaProtocolResponses,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed, err := normalizeOpenAIJSONSchemaForAccount(forceJSONObjectAccount(), []byte(tt.body), tt.protocol)
			require.Error(t, err)
			require.False(t, changed)
			require.JSONEq(t, tt.body, string(got))
		})
	}
}

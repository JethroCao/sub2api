package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAccountShouldStripOpenAIResponsesLiteOnModelMapping(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		want    bool
	}{
		{name: "nil account"},
		{name: "missing setting", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}},
		{name: "disabled", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{OpenAIStripResponsesLiteOnModelMappingExtraKey: false}}},
		{name: "oauth enabled", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{OpenAIStripResponsesLiteOnModelMappingExtraKey: true}}, want: true},
		{name: "setup token enabled", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Extra: map[string]any{OpenAIStripResponsesLiteOnModelMappingExtraKey: true}}, want: true},
		{name: "api key ignores setting", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{OpenAIStripResponsesLiteOnModelMappingExtraKey: true}}},
		{name: "non openai ignores setting", account: &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth, Extra: map[string]any{OpenAIStripResponsesLiteOnModelMappingExtraKey: true}}},
		{name: "wrong value type", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{OpenAIStripResponsesLiteOnModelMappingExtraKey: "true"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.account.ShouldStripOpenAIResponsesLiteOnModelMapping())
		})
	}
}

func TestShouldStripOpenAIResponsesLiteForMappedModel(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{OpenAIStripResponsesLiteOnModelMappingExtraKey: true},
	}

	require.True(t, shouldStripOpenAIResponsesLiteForMappedModel(account, "gpt-5.6-terra", "gpt-5.5"))
	require.False(t, shouldStripOpenAIResponsesLiteForMappedModel(account, "gpt-5.5", "gpt-5.5"))
	require.False(t, shouldStripOpenAIResponsesLiteForMappedModel(account, "", "gpt-5.5"))
	require.False(t, shouldStripOpenAIResponsesLiteForMappedModel(account, "gpt-5.6-terra", ""))
}

func TestStripOpenAIResponsesLiteRepresentations(t *testing.T) {
	headers := http.Header{
		"X-Openai-Internal-Codex-Responses-Lite": []string{"true"},
		"X-Test":                                 []string{"keep"},
	}
	stripOpenAIResponsesLiteHeader(headers)
	require.Empty(t, headers.Get(responsesLiteHeader))
	require.Equal(t, "keep", headers.Get("X-Test"))

	body := []byte(`{"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true","keep":"value"},"input":"hi"}`)
	updated, changed, err := stripOpenAIResponsesLiteWebSocketMetadata(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(updated, "client_metadata."+responsesLiteWSMetadataKey).Exists())
	require.Equal(t, "value", gjson.GetBytes(updated, "client_metadata.keep").String())
	require.Equal(t, "hi", gjson.GetBytes(updated, "input").String())
}

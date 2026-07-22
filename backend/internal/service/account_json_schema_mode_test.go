package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountGetOpenAIJSONSchemaMode(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		want    string
	}{
		{name: "nil account", account: nil, want: OpenAIJSONSchemaModeAuto},
		{name: "missing extra", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, want: OpenAIJSONSchemaModeAuto},
		{name: "oauth ignores setting", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{OpenAIJSONSchemaModeExtraKey: OpenAIJSONSchemaModeForceJSONObject}}, want: OpenAIJSONSchemaModeAuto},
		{name: "non openai ignores setting", account: &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{OpenAIJSONSchemaModeExtraKey: OpenAIJSONSchemaModeForceJSONObject}}, want: OpenAIJSONSchemaModeAuto},
		{name: "auto", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{OpenAIJSONSchemaModeExtraKey: OpenAIJSONSchemaModeAuto}}, want: OpenAIJSONSchemaModeAuto},
		{name: "passthrough", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{OpenAIJSONSchemaModeExtraKey: OpenAIJSONSchemaModePassthrough}}, want: OpenAIJSONSchemaModePassthrough},
		{name: "force json object", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{OpenAIJSONSchemaModeExtraKey: OpenAIJSONSchemaModeForceJSONObject}}, want: OpenAIJSONSchemaModeForceJSONObject},
		{name: "normalized force json object", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{OpenAIJSONSchemaModeExtraKey: " FORCE_JSON_OBJECT "}}, want: OpenAIJSONSchemaModeForceJSONObject},
		{name: "invalid", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{OpenAIJSONSchemaModeExtraKey: "invalid"}}, want: OpenAIJSONSchemaModeAuto},
		{name: "wrong type", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{OpenAIJSONSchemaModeExtraKey: true}}, want: OpenAIJSONSchemaModeAuto},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.account.GetOpenAIJSONSchemaMode())
		})
	}
}

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAccountGetOpenAIImageInputMode(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		want    string
	}{
		{name: "missing defaults to auto", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, want: OpenAIImageInputModeAuto},
		{name: "multimodal", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{OpenAIImageInputModeExtraKey: OpenAIImageInputModeMultimodal}}, want: OpenAIImageInputModeMultimodal},
		{name: "text only", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{OpenAIImageInputModeExtraKey: OpenAIImageInputModeTextOnly}}, want: OpenAIImageInputModeTextOnly},
		{name: "normalizes value", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{OpenAIImageInputModeExtraKey: " MULTIMODAL "}}, want: OpenAIImageInputModeMultimodal},
		{name: "invalid defaults to auto", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{OpenAIImageInputModeExtraKey: "invalid"}}, want: OpenAIImageInputModeAuto},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.account.GetOpenAIImageInputMode())
		})
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_ImageInputCapability(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	groupID := int64(10130)
	textOnly := Account{
		ID: 38001, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0,
		Extra: map[string]any{OpenAIImageInputModeExtraKey: OpenAIImageInputModeTextOnly},
	}
	multimodal := Account{
		ID: 38002, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 10,
		Extra: map[string]any{OpenAIImageInputModeExtraKey: OpenAIImageInputModeMultimodal},
	}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{textOnly, multimodal}},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	imageSelection, _, err := svc.SelectAccountWithSchedulerForCapabilityAndImageInput(
		context.Background(), &groupID, "", "", "gpt-5.5", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		true, false, false, true,
	)
	require.NoError(t, err)
	require.NotNil(t, imageSelection)
	require.Equal(t, multimodal.ID, imageSelection.Account.ID)
	if imageSelection.ReleaseFunc != nil {
		imageSelection.ReleaseFunc()
	}

	textSelection, _, err := svc.SelectAccountWithSchedulerForCapabilityAndImageInput(
		context.Background(), &groupID, "", "", "gpt-5.5", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, false, true,
	)
	require.NoError(t, err)
	require.NotNil(t, textSelection)
	require.Equal(t, textOnly.ID, textSelection.Account.ID)
	if textSelection.ReleaseFunc != nil {
		textSelection.ReleaseFunc()
	}
}

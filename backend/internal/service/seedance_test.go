//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func seedanceTestAccount() *Account {
	return &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "ark-secret", "base_url": "https://ark.cn-beijing.volces.com/api/v3",
		"openai_capabilities": []string{"seedance"},
		"model_mapping":       map[string]any{"video": "ep-seedance"},
	}}
}

func TestSeedanceNativeForwarding(t *testing.T) {
	body := []byte(`{"model":"video","content":[{"type":"text","text":"waves"},{"type":"image_url","image_url":{"url":"https://example.com/first.png"},"role":"first_frame"},{"type":"audio_url","audio_url":{"url":"https://example.com/audio.mp3"}}],"duration":-1,"generate_audio":true,"future_field":{"keep":true}}`)
	upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"task-1"}`)}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	c, w := grokMediaContentTestContext(http.MethodPost, "/api/v3/contents/generations/tasks", nil)
	result, err := svc.ForwardSeedance(context.Background(), c, seedanceTestAccount(), SeedanceEndpointCreate, "", body)
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"task-1"}`, w.Body.String())
	require.Equal(t, "seedance:task-1", result.ResponseID)
	require.Zero(t, result.Usage.OutputTokens)
	require.Equal(t, "video", result.BillingModel)
	require.Equal(t, "ep-seedance", result.UpstreamModel)
	require.Equal(t, "https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks", upstream.request.URL.String())
	require.Equal(t, "Bearer ark-secret", upstream.request.Header.Get("Authorization"))
	forwarded, err := io.ReadAll(upstream.request.Body)
	require.NoError(t, err)
	require.Equal(t, "ep-seedance", gjson.GetBytes(forwarded, "model").String())
	for _, field := range []string{"content", "duration", "generate_audio", "future_field"} {
		require.Equal(t, gjson.GetBytes(body, field).Raw, gjson.GetBytes(forwarded, field).Raw)
	}
}

func TestSeedanceStatusAndDelete(t *testing.T) {
	for _, status := range []string{"queued", "running", "failed", "cancelled", "expired", "succeeded"} {
		t.Run(status, func(t *testing.T) {
			body := `{"id":"task-1","status":"` + status + `","model":"ep-seedance","content":{"video_url":"https://cdn.example/video.mp4"},"usage":{"completion_tokens":12345}}`
			upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(body)}
			svc := &OpenAIGatewayService{httpUpstream: upstream}
			c, w := grokMediaContentTestContext(http.MethodGet, "/api/v3/contents/generations/tasks/task-1", nil)
			result, err := svc.ForwardSeedance(context.Background(), c, seedanceTestAccount(), SeedanceEndpointStatus, "seedance:task-1", nil)
			require.NoError(t, err)
			require.JSONEq(t, body, w.Body.String())
			require.Equal(t, "/api/v3/contents/generations/tasks/task-1", upstream.request.URL.Path)
			if status == "succeeded" {
				require.Equal(t, 12345, result.Usage.OutputTokens)
			} else {
				require.Zero(t, result.Usage.OutputTokens)
			}
			require.Zero(t, result.VideoCount, "must use token billing, not Grok seconds")
		})
	}
	upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse("")}
	upstream.response.StatusCode = http.StatusNoContent
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	c, w := grokMediaContentTestContext(http.MethodDelete, "/api/v3/contents/generations/tasks/task-1", nil)
	_, err := svc.ForwardSeedance(context.Background(), c, seedanceTestAccount(), SeedanceEndpointDelete, "seedance:task-1", nil)
	require.NoError(t, err)
	require.Equal(t, http.MethodDelete, upstream.request.Method)
	require.Equal(t, http.StatusNoContent, w.Code)
}

func TestSeedanceValidationAndCapability(t *testing.T) {
	for _, body := range []string{`{`, `[]`, `{}`, `{"model":12,"content":[{}]}`, `{"model":"x","content":[]}`} {
		_, err := ParseSeedanceRequest([]byte(body))
		require.Error(t, err)
	}
	info, err := ParseSeedanceRequest([]byte(`{"model":"x","content":[{"type":"text","text":"first"},{"type":"text","text":"second"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}`))
	require.NoError(t, err)
	require.Contains(t, string(info.ModerationBody()), "first")
	require.Contains(t, string(info.ModerationBody()), "second")
	require.Contains(t, string(info.ModerationBody()), "https://example.com/a.png")
	for _, id := range []string{"", "..", "a/b", "a?b", "a#b", "%2e%2e"} {
		_, err := buildSeedanceURL("https://example.com", SeedanceEndpointStatus, id)
		require.Error(t, err, id)
	}
	for _, base := range []string{"https://example.com", "https://example.com/api/v3/", "https://example.com/v3"} {
		url, err := buildSeedanceURL(base, SeedanceEndpointCreate, "")
		require.NoError(t, err)
		require.NotContains(t, url, "/v3/api/v3")
	}
	a := seedanceTestAccount()
	require.True(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance))
	a.Type = AccountTypeOAuth
	require.False(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance))
	a.Type = AccountTypeAPIKey
	delete(a.Credentials, "openai_capabilities")
	require.False(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance))
}

func TestSeedancePreservesUpstreamErrorsWithoutRetry(t *testing.T) {
	upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"error":{"code":"QuotaExceeded","message":"quota exhausted"}}`)}
	upstream.response.StatusCode = 429
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	c, w := grokMediaContentTestContext(http.MethodPost, "/api/v3/contents/generations/tasks", nil)
	_, err := svc.ForwardSeedance(context.Background(), c, seedanceTestAccount(), SeedanceEndpointCreate, "", []byte(`{"model":"video","content":[{"type":"text","text":"waves"}]}`))
	require.Error(t, err)
	require.Equal(t, 429, w.Code)
	require.Contains(t, w.Body.String(), "QuotaExceeded")
	require.Len(t, upstream.requests, 1)
}

func TestSeedanceOfficialPricingRejectsUnpriceableCreateBeforeForwarding(t *testing.T) {
	for _, tc := range []struct {
		name, body, rate string
	}{
		{"missing FX", `{"model":"seedance-2.0","content":[{"type":"text","text":"waves"}]}`, ""},
		{"zero FX", `{"model":"seedance-2.0","content":[{"type":"text","text":"waves"}]}`, "0"},
		{"invalid FX", `{"model":"seedance-2.0","content":[{"type":"text","text":"waves"}]}`, "NaN"},
		{"unknown model", `{"model":"seedance-unknown","content":[{"type":"text","text":"waves"}]}`, "6.7351"},
		{"unknown resolution", `{"model":"seedance-2.0","resolution":"8k","content":[{"type":"text","text":"waves"}]}`, "6.7351"},
		{"2.5 has no 4k price", `{"model":"seedance-2.5","resolution":"4k","content":[{"type":"text","text":"waves"}]}`, "6.7351"},
		{"draft requires owned snapshot", `{"model":"seedance-2.5","resolution":"1080p","content":[{"type":"draft_task","draft_task":{"id":"draft-1"}}]}`, "6.7351"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := seedanceTestAccount()
			account.Credentials["seedance_official_pricing"] = "true"
			account.Credentials["seedance_usd_to_cny_rate"] = tc.rate
			upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"paid-task"}`)}
			svc := &OpenAIGatewayService{httpUpstream: upstream}
			c, _ := grokMediaContentTestContext(http.MethodPost, "/api/v3/contents/generations/tasks", nil)
			_, err := svc.ForwardSeedance(context.Background(), c, account, SeedanceEndpointCreate, "", []byte(tc.body))
			require.Error(t, err)
			require.Empty(t, upstream.requests, "must reject before creating a paid upstream task")
		})
	}
}

func TestSeedanceOfficialTariffsAndDollarConversion(t *testing.T) {
	for _, tc := range []struct {
		family, resolution string
		video              bool
		cny                float64
	}{
		{"2.0", "480p", false, 46}, {"2.0", "720p", false, 46}, {"2.0", "1080p", false, 51}, {"2.0", "4k", false, 26},
		{"2.0", "480p", true, 28}, {"2.0", "720p", true, 28}, {"2.0", "1080p", true, 31}, {"2.0", "4k", true, 16},
		{"2.5", "480p", false, 70}, {"2.5", "720p", false, 70}, {"2.5", "1080p", false, 77},
		{"2.5", "480p", true, 42}, {"2.5", "720p", true, 42}, {"2.5", "1080p", true, 46},
	} {
		t.Run(tc.family+"/"+tc.resolution+fmt.Sprint(tc.video), func(t *testing.T) {
			account := seedanceTestAccount()
			account.Credentials["seedance_official_pricing"] = "true"
			account.Credentials["seedance_usd_to_cny_rate"] = "6.7351"
			model := "seedance-" + tc.family
			account.Credentials["model_mapping"] = map[string]any{model: "ep-priced-model"}
			content := `[{"type":"image_url","image_url":{"url":"asset://image-1"}}]`
			if tc.video {
				content = `[{"type":"video_url","video_url":{"url":"asset://video-1"}}]`
			}
			body := []byte(fmt.Sprintf(`{"model":%q,"resolution":%q,"content":%s}`, model, tc.resolution, content))
			upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"paid-task"}`)}
			svc := &OpenAIGatewayService{httpUpstream: upstream}
			c, _ := grokMediaContentTestContext(http.MethodPost, "/api/v3/contents/generations/tasks", nil)
			result, err := svc.ForwardSeedance(context.Background(), c, account, SeedanceEndpointCreate, "", body)
			require.NoError(t, err)
			require.NotNil(t, result.SeedanceBilling)
			require.Equal(t, tc.video, result.SeedanceBilling.HasInputVideo)
			// The existing unified usage cost path must use the Seedance snapshot,
			// not missing catalog prices, Grok seconds, or token peak surcharges.
			cost, err := svc.calculateOpenAIRecordUsageCost(context.Background(), result, nil, []string{model}, 3, 5, 7, 1, UsageTokens{OutputTokens: 1_000_000}, "", nil, time.Now())
			require.NoError(t, err)
			require.InDelta(t, tc.cny/6.7351, cost.OutputCost, 1e-12)
			require.InDelta(t, tc.cny/6.7351, cost.ActualCost, 1e-12)
			require.Zero(t, cost.InputCost)
			require.Equal(t, string(BillingModeToken), cost.BillingMode)
		})
	}
}

func TestSeedanceOfficialDraftUsesOriginalInputVideoCondition(t *testing.T) {
	account := seedanceTestAccount()
	account.Credentials["seedance_official_pricing"] = "true"
	account.Credentials["seedance_usd_to_cny_rate"] = "6.7351"
	for _, video := range []bool{false, true} {
		content := `[{"type":"text","text":"waves"}]`
		if video {
			content = `[{"type":"video_url","video_url":{"url":"https://example.com/source.mp4"}}]`
		}
		parent, err := prepareSeedanceOfficialBilling(account, []byte(`{"model":"seedance-2.5","draft":true,"content":`+content+`}`), nil)
		require.NoError(t, err)
		require.Equal(t, "480p", parent.Resolution)
		formal, err := prepareSeedanceOfficialBilling(account, []byte(`{"model":"seedance-2.5","resolution":"1080p","content":[{"type":"draft_task","draft_task":{"id":"draft-1"}}]}`), parent)
		require.NoError(t, err)
		require.Equal(t, video, formal.HasInputVideo)
		price := 77.0
		if video {
			price = 46
		}
		cost, err := calculateSeedanceOfficialCost(formal, 1_000_000, 1)
		require.NoError(t, err)
		require.InDelta(t, price/6.7351, cost.ActualCost, 1e-12)
		defaultFormal, err := prepareSeedanceOfficialBilling(account, []byte(`{"model":"seedance-2.5","content":[{"type":"draft_task","draft_task":{"id":"draft-1"}}]}`), parent)
		require.NoError(t, err)
		require.Equal(t, "1080p", defaultFormal.Resolution)
		_, err = prepareSeedanceOfficialBilling(account, []byte(`{"model":"seedance-2.5","resolution":"720p","content":[{"type":"draft_task","draft_task":{"id":"draft-1"}}]}`), parent)
		require.Error(t, err, "formal video currently supports 1080p only")
	}
}

type seedanceTaskTTLCache struct {
	GatewayCache
	ownerTTL, pendingTTL, claimTTL time.Duration
}

func (c *seedanceTaskTTLCache) SetSessionAccountID(_ context.Context, _ int64, _ string, _ int64, ttl time.Duration) error {
	c.ownerTTL = ttl
	return nil
}
func (c *seedanceTaskTTLCache) SetGrokVideoPendingBilling(_ context.Context, _ string, _ []byte, ttl time.Duration) error {
	c.pendingTTL = ttl
	return nil
}
func (c *seedanceTaskTTLCache) ClaimGrokVideoBilled(_ context.Context, _ string, ttl time.Duration) (bool, error) {
	c.claimTTL = ttl
	return true, nil
}

func TestSeedanceTaskTTLMatchesOfficialLifecycleWithoutChangingGrok(t *testing.T) {
	for _, tc := range []struct {
		task           string
		pending, claim time.Duration
	}{
		{"seedance:task-1", 8 * 24 * time.Hour, 8 * 24 * time.Hour},
		{"grok-task-1", 24 * time.Hour, 48 * time.Hour},
	} {
		cache := &seedanceTaskTTLCache{}
		svc := &OpenAIGatewayService{cache: cache}
		group := int64(12)
		require.NoError(t, svc.BindGrokMediaVideoRequestAccount(context.Background(), &group, tc.task, 10, 20, 15))
		require.NoError(t, svc.StoreGrokVideoPendingBilling(context.Background(), tc.task, 10, 20, GrokVideoPendingBilling{Model: "seedance-2.0"}))
		_, err := svc.ClaimGrokVideoBilling(context.Background(), tc.task, 10, 20)
		require.NoError(t, err)
		require.Equal(t, tc.pending, cache.ownerTTL)
		require.Equal(t, tc.pending, cache.pendingTTL)
		require.Equal(t, tc.claim, cache.claimTTL)
	}
}

func TestSeedanceOfficialDefaultsWeakParametersAndModelIdentity(t *testing.T) {
	account := seedanceTestAccount()
	account.Credentials["seedance_official_pricing"] = "true"
	account.Credentials["seedance_usd_to_cny_rate"] = "6.7351"
	for _, tc := range []struct{ body, res string }{
		{`{"model":"seedance-2.0","content":[{"type":"text","text":"waves"}]}`, "720p"},
		{`{"model":"seedance-2.0","content":[{"type":"text","text":"waves --rs 1080p"}]}`, "1080p"},
		{`{"model":"seedance-2.0","resolution":"4k","content":[{"type":"text","text":"waves --rs 720p"}]}`, "4k"},
	} {
		price, err := prepareSeedanceOfficialBilling(account, []byte(tc.body), nil)
		require.NoError(t, err)
		require.Equal(t, tc.res, price.Resolution)
		actual, err := price.WithResolution("1080p")
		require.NoError(t, err)
		require.Equal(t, "1080p", actual.Resolution)
		require.Equal(t, tc.res, price.Resolution, "reconciliation must not mutate the create snapshot")
	}
	require.Equal(t, "2.0", seedanceModelFamily("alias", "doubao-seedance-2-0-260128"))
	require.Equal(t, "2.5", seedanceModelFamily("alias", "doubao-seedance-2-5-260628"))
	require.Empty(t, seedanceModelFamily("seedance-2.0", "doubao-seedance-2-0-fast-260128"))
	for _, body := range []string{`{"content":[{"type":"draft_task","draft_task":{"id":"../other"}}]}`, `{"content":[{"type":"draft_task","draft_task":{"id":"a"}},{"type":"draft_task","draft_task":{"id":"b"}}]}`} {
		_, err := SeedanceDraftTaskID([]byte(body))
		require.Error(t, err)
	}
}

func TestSeedanceOfficialUsageRecordsRealChargeAndStableTaskID(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	groupID := int64(12)
	taskID := StableGrokVideoBillingRequestID("seedance:priced-task")
	price := &SeedanceBillingSnapshot{ModelFamily: "2.0", Resolution: "1080p", HasInputVideo: true, USDToCNYRate: 6.7351, OutputPricesCNYPerMillion: map[string]float64{"1080p": 31}}
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "poll-local-id")
	err := svc.RecordUsage(ctx, &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{RequestID: taskID, ResponseID: "seedance:priced-task", Model: "seedance-2.0", BillingModel: "seedance-2.0", Usage: OpenAIUsage{OutputTokens: 12345}, SeedanceBilling: price},
		APIKey: &APIKey{ID: 1000, GroupID: &groupID, Group: &Group{ID: groupID, RateMultiplier: 1}},
		User:   &User{ID: 2000}, Account: &Account{ID: 15, Type: AccountTypeAPIKey},
	})
	require.NoError(t, err)
	require.Equal(t, taskID, usageRepo.lastLog.RequestID)
	require.Equal(t, taskID, billingRepo.lastCmd.RequestID)
	require.Equal(t, 1.0, usageRepo.lastLog.RateMultiplier)
	require.Equal(t, "1080p", *usageRepo.lastLog.VideoResolution)
	require.Equal(t, string(BillingModeToken), *usageRepo.lastLog.BillingMode)
	want := float64(12345) * 31 / 1_000_000 / 6.7351
	require.InDelta(t, want, usageRepo.lastLog.ActualCost, 1e-8)
	require.InDelta(t, want, billingRepo.lastCmd.BalanceCost, 1e-8)
	require.Positive(t, billingRepo.lastCmd.BalanceCost, "must not fall back to free missing-catalog pricing")
}

func TestSeedanceOfficialDraftForwardPollAndRecordedCharge(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	upstream := &grokMediaContentUpstreamStub{responses: []*http.Response{
		grokMediaContentStatusResponse(`{"id":"draft-1"}`),
		grokMediaContentStatusResponse(`{"id":"formal-1"}`),
		grokMediaContentStatusResponse(`{"id":"formal-1","status":"succeeded","usage":{"completion_tokens":12345}}`),
	}}
	svc.httpUpstream = upstream
	account := seedanceTestAccount()
	account.Credentials["seedance_official_pricing"] = "true"
	account.Credentials["seedance_usd_to_cny_rate"] = "6.7351"
	account.Credentials["model_mapping"] = map[string]any{"seedance-2.5": "ep-seedance-25"}
	ctx := context.Background()
	c, _ := grokMediaContentTestContext(http.MethodPost, "/api/v3/contents/generations/tasks", nil)
	draft, err := svc.ForwardSeedance(ctx, c, account, SeedanceEndpointCreate, "", []byte(`{"model":"seedance-2.5","draft":true,"content":[{"type":"video_url","video_url":{"url":"https://example.com/source.mp4"}}]}`))
	require.NoError(t, err)
	require.True(t, draft.SeedanceBilling.Draft)
	require.True(t, draft.SeedanceBilling.HasInputVideo)
	c, _ = grokMediaContentTestContext(http.MethodPost, "/api/v3/contents/generations/tasks", nil)
	formal, err := svc.ForwardSeedance(ctx, c, account, SeedanceEndpointCreate, "", []byte(`{"model":"seedance-2.5","content":[{"type":"draft_task","draft_task":{"id":"draft-1"}}]}`), draft.SeedanceBilling)
	require.NoError(t, err)
	require.True(t, formal.SeedanceBilling.HasInputVideo)
	require.Equal(t, "1080p", formal.SeedanceBilling.Resolution)
	c, _ = grokMediaContentTestContext(http.MethodGet, "/api/v3/contents/generations/tasks/formal-1", nil)
	completed, err := svc.ForwardSeedance(ctx, c, account, SeedanceEndpointStatus, formal.ResponseID, nil)
	require.NoError(t, err)
	completed.SeedanceBilling, err = formal.SeedanceBilling.WithResolution(completed.VideoResolution)
	require.NoError(t, err)
	completed.Model, completed.BillingModel = formal.Model, formal.BillingModel
	completed.RequestID = StableGrokVideoBillingRequestID(completed.ResponseID)
	groupID := int64(12)
	require.NoError(t, svc.RecordUsage(ctx, &OpenAIRecordUsageInput{
		Result: completed, Account: account, User: &User{ID: 2000},
		APIKey: &APIKey{ID: 1000, GroupID: &groupID, Group: &Group{ID: groupID, RateMultiplier: 1}},
	}))
	require.Equal(t, "grok-video:seedance:formal-1", usageRepo.lastLog.RequestID)
	require.InDelta(t, float64(12345)*46/1_000_000/6.7351, billingRepo.lastCmd.BalanceCost, 1e-8)
	require.Len(t, upstream.requests, 3)
}

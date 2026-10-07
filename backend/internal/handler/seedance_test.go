//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSeedanceHandlerLifecycleAndOwnership(t *testing.T) {
	h, slots, bindings, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	var owner int64
	upstream.call = func(req *http.Request, id int64) (*http.Response, error) {
		body := `{"id":"task-ark","status":"queued"}`
		if req.Method == http.MethodPost {
			owner = id
		} else {
			require.Equal(t, owner, id)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	newContext := func(method string) (*gin.Context, *httptest.ResponseRecorder) {
		c, w := grokMediaSlotContext(context.Background(), method == http.MethodPost)
		key, _ := middleware.GetAPIKeyFromContext(c)
		key.Group.Platform = service.PlatformOpenAI
		body := ""
		if method == http.MethodPost {
			body = `{"model":"doubao-seedance","content":[{"type":"text","text":"waves"}]}`
		}
		c.Request = httptest.NewRequest(method, "/api/v3/contents/generations/tasks", strings.NewReader(body))
		c.Params = gin.Params{{Key: "task_id", Value: "task-ark"}}
		return c, w
	}
	c, w := newContext(http.MethodPost)
	h.SeedanceTasks(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Positive(t, owner)
	require.Len(t, bindings.pending, 1)
	slots.assertReleased(t)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		c, w = newContext(method)
		h.SeedanceTasks(c)
		require.Equal(t, 200, w.Code, w.Body.String())
		slots.assertReleased(t)
	}
	for _, other := range []string{"user", "key", "group", "task", "provider"} {
		c, w = newContext(http.MethodGet)
		key, _ := middleware.GetAPIKeyFromContext(c)
		switch other {
		case "user":
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 11, Concurrency: 5})
		case "key":
			key.ID = 21
		case "group":
			group := int64(25)
			key.GroupID = &group
		case "task":
			c.Params = gin.Params{{Key: "task_id", Value: "other"}}
		case "provider":
			c.Params = gin.Params{{Key: "request_id", Value: "task-ark"}}
		}
		before := upstream.calls
		if other == "provider" {
			h.GrokVideoStatus(c)
		} else {
			h.SeedanceTasks(c)
		}
		require.Equal(t, 404, w.Code, other+": "+w.Body.String())
		require.Equal(t, before, upstream.calls)
		slots.assertReleased(t)
	}
	c, _ = newContext(http.MethodGet)
	key, _ := middleware.GetAPIKeyFromContext(c)
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	result := &service.OpenAIForwardResult{Usage: service.OpenAIUsage{OutputTokens: 12345}, ResponseID: "seedance:task-ark"}
	for i := range 20 {
		billed := prepareSeedanceCompletionBilling(context.Background(), h, key, subject, result.ResponseID, result)
		if i == 0 {
			require.NotNil(t, billed)
			require.Equal(t, "doubao-seedance", billed.BillingModel)
			require.Equal(t, 12345, billed.Usage.OutputTokens)
			require.Zero(t, billed.VideoCount)
		} else {
			require.Nil(t, billed)
		}
	}
	require.Len(t, bindings.billed, 1)
}

func TestSeedanceCompletionUsesFrozenOfficialPricingAndActualResolution(t *testing.T) {
	h, _, bindings, _ := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	c, _ := grokMediaSlotContext(context.Background(), false)
	key, _ := middleware.GetAPIKeyFromContext(c)
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	p := &service.SeedanceBillingSnapshot{ModelFamily: "2.0", Resolution: "720p", USDToCNYRate: 6.7351, OutputPricesCNYPerMillion: map[string]float64{"720p": 46, "1080p": 51, "4k": 26}}
	require.NoError(t, h.gatewayService.StoreGrokVideoPendingBilling(context.Background(), "seedance:priced", subject.UserID, key.ID, service.GrokVideoPendingBilling{Model: "seedance-2.0", SeedanceBilling: p}))
	result := &service.OpenAIForwardResult{ResponseID: "seedance:priced", VideoResolution: "4k", Usage: service.OpenAIUsage{OutputTokens: 12345}}
	billed := prepareSeedanceCompletionBilling(context.Background(), h, key, subject, result.ResponseID, result)
	require.NotNil(t, billed)
	require.NotNil(t, billed.SeedanceBilling)
	require.Equal(t, "4k", billed.SeedanceBilling.Resolution)
	require.Equal(t, 6.7351, billed.SeedanceBilling.USDToCNYRate)
	require.Equal(t, 26.0, billed.SeedanceBilling.OutputPricesCNYPerMillion["4k"])
	require.Nil(t, prepareSeedanceCompletionBilling(context.Background(), h, key, subject, result.ResponseID, result))
	require.Len(t, bindings.billed, 1)
}

func TestSeedanceInvalidStatusResolutionDoesNotConsumeBillingClaim(t *testing.T) {
	h, _, bindings, _ := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	c, _ := grokMediaSlotContext(context.Background(), false)
	key, _ := middleware.GetAPIKeyFromContext(c)
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	p := &service.SeedanceBillingSnapshot{ModelFamily: "2.5", Resolution: "720p", USDToCNYRate: 6.7351, OutputPricesCNYPerMillion: map[string]float64{"720p": 70, "1080p": 77}}
	require.NoError(t, h.gatewayService.StoreGrokVideoPendingBilling(context.Background(), "seedance:invalid", subject.UserID, key.ID, service.GrokVideoPendingBilling{Model: "seedance-2.5", SeedanceBilling: p}))
	result := &service.OpenAIForwardResult{ResponseID: "seedance:invalid", VideoResolution: "4k", Usage: service.OpenAIUsage{OutputTokens: 12345}}
	require.Nil(t, prepareSeedanceCompletionBilling(context.Background(), h, key, subject, result.ResponseID, result))
	require.Empty(t, bindings.billed)
	result.VideoResolution = "1080p"
	require.NotNil(t, prepareSeedanceCompletionBilling(context.Background(), h, key, subject, result.ResponseID, result))
}

func TestSeedanceDraftReferenceUsesOwnerAccountAndRejectsOtherOwners(t *testing.T) {
	for _, other := range []string{"none", "user", "key", "group"} {
		t.Run(other, func(t *testing.T) {
			h, slots, _, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
			c, w := grokMediaSlotContext(context.Background(), true)
			key, _ := middleware.GetAPIKeyFromContext(c)
			key.Group.Platform = service.PlatformOpenAI
			require.NoError(t, h.gatewayService.BindGrokMediaVideoRequestAccount(context.Background(), key.GroupID, "seedance:draft-1", 10, 20, 2))
			switch other {
			case "user":
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 11, Concurrency: 5})
			case "key":
				key.ID = 21
			case "group":
				group := int64(25)
				key.GroupID = &group
			}
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v3/contents/generations/tasks", strings.NewReader(`{"model":"seedance-2.5","resolution":"1080p","content":[{"type":"draft_task","draft_task":{"id":"draft-1"}}]}`))
			upstream.call = func(req *http.Request, accountID int64) (*http.Response, error) {
				require.Equal(t, int64(2), accountID, "formal video must use the draft's original account")
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"formal-1"}`))}, nil
			}
			h.SeedanceTasks(c)
			if other == "none" {
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Equal(t, 1, upstream.calls)
			} else {
				require.Equal(t, 404, w.Code, w.Body.String())
				require.Zero(t, upstream.calls)
			}
			slots.assertReleased(t)
		})
	}
}

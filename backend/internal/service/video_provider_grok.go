package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/tidwall/gjson"
)

const grokVideoResponseReadLimit int64 = 8 << 20

// GrokVideoTokenProvider supplies a refreshed OAuth access token. API-key
// accounts read their credential directly and do not use this dependency.
type GrokVideoTokenProvider interface {
	GetAccessToken(context.Context, *Account) (string, error)
}

// GrokVideoProvider isolates xAI's asynchronous video API from HTTP handlers.
// It deliberately accepts only standard Go context and HTTP values so durable
// task workers can submit, poll, and download without a gin.Context.
type GrokVideoProvider struct {
	upstream      HTTPUpstream
	tokenProvider GrokVideoTokenProvider
	cfg           *config.Config
}

func NewGrokVideoProvider(upstream HTTPUpstream, tokenProvider GrokVideoTokenProvider, cfg ...*config.Config) *GrokVideoProvider {
	provider := &GrokVideoProvider{upstream: upstream, tokenProvider: tokenProvider}
	if len(cfg) > 0 {
		provider.cfg = cfg[0]
	}
	return provider
}

func (p *GrokVideoProvider) Name() string { return PlatformGrok }

func (p *GrokVideoProvider) Capabilities() VideoProviderCapabilities {
	return VideoProviderCapabilities{
		VideoOperationGeneration: {Text: true, FirstFrame: true, ReferenceImages: true},
		VideoOperationEdit:       {Edit: true, ReferenceVideos: true},
		VideoOperationExtension:  {Extension: true, ReferenceVideos: true},
	}
}

func (p *GrokVideoProvider) Submit(ctx context.Context, account *Account, request CanonicalVideoRequest, _ string) (VideoSubmitResult, error) {
	endpoint, err := grokVideoEndpointForOperation(request.Operation)
	if err != nil {
		return VideoSubmitResult{}, NewVideoProviderError(http.StatusBadRequest, "invalid_request", false, false, err)
	}
	body, err := buildGrokVideoSubmissionBody(account, request)
	if err != nil {
		return VideoSubmitResult{}, NewVideoProviderError(http.StatusBadRequest, "invalid_request", false, false, err)
	}
	responseBody, _, err := p.doJSON(ctx, account, endpoint, "", http.MethodPost, body)
	if err != nil {
		return VideoSubmitResult{}, grokVideoProviderSubmissionError(err)
	}
	upstreamTaskID := extractGrokMediaVideoRequestID(responseBody)
	if upstreamTaskID == "" {
		return VideoSubmitResult{}, NewVideoProviderError(http.StatusBadGateway, "provider_contract_error", true, true, errors.New("grok video submission response has no request ID"))
	}
	return VideoSubmitResult{
		UpstreamTaskID: upstreamTaskID,
		Status:         VideoTaskQueued,
		UpstreamStatus: strings.TrimSpace(gjson.GetBytes(responseBody, "status").String()),
	}, nil
}

// RecoverSubmission intentionally does not query upstream: xAI has no
// verified client-token recovery endpoint. Returning found=false leaves an
// ambiguous submission unknown for manual review instead of resubmitting it.
func (p *GrokVideoProvider) RecoverSubmission(context.Context, *Account, VideoTask, string) (VideoSubmitResult, bool, error) {
	return VideoSubmitResult{}, false, nil
}

func (p *GrokVideoProvider) Poll(ctx context.Context, account *Account, task VideoTask) (VideoPollResult, error) {
	upstreamTaskID := ""
	if task.UpstreamTaskID != nil {
		upstreamTaskID = strings.TrimSpace(*task.UpstreamTaskID)
	}
	if upstreamTaskID == "" {
		return VideoPollResult{}, NewVideoProviderError(http.StatusBadRequest, "invalid_request", false, false, errors.New("grok video task ID is required"))
	}
	body, _, err := p.doJSON(WithHTTPUpstreamRedirectsDisabled(ctx), account, GrokMediaEndpointVideoStatus, upstreamTaskID, http.MethodGet, nil)
	if err != nil {
		return VideoPollResult{}, grokVideoProviderPollError(err)
	}
	upstreamStatus := strings.TrimSpace(gjson.GetBytes(body, "status").String())
	result := VideoPollResult{
		Status:         grokVideoTaskStatus(upstreamStatus),
		UpstreamStatus: upstreamStatus,
		ResultURL: firstNonEmpty(
			strings.TrimSpace(gjson.GetBytes(body, "video.url").String()),
			strings.TrimSpace(gjson.GetBytes(body, "url").String()),
		),
		ResultContentType:     strings.TrimSpace(gjson.GetBytes(body, "video.content_type").String()),
		ActualDurationSeconds: int(gjson.GetBytes(body, "video.duration").Int()),
		Resolution:            strings.TrimSpace(gjson.GetBytes(body, "video.resolution").String()),
	}
	if result.Status == VideoTaskFailed {
		result.Error = NewVideoTaskError("UPSTREAM_TASK_FAILED", strings.TrimSpace(extractUpstreamErrorMessage(body)), false)
	}
	return result, nil
}

type grokVideoContentRangeContextKey struct{}

// WithGrokVideoContentRange makes an inbound HTTP Range header available to
// OpenContent without coupling the provider to a web framework.
func WithGrokVideoContentRange(ctx context.Context, value string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, grokVideoContentRangeContextKey{}, strings.TrimSpace(value))
}

func grokVideoContentRange(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(grokVideoContentRangeContextKey{}).(string)
	return strings.TrimSpace(value)
}

func (p *GrokVideoProvider) OpenContent(ctx context.Context, account *Account, task VideoTask) (io.ReadCloser, http.Header, int64, error) {
	if task.UpstreamTaskID == nil || strings.TrimSpace(*task.UpstreamTaskID) == "" {
		return nil, nil, 0, NewVideoProviderError(http.StatusBadRequest, "invalid_request", false, false, errors.New("grok video task ID is required"))
	}
	upstreamTaskID := strings.TrimSpace(*task.UpstreamTaskID)
	token, err := p.token(ctx, account)
	if err != nil {
		return nil, nil, 0, NewVideoProviderError(http.StatusUnauthorized, "upstream_authentication", false, false, err)
	}
	statusBody, _, err := p.doJSONWithToken(WithHTTPUpstreamRedirectsDisabled(ctx), account, GrokMediaEndpointVideoStatus, upstreamTaskID, http.MethodGet, nil, token)
	if err != nil {
		return nil, nil, 0, grokVideoProviderPollError(err)
	}
	contentURL, err := grokMediaSignedVideoContentURL(statusBody, upstreamTaskID)
	if err != nil {
		return nil, nil, 0, NewVideoProviderError(http.StatusBadGateway, "provider_contract_error", false, false, err)
	}
	signedContent := contentURL != ""
	if !signedContent {
		contentURL, err = buildGrokMediaURL(account, p.cfg, GrokMediaEndpointVideoContent, upstreamTaskID)
		if err != nil {
			return nil, nil, 0, NewVideoProviderError(http.StatusBadGateway, "upstream_unavailable", true, false, err)
		}
	}

	upstreamCtx, release := detachUpstreamContext(ctx)
	defer release()
	request, err := http.NewRequestWithContext(WithHTTPUpstreamRedirectsDisabled(upstreamCtx), http.MethodGet, contentURL, nil)
	if err != nil {
		return nil, nil, 0, NewVideoProviderError(http.StatusBadGateway, "upstream_error", true, false, err)
	}
	request.Header.Set("Accept", "*/*")
	if rangeHeader := grokVideoContentRange(ctx); rangeHeader != "" {
		request.Header.Set("Range", rangeHeader)
	}
	if !signedContent {
		request.Header.Set("Authorization", "Bearer "+token)
		if account.IsGrokOAuth() && isGrokCLIProxyTarget(contentURL) {
			applyGrokCLIHeaders(request.Header)
		}
		account.ApplyHeaderOverrides(request.Header)
	}
	response, err := p.do(request, account)
	if err != nil {
		return nil, nil, 0, NewVideoProviderError(http.StatusBadGateway, "upstream_timeout", true, false, err)
	}
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		body := readGrokVideoResponse(response)
		return nil, nil, 0, grokVideoProviderHTTPError(response.StatusCode, body, false)
	}
	header := response.Header.Clone()
	length := response.ContentLength
	if length <= 0 {
		if parsed, parseErr := strconv.ParseInt(strings.TrimSpace(header.Get("Content-Length")), 10, 64); parseErr == nil {
			length = parsed
		}
	}
	return response.Body, header, length, nil
}

func grokVideoEndpointForOperation(operation VideoOperation) (GrokMediaEndpoint, error) {
	switch operation {
	case VideoOperationGeneration:
		return GrokMediaEndpointVideosGenerations, nil
	case VideoOperationEdit:
		return GrokMediaEndpointVideosEdits, nil
	case VideoOperationExtension:
		return GrokMediaEndpointVideosExtensions, nil
	default:
		return "", fmt.Errorf("unsupported grok video operation: %s", operation)
	}
}

func buildGrokVideoSubmissionBody(account *Account, request CanonicalVideoRequest) ([]byte, error) {
	endpoint, err := grokVideoEndpointForOperation(request.Operation)
	if err != nil {
		return nil, err
	}
	model := NormalizeGrokMediaModelForEndpoint(endpoint, request.Model, len(request.FirstFrame) > 0 || len(request.ReferenceImages) > 0)
	if account != nil {
		if mapped := strings.TrimSpace(account.GetMappedModel(model)); mapped != "" {
			model = mapped
		}
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("grok video model is required")
	}
	payload := make(map[string]any)
	payload["model"] = model
	if prompt := strings.TrimSpace(request.Prompt); prompt != "" {
		payload["prompt"] = prompt
	}
	if len(request.FirstFrame) > 0 {
		payload["image"] = map[string]string{"url": request.FirstFrame[0].URL}
	}
	if len(request.ReferenceImages) > 0 {
		payload["images"] = grokVideoAssets(request.ReferenceImages)
	}
	if len(request.ReferenceVideos) > 0 {
		payload["video"] = map[string]string{"url": request.ReferenceVideos[0].URL}
	}
	if len(request.ReferenceVideos) > 1 {
		payload["reference_videos"] = grokVideoAssets(request.ReferenceVideos[1:])
	}
	if resolution := strings.TrimSpace(request.Resolution); resolution != "" {
		payload["resolution"] = resolution
	}
	if request.DurationSeconds > 0 {
		payload["duration"] = request.DurationSeconds
	}
	if aspectRatio := strings.TrimSpace(request.AspectRatio); aspectRatio != "" {
		payload["aspect_ratio"] = aspectRatio
	}
	if request.Audio != nil {
		payload["audio"] = *request.Audio
	}
	return json.Marshal(payload)
}

func grokVideoAssets(assets []VideoAsset) []map[string]string {
	values := make([]map[string]string, 0, len(assets))
	for _, asset := range assets {
		values = append(values, map[string]string{"url": asset.URL})
	}
	return values
}

func (p *GrokVideoProvider) doJSON(ctx context.Context, account *Account, endpoint GrokMediaEndpoint, requestID, method string, body []byte) ([]byte, http.Header, error) {
	if p == nil || p.upstream == nil {
		return nil, nil, errors.New("grok video upstream is unavailable")
	}
	if account == nil || account.Platform != PlatformGrok {
		return nil, nil, errors.New("grok account is required")
	}
	token, err := p.token(ctx, account)
	if err != nil {
		return nil, nil, err
	}
	return p.doJSONWithToken(ctx, account, endpoint, requestID, method, body, token)
}

func (p *GrokVideoProvider) doJSONWithToken(ctx context.Context, account *Account, endpoint GrokMediaEndpoint, requestID, method string, body []byte, token string) ([]byte, http.Header, error) {
	if p == nil || p.upstream == nil {
		return nil, nil, errors.New("grok video upstream is unavailable")
	}
	if account == nil || account.Platform != PlatformGrok {
		return nil, nil, errors.New("grok account is required")
	}
	targetURL, err := buildGrokMediaURL(account, p.cfg, endpoint, requestID)
	if err != nil {
		return nil, nil, err
	}
	upstreamCtx, release := detachUpstreamContext(ctx)
	defer release()
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(upstreamCtx, method, targetURL, bodyReader)
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	if account.IsGrokOAuth() && isGrokCLIProxyTarget(targetURL) {
		applyGrokCLIHeaders(request.Header)
	}
	account.ApplyHeaderOverrides(request.Header)
	response, err := p.do(request, account)
	if err != nil {
		return nil, nil, err
	}
	responseBody := readGrokVideoResponse(response)
	if response.StatusCode >= http.StatusMultipleChoices {
		return nil, nil, grokVideoProviderHTTPError(response.StatusCode, responseBody, false)
	}
	return responseBody, response.Header.Clone(), nil
}

func (p *GrokVideoProvider) do(request *http.Request, account *Account) (*http.Response, error) {
	proxyURL := ""
	if account != nil && account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	return p.upstream.Do(request, proxyURL, account.ID, account.Concurrency)
}

func (p *GrokVideoProvider) token(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("grok account is required")
	}
	if account.Type == AccountTypeAPIKey {
		token := strings.TrimSpace(account.GetCredential("api_key"))
		if token == "" {
			return "", errors.New("grok API key is missing")
		}
		return token, nil
	}
	if !account.IsGrokOAuth() || p.tokenProvider == nil {
		return "", errors.New("grok OAuth token provider is unavailable")
	}
	return p.tokenProvider.GetAccessToken(ctx, account)
}

func readGrokVideoResponse(response *http.Response) []byte {
	if response == nil || response.Body == nil {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, grokVideoResponseReadLimit+1))
	_ = response.Body.Close()
	if int64(len(body)) > grokVideoResponseReadLimit {
		return nil
	}
	return body
}

func grokVideoProviderSubmissionError(err error) error {
	var providerError VideoProviderError
	if errors.As(err, &providerError) {
		return providerError
	}
	return NewVideoProviderError(http.StatusBadGateway, "upstream_timeout", true, true, err)
}

func grokVideoProviderPollError(err error) error {
	var providerError VideoProviderError
	if errors.As(err, &providerError) {
		return providerError
	}
	return NewVideoProviderError(http.StatusBadGateway, "upstream_timeout", true, false, err)
}

func grokVideoProviderHTTPError(statusCode int, body []byte, ambiguous bool) VideoProviderError {
	if isGrokContentPolicyRejection(statusCode, body) {
		return NewVideoProviderError(statusCode, "content_rejected", false, false, errors.New("grok content policy rejection"))
	}
	switch {
	case statusCode == http.StatusBadRequest:
		return NewVideoProviderError(statusCode, "invalid_request", false, false, errors.New("grok video request rejected"))
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden || statusCode == http.StatusPaymentRequired:
		return NewVideoProviderError(statusCode, "upstream_authentication", true, false, errors.New("grok video authentication rejected"))
	case statusCode == http.StatusTooManyRequests:
		return NewVideoProviderError(statusCode, "upstream_rate_limit", true, false, errors.New("grok video rate limited"))
	case statusCode >= http.StatusInternalServerError:
		return NewVideoProviderError(statusCode, "upstream_unavailable", true, ambiguous, errors.New("grok video upstream unavailable"))
	default:
		return NewVideoProviderError(statusCode, "upstream_error", false, ambiguous, errors.New("grok video upstream error"))
	}
}

func grokVideoTaskStatus(raw string) VideoTaskStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "queued", "pending", "submitted":
		return VideoTaskQueued
	case "running", "processing", "in_progress", "in-progress":
		return VideoTaskRunning
	case "completed", "complete", "succeeded", "success", "done":
		return VideoTaskSucceeded
	case "failed", "error":
		return VideoTaskFailed
	case "cancelled", "canceled":
		return VideoTaskCancelled
	default:
		return VideoTaskUnknown
	}
}

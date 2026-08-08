package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

const (
	seedanceArkBaseURL          = "https://ark.cn-beijing.volces.com"
	seedanceTasksPath           = "/api/v3/contents/generations/tasks"
	seedanceResponseReadLimit   = 8 << 20
	seedanceContentTypeText     = "text"
	seedanceContentTypeImageURL = "image_url"
	seedanceContentTypeVideoURL = "video_url"
)

// SeedanceVideoProvider implements the documented Volcengine Ark video-task
// contract. It purposefully exposes only generation until verified task API
// fixtures exist for edit and extension operations.
type SeedanceVideoProvider struct {
	upstream           HTTPUpstream
	validateResolvedIP SeedanceResolvedIPValidator
}

// SeedanceResolvedIPValidator validates a hostname at the adapter boundary.
// The shared HTTP upstream repeats the same check before its real request.
type SeedanceResolvedIPValidator func(context.Context, string) error

func NewSeedanceVideoProvider(upstream HTTPUpstream, validators ...SeedanceResolvedIPValidator) *SeedanceVideoProvider {
	validator := defaultSeedanceResolvedIPValidator
	if len(validators) > 0 && validators[0] != nil {
		validator = validators[0]
	}
	return &SeedanceVideoProvider{upstream: upstream, validateResolvedIP: validator}
}

func (p *SeedanceVideoProvider) Name() string { return VideoProviderSeedance }

func (p *SeedanceVideoProvider) Capabilities() VideoProviderCapabilities {
	// Model capabilities are configured through VideoCapabilityCatalog keys.
	// No provider-wide fallback is safe because upstream Seedance models differ.
	return VideoProviderCapabilities{}
}

func (p *SeedanceVideoProvider) Submit(ctx context.Context, account *Account, request CanonicalVideoRequest, _ string) (VideoSubmitResult, error) {
	if request.Operation != VideoOperationGeneration {
		return VideoSubmitResult{}, NewVideoProviderError(http.StatusBadRequest, "unsupported_capability", false, false, errors.New("seedance supports generation only"))
	}
	body, err := buildSeedanceVideoSubmissionBody(account, request)
	if err != nil {
		return VideoSubmitResult{}, NewVideoProviderError(http.StatusBadRequest, "invalid_request", false, false, err)
	}
	responseBody, statusCode, err := p.doJSON(ctx, account, http.MethodPost, seedanceTasksPath, body)
	if err != nil {
		return VideoSubmitResult{}, seedanceSubmissionError(err)
	}
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return VideoSubmitResult{}, seedanceSubmissionHTTPError(statusCode, responseBody)
	}
	upstreamTaskID := strings.TrimSpace(seedanceJSONText(responseBody, "id"))
	if upstreamTaskID == "" {
		return VideoSubmitResult{}, NewVideoProviderError(http.StatusBadGateway, "provider_contract_error", true, true, errors.New("seedance submission response has no task ID"))
	}
	return VideoSubmitResult{UpstreamTaskID: upstreamTaskID, Status: VideoTaskQueued}, nil
}

// RecoverSubmission does not issue an Ark list query: the verified task API
// has no server-side submission-token field to correlate safely. Reporting no
// match prevents ambiguous sent submissions from being charged twice.
func (p *SeedanceVideoProvider) RecoverSubmission(context.Context, *Account, VideoTask, string) (VideoSubmitResult, bool, error) {
	return VideoSubmitResult{}, false, nil
}

func (p *SeedanceVideoProvider) Poll(ctx context.Context, account *Account, task VideoTask) (VideoPollResult, error) {
	upstreamTaskID := ""
	if task.UpstreamTaskID != nil {
		upstreamTaskID = strings.TrimSpace(*task.UpstreamTaskID)
	}
	if upstreamTaskID == "" {
		return VideoPollResult{}, NewVideoProviderError(http.StatusBadRequest, "invalid_request", false, false, errors.New("seedance task ID is required"))
	}
	endpoint := seedanceTasksPath + "/" + url.PathEscape(upstreamTaskID)
	body, statusCode, err := p.doJSON(ctx, account, http.MethodGet, endpoint, nil)
	if err != nil {
		return VideoPollResult{}, seedancePollError(err)
	}
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return VideoPollResult{}, seedancePollHTTPError(statusCode, body)
	}
	upstreamStatus := strings.TrimSpace(seedanceJSONText(body, "status"))
	if upstreamStatus == "" {
		return VideoPollResult{}, NewVideoProviderError(http.StatusBadGateway, "provider_contract_error", true, false, errors.New("seedance task response has no status"))
	}
	result := VideoPollResult{
		Status:                seedanceVideoTaskStatus(upstreamStatus),
		UpstreamStatus:        upstreamStatus,
		ResultURL:             strings.TrimSpace(seedanceJSONText(body, "content.video_url")),
		LastFrameURL:          strings.TrimSpace(seedanceJSONText(body, "content.last_frame_url")),
		ActualDurationSeconds: seedanceJSONInt(body, "duration"),
		Resolution:            strings.TrimSpace(seedanceJSONText(body, "resolution")),
		AspectRatio:           strings.TrimSpace(seedanceJSONText(body, "ratio")),
	}
	if result.Status == VideoTaskFailed {
		result.Error = NewVideoTaskError("UPSTREAM_TASK_FAILED", "upstream video task failed", false)
	}
	return result, nil
}

// OpenContent streams the signed result URL retained on the durable task. Ark
// task responses supply this URL; no Ark bearer credential is forwarded to it.
func (p *SeedanceVideoProvider) OpenContent(ctx context.Context, _ *Account, task VideoTask) (io.ReadCloser, http.Header, int64, error) {
	if p == nil || p.upstream == nil {
		return nil, nil, 0, NewVideoProviderError(http.StatusBadGateway, "upstream_unavailable", true, false, errors.New("seedance upstream is unavailable"))
	}
	if task.ResultURL == nil || strings.TrimSpace(*task.ResultURL) == "" {
		return nil, nil, 0, NewVideoProviderError(http.StatusBadRequest, "invalid_request", false, false, errors.New("seedance result URL is required"))
	}
	targetURL, err := seedanceValidatedContentURL(strings.TrimSpace(*task.ResultURL))
	if err != nil {
		return nil, nil, 0, NewVideoProviderError(http.StatusBadRequest, "invalid_request", false, false, err)
	}
	requestContext := WithHTTPUpstreamResolvedIPValidation(WithHTTPUpstreamRedirectsDisabled(nonNilVideoContext(ctx)))
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, nil, 0, NewVideoProviderError(http.StatusBadGateway, "upstream_error", true, false, err)
	}
	request.Header.Set("Accept", "*/*")
	if err := p.validateRequestDestination(request); err != nil {
		return nil, nil, 0, NewVideoProviderError(http.StatusBadRequest, "invalid_request", false, false, err)
	}
	response, err := p.upstream.Do(request, "", 0, 0)
	if err != nil {
		return nil, nil, 0, NewVideoProviderError(http.StatusBadGateway, "upstream_timeout", true, false, err)
	}
	if response == nil || response.Body == nil {
		return nil, nil, 0, NewVideoProviderError(http.StatusBadGateway, "provider_contract_error", true, false, errors.New("seedance content response is empty"))
	}
	if response.StatusCode >= http.StatusMultipleChoices {
		body := readSeedanceResponse(response)
		return nil, nil, 0, seedancePollHTTPError(response.StatusCode, body)
	}
	return response.Body, response.Header.Clone(), response.ContentLength, nil
}

type seedanceSubmissionPayload struct {
	Model           string                `json:"model"`
	Content         []seedanceContentPart `json:"content"`
	Duration        int                   `json:"duration,omitempty"`
	Resolution      string                `json:"resolution,omitempty"`
	Ratio           string                `json:"ratio,omitempty"`
	GenerateAudio   *bool                 `json:"generate_audio,omitempty"`
	Seed            *int64                `json:"seed,omitempty"`
	Watermark       *bool                 `json:"watermark,omitempty"`
	ReturnLastFrame *bool                 `json:"return_last_frame,omitempty"`
	ServiceTier     *string               `json:"service_tier,omitempty"`
}

type seedanceContentPart struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	ImageURL *seedanceMediaURL `json:"image_url,omitempty"`
	VideoURL *seedanceMediaURL `json:"video_url,omitempty"`
	Role     string            `json:"role,omitempty"`
}

type seedanceMediaURL struct {
	URL string `json:"url"`
}

func buildSeedanceVideoSubmissionBody(account *Account, request CanonicalVideoRequest) ([]byte, error) {
	if err := validateSeedanceAccount(account); err != nil {
		return nil, err
	}
	model := strings.TrimSpace(account.GetMappedModel(request.Model))
	if model == "" {
		return nil, errors.New("seedance model is required")
	}
	payload := seedanceSubmissionPayload{Model: model, Duration: request.DurationSeconds, Resolution: strings.TrimSpace(request.Resolution), Ratio: strings.TrimSpace(request.AspectRatio)}
	if request.Audio != nil {
		audio := *request.Audio
		payload.GenerateAudio = &audio
	}
	if prompt := strings.TrimSpace(request.Prompt); prompt != "" {
		payload.Content = append(payload.Content, seedanceContentPart{Type: seedanceContentTypeText, Text: prompt})
	}
	appendImages := func(assets []VideoAsset, role string) {
		for _, asset := range assets {
			payload.Content = append(payload.Content, seedanceContentPart{Type: seedanceContentTypeImageURL, ImageURL: &seedanceMediaURL{URL: asset.URL}, Role: role})
		}
	}
	appendImages(request.FirstFrame, "first_frame")
	appendImages(request.LastFrame, "last_frame")
	appendImages(request.ReferenceImages, "reference_image")
	for _, asset := range request.ReferenceVideos {
		payload.Content = append(payload.Content, seedanceContentPart{Type: seedanceContentTypeVideoURL, VideoURL: &seedanceMediaURL{URL: asset.URL}, Role: "reference_video"})
	}
	if len(payload.Content) == 0 {
		return nil, errors.New("seedance content is required")
	}
	if raw, ok := request.ProviderOptions[VideoProviderSeedance]; ok && len(bytes.TrimSpace(raw)) > 0 {
		if err := applySeedanceProviderOptions(&payload, raw); err != nil {
			return nil, err
		}
	}
	return json.Marshal(payload)
}

func applySeedanceProviderOptions(payload *seedanceSubmissionPayload, raw json.RawMessage) error {
	var options map[string]json.RawMessage
	if err := json.Unmarshal(raw, &options); err != nil || options == nil {
		return errors.New("seedance provider options are invalid")
	}
	if value, ok := options["seed"]; ok {
		var parsed int64
		if err := json.Unmarshal(value, &parsed); err != nil {
			return errors.New("seedance seed is invalid")
		}
		payload.Seed = &parsed
	}
	if value, ok := options["watermark"]; ok {
		var parsed bool
		if err := json.Unmarshal(value, &parsed); err != nil {
			return errors.New("seedance watermark is invalid")
		}
		payload.Watermark = &parsed
	}
	if value, ok := options["return_last_frame"]; ok {
		var parsed bool
		if err := json.Unmarshal(value, &parsed); err != nil {
			return errors.New("seedance return_last_frame is invalid")
		}
		payload.ReturnLastFrame = &parsed
	}
	if value, ok := options["service_tier"]; ok {
		var parsed string
		if err := json.Unmarshal(value, &parsed); err != nil {
			return errors.New("seedance service_tier is invalid")
		}
		payload.ServiceTier = &parsed
	}
	return nil
}

func validateSeedanceAccount(account *Account) error {
	if account == nil || account.Platform != PlatformVideo || account.VideoProvider() != VideoProviderSeedance || account.Type != AccountTypeAPIKey {
		return errors.New("seedance API-key video account is required")
	}
	if strings.TrimSpace(account.GetCredential("api_key")) == "" {
		return errors.New("seedance API key is required")
	}
	_, err := seedanceBaseURL(account)
	return err
}

func seedanceBaseURL(account *Account) (string, error) {
	raw := seedanceArkBaseURL
	if account != nil && strings.TrimSpace(account.GetCredential("base_url")) != "" {
		raw = account.GetCredential("base_url")
	}
	return seedanceValidatedBaseURL(raw)
}

func seedanceValidatedBaseURL(raw string) (string, error) {
	normalized, err := urlvalidator.ValidateHTTPSURL(raw, urlvalidator.ValidationOptions{AllowPrivate: false})
	if err != nil {
		return "", errors.New("base URL rejected by URL security policy")
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return "", errors.New("base URL rejected by URL security policy")
	}
	return normalized, nil
}

func seedanceValidatedContentURL(raw string) (string, error) {
	normalized, err := urlvalidator.ValidateHTTPSURL(raw, urlvalidator.ValidationOptions{AllowPrivate: false})
	if err != nil {
		return "", errors.New("content URL rejected by URL security policy")
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("content URL rejected by URL security policy")
	}
	return normalized, nil
}

func (p *SeedanceVideoProvider) doJSON(ctx context.Context, account *Account, method, endpoint string, body []byte) ([]byte, int, error) {
	if p == nil || p.upstream == nil {
		return nil, 0, errors.New("seedance upstream is unavailable")
	}
	if err := validateSeedanceAccount(account); err != nil {
		return nil, 0, err
	}
	baseURL, err := seedanceBaseURL(account)
	if err != nil {
		return nil, 0, err
	}
	targetURL := strings.TrimRight(baseURL, "/") + endpoint
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	requestContext := WithHTTPUpstreamResolvedIPValidation(WithHTTPUpstreamRedirectsDisabled(nonNilVideoContext(ctx)))
	request, err := http.NewRequestWithContext(requestContext, method, targetURL, reader)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(account.GetCredential("api_key")))
	request.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	if err := p.validateRequestDestination(request); err != nil {
		return nil, 0, NewVideoProviderError(http.StatusBadRequest, "invalid_request", false, false, err)
	}
	account.ApplyHeaderOverrides(request.Header)
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	response, err := p.upstream.Do(request, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return nil, 0, err
	}
	if response == nil || response.Body == nil {
		return nil, 0, errors.New("seedance response is empty")
	}
	return readSeedanceResponse(response), response.StatusCode, nil
}

func readSeedanceResponse(response *http.Response) []byte {
	if response == nil || response.Body == nil {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, seedanceResponseReadLimit+1))
	_ = response.Body.Close()
	if int64(len(body)) > seedanceResponseReadLimit {
		return nil
	}
	return body
}

func seedanceSubmissionError(err error) error {
	var providerError VideoProviderError
	if errors.As(err, &providerError) {
		return providerError
	}
	return NewVideoProviderError(http.StatusBadGateway, "upstream_timeout", true, true, err)
}

func seedancePollError(err error) error {
	var providerError VideoProviderError
	if errors.As(err, &providerError) {
		return providerError
	}
	return NewVideoProviderError(http.StatusBadGateway, "upstream_timeout", true, false, err)
}

func seedanceSubmissionHTTPError(statusCode int, body []byte) VideoProviderError {
	if statusCode == http.StatusBadRequest {
		if seedanceSensitiveContentError(body) {
			return NewVideoProviderError(statusCode, "content_rejected", false, false, errors.New("seedance content rejected"))
		}
		return NewVideoProviderError(statusCode, "invalid_request", false, false, errors.New("seedance request rejected"))
	}
	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden || statusCode == http.StatusPaymentRequired:
		return NewVideoProviderError(statusCode, "upstream_authentication", false, false, errors.New("seedance authentication rejected"))
	case statusCode == http.StatusTooManyRequests:
		if seedanceSubmissionProvesRejection(body) {
			return NewVideoProviderError(statusCode, "upstream_rate_limit", true, false, errors.New("seedance rate limited"))
		}
		return NewVideoProviderError(statusCode, "upstream_rate_limit", false, true, errors.New("seedance rate limited"))
	case statusCode >= http.StatusInternalServerError:
		return NewVideoProviderError(statusCode, "upstream_unavailable", false, true, errors.New("seedance upstream unavailable"))
	default:
		return NewVideoProviderError(statusCode, "upstream_error", false, true, errors.New("seedance upstream error"))
	}
}

func seedanceSubmissionProvesRejection(body []byte) bool {
	var response struct {
		ID    string `json:"id"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &response) != nil || strings.TrimSpace(response.ID) != "" || response.Error == nil {
		return false
	}
	// QuotaExceeded is the documented Ark 429 task-creation rejection. Other
	// proxy or malformed 429/5xx bodies cannot prove no task was accepted.
	return strings.EqualFold(strings.TrimSpace(response.Error.Code), "QuotaExceeded")
}

func seedancePollHTTPError(statusCode int, body []byte) VideoProviderError {
	if statusCode == http.StatusBadRequest {
		if seedanceSensitiveContentError(body) {
			return NewVideoProviderError(statusCode, "content_rejected", false, false, errors.New("seedance content rejected"))
		}
		return NewVideoProviderError(statusCode, "invalid_request", false, false, errors.New("seedance request rejected"))
	}
	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden || statusCode == http.StatusPaymentRequired:
		return NewVideoProviderError(statusCode, "upstream_authentication", false, false, errors.New("seedance authentication rejected"))
	case statusCode == http.StatusTooManyRequests:
		return NewVideoProviderError(statusCode, "upstream_rate_limit", true, false, errors.New("seedance rate limited"))
	case statusCode >= http.StatusInternalServerError:
		return NewVideoProviderError(statusCode, "upstream_unavailable", true, false, errors.New("seedance upstream unavailable"))
	default:
		return NewVideoProviderError(statusCode, "upstream_error", false, false, errors.New("seedance upstream error"))
	}
}

func seedanceSensitiveContentError(body []byte) bool {
	return strings.Contains(strings.ToLower(string(body)), "sensitivecontent")
}

func seedanceVideoTaskStatus(raw string) VideoTaskStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "queued":
		return VideoTaskQueued
	case "running":
		return VideoTaskRunning
	case "succeeded":
		return VideoTaskSucceeded
	case "failed":
		return VideoTaskFailed
	case "cancelled":
		return VideoTaskCancelled
	default:
		return VideoTaskUnknown
	}
}

func seedanceJSONText(body []byte, path string) string {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return ""
	}
	for _, component := range strings.Split(path, ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		value, ok = object[component]
		if !ok {
			return ""
		}
	}
	text, _ := value.(string)
	return text
}

func seedanceJSONInt(body []byte, path string) int {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return 0
	}
	for _, component := range strings.Split(path, ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return 0
		}
		value, ok = object[component]
		if !ok {
			return 0
		}
	}
	switch number := value.(type) {
	case float64:
		return int(number)
	case string:
		var parsed int
		_, _ = fmt.Sscan(number, &parsed)
		return parsed
	default:
		return 0
	}
}

func nonNilVideoContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func defaultSeedanceResolvedIPValidator(_ context.Context, host string) error {
	return urlvalidator.ValidateResolvedIP(host)
}

func (p *SeedanceVideoProvider) validateRequestDestination(request *http.Request) error {
	if request == nil || request.URL == nil || strings.TrimSpace(request.URL.Hostname()) == "" {
		return errors.New("seedance request URL is invalid")
	}
	validator := defaultSeedanceResolvedIPValidator
	if p != nil && p.validateResolvedIP != nil {
		validator = p.validateResolvedIP
	}
	return validator(request.Context(), request.URL.Hostname())
}

package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

const (
	klingDefaultBaseURL       = "https://api-singapore.klingai.com"
	klingTextToVideoPath      = "/v1/videos/text2video"
	klingImageToVideoPath     = "/v1/videos/image2video"
	klingVideoExtendPath      = "/v1/videos/video-extend"
	klingResponseReadLimit    = 8 << 20
	klingTaskKindTextToVideo  = "text2video"
	klingTaskKindImageToVideo = "image2video"
	klingTaskKindVideoExtend  = "video-extend"
)

var klingOpaqueIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)

var errKlingUnsupportedCapability = errors.New("unsupported kling capability")

type KlingResolvedIPValidator func(context.Context, string) error

type klingJWTCacheEntry struct {
	token     string
	expiresAt time.Time
}

// KlingVideoProvider implements only the verified direct Kling Open Platform
// legacy task contract. Model-specific capabilities remain catalog-driven.
type KlingVideoProvider struct {
	upstream           HTTPUpstream
	clock              KlingClock
	validateResolvedIP KlingResolvedIPValidator
	tokenMu            sync.Mutex
	tokens             map[[32]byte]klingJWTCacheEntry
}

func NewKlingVideoProvider(upstream HTTPUpstream, clock KlingClock, validators ...KlingResolvedIPValidator) *KlingVideoProvider {
	if clock == nil {
		clock = klingSystemClock{}
	}
	validator := defaultKlingResolvedIPValidator
	if len(validators) > 0 && validators[0] != nil {
		validator = validators[0]
	}
	return &KlingVideoProvider{
		upstream: upstream, clock: clock, validateResolvedIP: validator,
		tokens: make(map[[32]byte]klingJWTCacheEntry),
	}
}

func (p *KlingVideoProvider) Name() string { return VideoProviderKling }

func (p *KlingVideoProvider) Capabilities() VideoProviderCapabilities {
	return VideoProviderCapabilities{}
}

func (p *KlingVideoProvider) Submit(ctx context.Context, account *Account, request CanonicalVideoRequest, submissionToken string) (VideoSubmitResult, error) {
	kind, err := klingProviderTaskKind(request)
	if err != nil {
		if errors.Is(err, errKlingUnsupportedCapability) {
			return VideoSubmitResult{}, NewVideoProviderError(http.StatusBadRequest, "unsupported_capability", false, false, err)
		}
		return VideoSubmitResult{}, klingRequestError(err)
	}
	payload, err := buildKlingSubmissionPayload(account, request, kind, submissionToken)
	if err != nil {
		return VideoSubmitResult{}, klingRequestError(err)
	}
	body, statusCode, err := p.doJSON(ctx, account, http.MethodPost, klingPathForTaskKind(kind), payload)
	if err != nil {
		return VideoSubmitResult{}, klingSubmissionError(err)
	}
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return VideoSubmitResult{}, klingSubmissionHTTPError(statusCode)
	}
	response, err := decodeKlingTaskResponse(body)
	if err != nil || strings.TrimSpace(response.Data.TaskID) == "" || strings.TrimSpace(response.Data.TaskStatus) == "" {
		return VideoSubmitResult{}, NewVideoProviderError(http.StatusBadGateway, "provider_contract_error", true, true, errors.New("kling create response is invalid"))
	}
	if strings.TrimSpace(response.Data.TaskInfo.ExternalTaskID) != strings.TrimSpace(submissionToken) {
		return VideoSubmitResult{}, NewVideoProviderError(http.StatusBadGateway, "provider_contract_error", true, true, errors.New("kling create response has mismatched external task ID"))
	}
	return VideoSubmitResult{
		UpstreamTaskID: strings.TrimSpace(response.Data.TaskID),
		Status:         klingVideoTaskStatus(response.Data.TaskStatus),
		UpstreamStatus: strings.TrimSpace(response.Data.TaskStatus),
	}, nil
}

// RecoverSubmission remains disabled until authenticated paid-task fixtures
// prove the external_task_id query semantics. Public documentation alone is
// not sufficient to make an ambiguity-recovery request safe.
func (p *KlingVideoProvider) RecoverSubmission(context.Context, *Account, VideoTask, string) (VideoSubmitResult, bool, error) {
	return VideoSubmitResult{}, false, nil
}

func (p *KlingVideoProvider) Poll(ctx context.Context, account *Account, task VideoTask) (VideoPollResult, error) {
	upstreamTaskID := ""
	if task.UpstreamTaskID != nil {
		upstreamTaskID = strings.TrimSpace(*task.UpstreamTaskID)
	}
	if !klingOpaqueIDPattern.MatchString(upstreamTaskID) {
		return VideoPollResult{}, klingRequestError(errors.New("kling task ID is invalid"))
	}
	kind, err := klingTaskKindFromDurableTask(task)
	if err != nil {
		return VideoPollResult{}, NewVideoProviderError(http.StatusBadGateway, "provider_contract_error", false, false, err)
	}
	body, statusCode, err := p.doJSON(ctx, account, http.MethodGet, klingPathForTaskKind(kind)+"/"+url.PathEscape(upstreamTaskID), nil)
	if err != nil {
		return VideoPollResult{}, klingPollError(err)
	}
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return VideoPollResult{}, klingPollHTTPError(statusCode)
	}
	response, err := decodeKlingTaskResponse(body)
	if err != nil || strings.TrimSpace(response.Data.TaskID) == "" || strings.TrimSpace(response.Data.TaskStatus) == "" {
		return VideoPollResult{}, NewVideoProviderError(http.StatusBadGateway, "provider_contract_error", true, false, errors.New("kling query response is invalid"))
	}
	if strings.TrimSpace(response.Data.TaskID) != upstreamTaskID {
		return VideoPollResult{}, NewVideoProviderError(http.StatusBadGateway, "provider_contract_error", true, false, errors.New("kling query response has mismatched task ID"))
	}
	result := VideoPollResult{
		Status:         klingVideoTaskStatus(response.Data.TaskStatus),
		UpstreamStatus: strings.TrimSpace(response.Data.TaskStatus),
	}
	if result.Status == VideoTaskSucceeded {
		if response.Data.TaskResult == nil || len(response.Data.TaskResult.Videos) == 0 {
			return VideoPollResult{}, NewVideoProviderError(http.StatusBadGateway, "provider_contract_error", true, false, errors.New("kling succeeded response has no video"))
		}
		video := response.Data.TaskResult.Videos[0]
		result.ResultURL = strings.TrimSpace(video.URL)
		if result.ResultURL == "" {
			return VideoPollResult{}, NewVideoProviderError(http.StatusBadGateway, "provider_contract_error", true, false, errors.New("kling succeeded response has no video URL"))
		}
		duration, parseErr := strconv.Atoi(strings.TrimSpace(video.Duration))
		if parseErr != nil || duration < 0 {
			return VideoPollResult{}, NewVideoProviderError(http.StatusBadGateway, "provider_contract_error", true, false, errors.New("kling succeeded response has invalid duration"))
		}
		result.ActualDurationSeconds = duration
	}
	if result.Status == VideoTaskFailed {
		result.Error = NewVideoTaskError("UPSTREAM_TASK_FAILED", "upstream video task failed", false)
	}
	return result, nil
}

func (p *KlingVideoProvider) OpenContent(ctx context.Context, _ *Account, task VideoTask) (io.ReadCloser, http.Header, int64, error) {
	if p == nil || p.upstream == nil {
		return nil, nil, 0, NewVideoProviderError(http.StatusBadGateway, "upstream_unavailable", true, false, errors.New("kling upstream is unavailable"))
	}
	if task.ResultURL == nil || strings.TrimSpace(*task.ResultURL) == "" {
		return nil, nil, 0, klingRequestError(errors.New("kling result URL is required"))
	}
	targetURL, err := klingValidatedContentURL(*task.ResultURL)
	if err != nil {
		return nil, nil, 0, klingRequestError(err)
	}
	requestContext := WithHTTPUpstreamResolvedIPValidation(WithHTTPUpstreamRedirectsDisabled(nonNilVideoContext(ctx)))
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, nil, 0, NewVideoProviderError(http.StatusBadGateway, "upstream_error", true, false, err)
	}
	request.Header.Set("Accept", "*/*")
	if err := p.validateRequestDestination(request); err != nil {
		return nil, nil, 0, klingRequestError(err)
	}
	response, err := p.upstream.Do(request, "", 0, 0)
	if err != nil {
		return nil, nil, 0, NewVideoProviderError(http.StatusBadGateway, "upstream_timeout", true, false, err)
	}
	if response == nil || response.Body == nil {
		return nil, nil, 0, NewVideoProviderError(http.StatusBadGateway, "provider_contract_error", true, false, errors.New("kling content response is empty"))
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_ = readKlingResponse(response)
		return nil, nil, 0, klingPollHTTPError(response.StatusCode)
	}
	return response.Body, response.Header.Clone(), response.ContentLength, nil
}

type klingSubmissionPayload struct {
	ModelName      string              `json:"model_name,omitempty"`
	Image          string              `json:"image,omitempty"`
	ImageTail      string              `json:"image_tail,omitempty"`
	VideoID        string              `json:"video_id,omitempty"`
	Prompt         string              `json:"prompt,omitempty"`
	NegativePrompt string              `json:"negative_prompt,omitempty"`
	Duration       string              `json:"duration,omitempty"`
	Mode           string              `json:"mode,omitempty"`
	Sound          string              `json:"sound,omitempty"`
	AspectRatio    string              `json:"aspect_ratio,omitempty"`
	CFGScale       *float64            `json:"cfg_scale,omitempty"`
	WatermarkInfo  *klingWatermarkInfo `json:"watermark_info,omitempty"`
	ExternalTaskID string              `json:"external_task_id"`
}

type klingWatermarkInfo struct {
	Enabled bool `json:"enabled"`
}

type klingProviderOptions struct {
	VideoID        string
	NegativePrompt string
	Mode           string
	Sound          string
	CFGScale       *float64
	Watermark      *bool
}

func buildKlingSubmissionPayload(account *Account, request CanonicalVideoRequest, kind, submissionToken string) ([]byte, error) {
	if err := validateKlingAccount(account); err != nil {
		return nil, err
	}
	submissionToken = strings.TrimSpace(submissionToken)
	if !klingOpaqueIDPattern.MatchString(submissionToken) {
		return nil, errors.New("kling submission token is invalid")
	}
	options, err := parseKlingProviderOptions(request.ProviderOptions)
	if err != nil {
		return nil, err
	}
	payload := klingSubmissionPayload{
		Prompt: strings.TrimSpace(request.Prompt), NegativePrompt: options.NegativePrompt,
		ExternalTaskID: submissionToken,
	}
	if request.Resolution != "" || request.Audio != nil {
		return nil, errors.New("kling request contains unsupported fields")
	}
	switch kind {
	case klingTaskKindTextToVideo:
		if payload.Prompt == "" {
			return nil, errors.New("kling text prompt is required")
		}
		if options.VideoID != "" || options.CFGScale != nil || options.Watermark != nil {
			return nil, errors.New("kling text-to-video options are invalid")
		}
		payload.ModelName = strings.TrimSpace(account.GetMappedModel(request.Model))
		payload.Mode = options.Mode
		payload.Sound = options.Sound
		payload.AspectRatio = strings.TrimSpace(request.AspectRatio)
	case klingTaskKindImageToVideo:
		if request.AspectRatio != "" || options.VideoID != "" || options.CFGScale != nil || options.Watermark != nil {
			return nil, errors.New("kling image-to-video options are invalid")
		}
		payload.ModelName = strings.TrimSpace(account.GetMappedModel(request.Model))
		payload.Mode = options.Mode
		payload.Sound = options.Sound
		if len(request.FirstFrame) > 0 {
			payload.Image = strings.TrimSpace(request.FirstFrame[0].URL)
		}
		if len(request.LastFrame) > 0 {
			payload.ImageTail = strings.TrimSpace(request.LastFrame[0].URL)
		}
	case klingTaskKindVideoExtend:
		if options.VideoID == "" || options.Mode != "" || options.Sound != "" || request.AspectRatio != "" || request.DurationSeconds != 0 {
			return nil, errors.New("kling video-extension options are invalid")
		}
		payload.VideoID = options.VideoID
		payload.CFGScale = options.CFGScale
		if options.Watermark != nil {
			payload.WatermarkInfo = &klingWatermarkInfo{Enabled: *options.Watermark}
		}
	default:
		return nil, errors.New("kling task kind is invalid")
	}
	if kind != klingTaskKindVideoExtend && request.DurationSeconds > 0 {
		payload.Duration = strconv.Itoa(request.DurationSeconds)
	}
	if kind != klingTaskKindVideoExtend && payload.ModelName == "" {
		return nil, errors.New("kling model is required")
	}
	return json.Marshal(payload)
}

func parseKlingProviderOptions(all map[string]json.RawMessage) (klingProviderOptions, error) {
	var options klingProviderOptions
	if len(all) == 0 {
		return options, nil
	}
	if len(all) != 1 {
		return options, errors.New("kling provider options are invalid")
	}
	raw, ok := all[VideoProviderKling]
	if !ok {
		return options, errors.New("kling provider options are invalid")
	}
	object, ok := decodeUniqueVideoJSONObject(raw)
	if !ok {
		return options, errors.New("kling provider options are invalid")
	}
	for name, value := range object {
		switch name {
		case "video_id", "negative_prompt", "mode", "sound":
			var parsed string
			if json.Unmarshal(value, &parsed) != nil {
				return options, errors.New("kling provider option is invalid")
			}
			parsed = strings.TrimSpace(parsed)
			if parsed == "" || len(parsed) > 20_000 || containsVideoCredential(parsed) {
				return options, errors.New("kling provider option is invalid")
			}
			switch name {
			case "video_id":
				if !klingOpaqueIDPattern.MatchString(parsed) {
					return options, errors.New("kling video ID is invalid")
				}
				options.VideoID = parsed
			case "negative_prompt":
				options.NegativePrompt = parsed
			case "mode":
				if len(parsed) > 64 {
					return options, errors.New("kling mode is invalid")
				}
				options.Mode = parsed
			case "sound":
				if len(parsed) > 64 {
					return options, errors.New("kling sound is invalid")
				}
				options.Sound = parsed
			}
		case "cfg_scale":
			var parsed float64
			if json.Unmarshal(value, &parsed) != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
				return options, errors.New("kling cfg_scale is invalid")
			}
			options.CFGScale = &parsed
		case "watermark":
			var parsed bool
			if json.Unmarshal(value, &parsed) != nil {
				return options, errors.New("kling watermark is invalid")
			}
			options.Watermark = &parsed
		default:
			return options, errors.New("kling provider option is not allowed")
		}
	}
	return options, nil
}

func klingProviderTaskKind(request CanonicalVideoRequest) (string, error) {
	switch request.Operation {
	case VideoOperationGeneration:
		if len(request.ReferenceImages) > 0 || len(request.ReferenceVideos) > 0 || len(request.FirstFrame) > 1 || len(request.LastFrame) > 1 {
			return "", fmt.Errorf("%w: generation shape", errKlingUnsupportedCapability)
		}
		if len(request.FirstFrame) > 0 || len(request.LastFrame) > 0 {
			return klingTaskKindImageToVideo, nil
		}
		return klingTaskKindTextToVideo, nil
	case VideoOperationExtension:
		if len(request.ReferenceVideos) != 1 || len(request.ReferenceImages) > 0 || len(request.FirstFrame) > 0 || len(request.LastFrame) > 0 {
			return "", fmt.Errorf("%w: extension shape", errKlingUnsupportedCapability)
		}
		return klingTaskKindVideoExtend, nil
	case VideoOperationEdit:
		return "", fmt.Errorf("%w: edit", errKlingUnsupportedCapability)
	default:
		return "", fmt.Errorf("%w: operation", errKlingUnsupportedCapability)
	}
}

func klingTaskKindFromDurableTask(task VideoTask) (string, error) {
	object, ok := decodeUniqueVideoJSONObject(task.RequestPayload)
	if !ok {
		return "", errors.New("kling durable route hint is missing")
	}
	raw, ok := object["provider_task_kind"]
	if !ok {
		return "", errors.New("kling durable route hint is missing")
	}
	var kind string
	if json.Unmarshal(raw, &kind) != nil {
		return "", errors.New("kling durable route hint is invalid")
	}
	kind = strings.TrimSpace(kind)
	switch kind {
	case klingTaskKindTextToVideo, klingTaskKindImageToVideo:
		if VideoOperation(task.Operation) != VideoOperationGeneration {
			return "", errors.New("kling durable route hint conflicts with operation")
		}
	case klingTaskKindVideoExtend:
		if VideoOperation(task.Operation) != VideoOperationExtension {
			return "", errors.New("kling durable route hint conflicts with operation")
		}
	default:
		return "", errors.New("kling durable route hint is invalid")
	}
	return kind, nil
}

func klingPathForTaskKind(kind string) string {
	switch kind {
	case klingTaskKindTextToVideo:
		return klingTextToVideoPath
	case klingTaskKindImageToVideo:
		return klingImageToVideoPath
	case klingTaskKindVideoExtend:
		return klingVideoExtendPath
	default:
		return ""
	}
}

type klingTaskResponse struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Data      struct {
		TaskID        string `json:"task_id"`
		TaskStatus    string `json:"task_status"`
		TaskStatusMsg string `json:"task_status_msg"`
		TaskInfo      struct {
			ExternalTaskID string `json:"external_task_id"`
			ParentVideo    *struct {
				ID       string `json:"id"`
				URL      string `json:"url"`
				Duration string `json:"duration"`
			} `json:"parent_video"`
		} `json:"task_info"`
		TaskResult *struct {
			Videos []struct {
				ID           string `json:"id"`
				URL          string `json:"url"`
				WatermarkURL string `json:"watermark_url"`
				Duration     string `json:"duration"`
			} `json:"videos"`
		} `json:"task_result"`
		WatermarkInfo         *klingWatermarkInfo `json:"watermark_info"`
		FinalUnitDeduction    string              `json:"final_unit_deduction"`
		FinalBalanceDeduction *struct {
			Quota     string `json:"quota"`
			ListPrice string `json:"list_price"`
		} `json:"final_balance_deduction"`
		CreatedAt int64 `json:"created_at"`
		UpdatedAt int64 `json:"updated_at"`
	} `json:"data"`
}

func decodeKlingTaskResponse(body []byte) (klingTaskResponse, error) {
	var response klingTaskResponse
	if len(body) == 0 || json.Unmarshal(body, &response) != nil || response.Code != 0 {
		return response, errors.New("kling response envelope is invalid")
	}
	return response, nil
}

func klingVideoTaskStatus(raw string) VideoTaskStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "submitted":
		return VideoTaskQueued
	case "processing":
		return VideoTaskRunning
	case "succeed":
		return VideoTaskSucceeded
	case "failed":
		return VideoTaskFailed
	default:
		return VideoTaskUnknown
	}
}

func validateKlingAccount(account *Account) error {
	if account == nil || account.Platform != PlatformVideo || account.VideoProvider() != VideoProviderKling || account.Type != AccountTypeAPIKey {
		return errors.New("kling API-key video account is required")
	}
	if strings.TrimSpace(account.GetCredential("access_key")) == "" || strings.TrimSpace(account.GetCredential("secret_key")) == "" {
		return errors.New("kling credentials are required")
	}
	_, err := klingBaseURL(account)
	return err
}

func klingBaseURL(account *Account) (string, error) {
	raw := klingDefaultBaseURL
	if account != nil && strings.TrimSpace(account.GetCredential("base_url")) != "" {
		raw = account.GetCredential("base_url")
	}
	return klingValidatedBaseURL(raw)
}

func klingValidatedBaseURL(raw string) (string, error) {
	normalized, err := urlvalidator.ValidateHTTPSURL(raw, urlvalidator.ValidationOptions{AllowPrivate: false})
	if err != nil {
		return "", errors.New("base URL rejected by URL security policy")
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("base URL rejected by URL security policy")
	}
	return strings.TrimRight(normalized, "/"), nil
}

func klingValidatedContentURL(raw string) (string, error) {
	normalized, err := urlvalidator.ValidateHTTPSURL(strings.TrimSpace(raw), urlvalidator.ValidationOptions{AllowPrivate: false})
	if err != nil {
		return "", errors.New("content URL rejected by URL security policy")
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("content URL rejected by URL security policy")
	}
	return normalized, nil
}

func (p *KlingVideoProvider) doJSON(ctx context.Context, account *Account, method, endpoint string, body []byte) ([]byte, int, error) {
	if p == nil || p.upstream == nil {
		return nil, 0, NewVideoProviderError(http.StatusBadGateway, "upstream_unavailable", true, false, errors.New("kling upstream is unavailable"))
	}
	if err := validateKlingAccount(account); err != nil {
		return nil, 0, klingRequestError(err)
	}
	baseURL, err := klingBaseURL(account)
	if err != nil {
		return nil, 0, klingRequestError(err)
	}
	token, err := p.authorizationToken(account)
	if err != nil {
		return nil, 0, NewVideoProviderError(http.StatusUnauthorized, "upstream_authentication", false, false, err)
	}
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	requestContext := WithHTTPUpstreamResolvedIPValidation(WithHTTPUpstreamRedirectsDisabled(nonNilVideoContext(ctx)))
	request, err := http.NewRequestWithContext(requestContext, method, baseURL+endpoint, reader)
	if err != nil {
		return nil, 0, NewVideoProviderError(http.StatusBadGateway, "upstream_error", false, false, err)
	}
	account.ApplyHeaderOverrides(request.Header)
	request.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if err := p.validateRequestDestination(request); err != nil {
		return nil, 0, klingRequestError(err)
	}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	response, err := p.upstream.Do(request, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return nil, 0, err
	}
	if response == nil || response.Body == nil {
		return nil, 0, errors.New("kling response is empty")
	}
	return readKlingResponse(response), response.StatusCode, nil
}

func (p *KlingVideoProvider) authorizationToken(account *Account) (string, error) {
	if p == nil {
		return "", errors.New("kling authorization is unavailable")
	}
	accessKey := strings.TrimSpace(account.GetCredential("access_key"))
	secretKey := strings.TrimSpace(account.GetCredential("secret_key"))
	cacheKey := sha256.Sum256([]byte(accessKey + "\x00" + secretKey))
	now := p.clock.Now()
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()
	if entry, ok := p.tokens[cacheKey]; ok && now.Before(entry.expiresAt.Add(-klingJWTRefreshBefore)) {
		return entry.token, nil
	}
	token, err := SignKlingJWT(accessKey, secretKey, p.clock)
	if err != nil {
		return "", err
	}
	p.tokens[cacheKey] = klingJWTCacheEntry{token: token, expiresAt: now.Add(klingJWTLifetime)}
	return token, nil
}

func readKlingResponse(response *http.Response) []byte {
	if response == nil || response.Body == nil {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, klingResponseReadLimit+1))
	_ = response.Body.Close()
	if int64(len(body)) > klingResponseReadLimit {
		return nil
	}
	return body
}

func defaultKlingResolvedIPValidator(_ context.Context, host string) error {
	return urlvalidator.ValidateResolvedIP(host)
}

func (p *KlingVideoProvider) validateRequestDestination(request *http.Request) error {
	if request == nil || request.URL == nil || strings.TrimSpace(request.URL.Hostname()) == "" {
		return errors.New("kling request URL is invalid")
	}
	validator := defaultKlingResolvedIPValidator
	if p != nil && p.validateResolvedIP != nil {
		validator = p.validateResolvedIP
	}
	return validator(request.Context(), request.URL.Hostname())
}

func klingRequestError(err error) VideoProviderError {
	return NewVideoProviderError(http.StatusBadRequest, "invalid_request", false, false, err)
}

func klingSubmissionError(err error) error {
	var providerError VideoProviderError
	if errors.As(err, &providerError) {
		return providerError
	}
	return NewVideoProviderError(http.StatusBadGateway, "upstream_timeout", true, true, err)
}

func klingPollError(err error) error {
	var providerError VideoProviderError
	if errors.As(err, &providerError) {
		return providerError
	}
	return NewVideoProviderError(http.StatusBadGateway, "upstream_timeout", true, false, err)
}

func klingSubmissionHTTPError(statusCode int) VideoProviderError {
	switch {
	case statusCode == http.StatusBadRequest || statusCode == http.StatusUnprocessableEntity:
		return NewVideoProviderError(statusCode, "invalid_request", false, false, errors.New("kling request rejected"))
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden || statusCode == http.StatusPaymentRequired:
		return NewVideoProviderError(statusCode, "upstream_authentication", false, false, errors.New("kling authentication rejected"))
	case statusCode == http.StatusTooManyRequests:
		return NewVideoProviderError(statusCode, "upstream_rate_limit", false, true, errors.New("kling rate limited"))
	case statusCode >= http.StatusInternalServerError:
		return NewVideoProviderError(statusCode, "upstream_unavailable", false, true, errors.New("kling upstream unavailable"))
	default:
		return NewVideoProviderError(statusCode, "upstream_error", false, true, errors.New("kling upstream error"))
	}
}

func klingPollHTTPError(statusCode int) VideoProviderError {
	switch {
	case statusCode == http.StatusBadRequest || statusCode == http.StatusNotFound || statusCode == http.StatusUnprocessableEntity:
		return NewVideoProviderError(statusCode, "invalid_request", false, false, errors.New("kling request rejected"))
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden || statusCode == http.StatusPaymentRequired:
		return NewVideoProviderError(statusCode, "upstream_authentication", false, false, errors.New("kling authentication rejected"))
	case statusCode == http.StatusTooManyRequests:
		return NewVideoProviderError(statusCode, "upstream_rate_limit", true, false, errors.New("kling rate limited"))
	case statusCode >= http.StatusInternalServerError:
		return NewVideoProviderError(statusCode, "upstream_unavailable", true, false, errors.New("kling upstream unavailable"))
	default:
		return NewVideoProviderError(statusCode, "upstream_error", false, false, errors.New("kling upstream error"))
	}
}

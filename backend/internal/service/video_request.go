package service

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

const (
	// MaxVideoRequestBodyBytes bounds both JSON requests and complete multipart
	// bodies before any provider adapter or billing service receives them.
	MaxVideoRequestBodyBytes = 32 << 20
	// MaxVideoRequestAssets bounds all canonical image and video assets combined.
	MaxVideoRequestAssets = 16
)

var (
	ErrVideoInvalidRequest      = infraerrors.BadRequest("VIDEO_INVALID_REQUEST", "invalid video request")
	ErrVideoInvalidMedia        = infraerrors.BadRequest("VIDEO_INVALID_MEDIA", "invalid video media")
	ErrVideoRequestBodyTooLarge = infraerrors.BadRequest("VIDEO_REQUEST_BODY_TOO_LARGE", "video request body is too large")
	ErrVideoTooManyAssets       = infraerrors.BadRequest("VIDEO_TOO_MANY_ASSETS", "video request has too many assets")

	videoResolutionPattern   = regexp.MustCompile(`^[1-9][0-9]{2,4}p$`)
	videoAspectRatioPattern  = regexp.MustCompile(`^[1-9][0-9]{0,2}:[1-9][0-9]{0,2}$`)
	videoProviderNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	videoBlockedIPPrefixes   = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("192.31.196.0/24"),
		netip.MustParsePrefix("192.52.193.0/24"),
		netip.MustParsePrefix("192.88.99.0/24"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("192.175.48.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("::/128"),
		netip.MustParsePrefix("::1/128"),
		netip.MustParsePrefix("64:ff9b:1::/48"),
		netip.MustParsePrefix("100::/64"),
		netip.MustParsePrefix("2001::/23"),
		netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("fc00::/7"),
		netip.MustParsePrefix("fe80::/10"),
		netip.MustParsePrefix("ff00::/8"),
	}
)

type VideoOperation string

const (
	VideoOperationGeneration VideoOperation = "generation"
	VideoOperationEdit       VideoOperation = "edit"
	VideoOperationExtension  VideoOperation = "extension"
)

type VideoAsset struct {
	URL string `json:"url"`
}

type CanonicalVideoRequest struct {
	Operation       VideoOperation
	Model           string
	Prompt          string
	DurationSeconds int
	Resolution      string
	AspectRatio     string
	FirstFrame      []VideoAsset
	LastFrame       []VideoAsset
	ReferenceImages []VideoAsset
	ReferenceVideos []VideoAsset
	Audio           *bool
	ProviderOptions map[string]json.RawMessage
}

// NormalizeVideoRequest converts the public JSON or legacy Grok multipart
// shapes into the single request consumed by capability checks and adapters.
func NormalizeVideoRequest(operation VideoOperation, contentType string, body []byte) (CanonicalVideoRequest, error) {
	request := CanonicalVideoRequest{Operation: operation}
	if len(body) > MaxVideoRequestBodyBytes {
		return request, ErrVideoRequestBodyTooLarge
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return request, ErrVideoInvalidRequest
	}

	var err error
	if json.Valid(body) {
		err = normalizeVideoJSONRequest(body, &request)
	} else {
		err = normalizeVideoMultipartRequest(contentType, body, &request)
	}
	if err != nil {
		return CanonicalVideoRequest{Operation: operation}, err
	}
	if err := validateCanonicalVideoRequest(&request); err != nil {
		return CanonicalVideoRequest{Operation: operation}, err
	}
	return request, nil
}

func normalizeVideoJSONRequest(body []byte, request *CanonicalVideoRequest) error {
	fields, ok := decodeUniqueVideoJSONObject(body)
	if !ok {
		return ErrVideoInvalidRequest
	}
	var err error
	if request.Model, err = videoJSONString(fields, "model"); err != nil {
		return err
	}
	if request.Prompt, err = videoJSONString(fields, "prompt"); err != nil {
		return err
	}
	if request.Resolution, err = videoJSONString(fields, "resolution"); err != nil {
		return err
	}
	if request.AspectRatio, err = videoJSONString(fields, "aspect_ratio"); err != nil {
		return err
	}
	request.Model = strings.TrimSpace(request.Model)
	request.Prompt = strings.TrimSpace(request.Prompt)
	request.Resolution = strings.ToLower(strings.TrimSpace(request.Resolution))
	request.AspectRatio = strings.TrimSpace(request.AspectRatio)

	if raw, exists := fields["duration"]; exists {
		if err := json.Unmarshal(raw, &request.DurationSeconds); err != nil || request.DurationSeconds <= 0 {
			return ErrVideoInvalidRequest
		}
	}
	if raw, exists := fields["audio"]; exists {
		var audio bool
		if err := json.Unmarshal(raw, &audio); err != nil {
			return ErrVideoInvalidRequest
		}
		request.Audio = &audio
	}
	if raw, exists := fields["provider_options"]; exists {
		if err := parseVideoProviderOptions(raw, request); err != nil {
			return err
		}
	}

	if raw, exists := fields["first_frame"]; exists {
		request.FirstFrame, err = decodeVideoAssets(raw)
		if err != nil {
			return err
		}
	}
	if raw, exists := fields["last_frame"]; exists {
		request.LastFrame, err = decodeVideoAssets(raw)
		if err != nil {
			return err
		}
	}
	if raw, exists := fields["image"]; exists {
		assets, assetErr := decodeVideoAssets(raw)
		if assetErr != nil {
			return assetErr
		}
		request.addLegacyFirstFrameAssets(assets)
	}
	if raw, exists := fields["images"]; exists {
		assets, assetErr := decodeVideoAssets(raw)
		if assetErr != nil {
			return assetErr
		}
		request.ReferenceImages = append(request.ReferenceImages, assets...)
	}
	if raw, exists := fields["reference_images"]; exists {
		assets, assetErr := decodeVideoAssets(raw)
		if assetErr != nil {
			return assetErr
		}
		request.ReferenceImages = append(request.ReferenceImages, assets...)
	}
	if raw, exists := fields["image_url"]; exists {
		assets, assetErr := decodeVideoAssets(raw)
		if assetErr != nil {
			return assetErr
		}
		request.addLegacyFirstFrameAssets(assets)
	}
	if raw, exists := fields["video"]; exists {
		assets, assetErr := decodeVideoAssets(raw)
		if assetErr != nil {
			return assetErr
		}
		request.ReferenceVideos = append(request.ReferenceVideos, assets...)
	}
	if raw, exists := fields["reference_videos"]; exists {
		assets, assetErr := decodeVideoAssets(raw)
		if assetErr != nil {
			return assetErr
		}
		request.ReferenceVideos = append(request.ReferenceVideos, assets...)
	}
	return nil
}

func normalizeVideoMultipartRequest(contentType string, body []byte, request *CanonicalVideoRequest) error {
	mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil || !strings.EqualFold(mediaType, "multipart/form-data") || strings.TrimSpace(params["boundary"]) == "" {
		return ErrVideoInvalidRequest
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	state := videoMultipartRequestState{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ErrVideoInvalidRequest
		}
		data, readErr := io.ReadAll(part)
		_ = part.Close()
		if readErr != nil {
			return ErrVideoInvalidRequest
		}
		name := strings.TrimSpace(part.FormName())
		if name == "" {
			continue
		}
		if fileName := strings.TrimSpace(part.FileName()); fileName != "" {
			if !isVideoMultipartImageField(name) {
				continue
			}
			asset, assetErr := videoMultipartImageAsset(fileName, part.Header.Get("Content-Type"), data)
			if assetErr != nil {
				return assetErr
			}
			request.addMultipartImage(name, asset)
			continue
		}
		if err := addVideoMultipartField(request, &state, name, strings.TrimSpace(string(data))); err != nil {
			return err
		}
	}
	if state.sourceVideo != nil {
		request.ReferenceVideos = append(request.ReferenceVideos, *state.sourceVideo)
	}
	request.ReferenceVideos = append(request.ReferenceVideos, state.referenceVideos...)
	return nil
}

type videoMultipartRequestState struct {
	sourceVideo     *VideoAsset
	referenceVideos []VideoAsset
}

func addVideoMultipartField(request *CanonicalVideoRequest, state *videoMultipartRequestState, name, value string) error {
	switch {
	case name == "model":
		request.Model = value
	case name == "prompt":
		request.Prompt = value
	case name == "duration":
		duration, err := strconv.Atoi(value)
		if err != nil || duration <= 0 {
			return ErrVideoInvalidRequest
		}
		request.DurationSeconds = duration
	case name == "resolution":
		request.Resolution = strings.ToLower(value)
	case name == "aspect_ratio":
		request.AspectRatio = value
	case name == "audio":
		audio, err := strconv.ParseBool(value)
		if err != nil {
			return ErrVideoInvalidRequest
		}
		request.Audio = &audio
	case name == "provider_options":
		return parseVideoProviderOptions(json.RawMessage(value), request)
	case name == "first_frame", name == "image", name == "image_url":
		if value != "" {
			request.addLegacyFirstFrameAssets([]VideoAsset{{URL: value}})
		}
	case name == "last_frame":
		if value != "" {
			request.LastFrame = append(request.LastFrame, VideoAsset{URL: value})
		}
	case name == "images", name == "images[]", name == "reference_images", name == "reference_images[]",
		strings.HasPrefix(name, "image["), strings.HasPrefix(name, "reference_images["):
		if value != "" {
			request.ReferenceImages = append(request.ReferenceImages, VideoAsset{URL: value})
		}
	case name == "video", name == "video_url":
		if value != "" {
			if state.sourceVideo != nil {
				return ErrVideoInvalidRequest
			}
			state.sourceVideo = &VideoAsset{URL: value}
		}
	case name == "reference_videos", name == "reference_videos[]", strings.HasPrefix(name, "reference_videos["):
		if value != "" {
			state.referenceVideos = append(state.referenceVideos, VideoAsset{URL: value})
		}
	}
	return nil
}

func (request *CanonicalVideoRequest) addLegacyFirstFrameAssets(assets []VideoAsset) {
	if len(assets) == 0 {
		return
	}
	if len(request.FirstFrame) == 0 {
		request.FirstFrame = append(request.FirstFrame, assets[0])
		assets = assets[1:]
	}
	request.ReferenceImages = append(request.ReferenceImages, assets...)
}

func (request *CanonicalVideoRequest) addMultipartImage(name string, asset VideoAsset) {
	switch {
	case name == "last_frame":
		request.LastFrame = append(request.LastFrame, asset)
	case name == "first_frame", name == "image", name == "image_url":
		request.addLegacyFirstFrameAssets([]VideoAsset{asset})
	case strings.HasPrefix(name, "image[") && len(request.FirstFrame) == 0:
		request.addLegacyFirstFrameAssets([]VideoAsset{asset})
	default:
		request.ReferenceImages = append(request.ReferenceImages, asset)
	}
}

func isVideoMultipartImageField(name string) bool {
	switch name {
	case "first_frame", "last_frame", "image", "image_url", "images", "images[]", "reference_images", "reference_images[]":
		return true
	default:
		return strings.HasPrefix(name, "image[") || strings.HasPrefix(name, "reference_images[")
	}
}

func videoMultipartImageAsset(fileName, contentType string, data []byte) (VideoAsset, error) {
	if len(data) == 0 {
		return VideoAsset{}, ErrVideoInvalidMedia
	}
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = strings.ToLower(strings.TrimSpace(mime.TypeByExtension(filepath.Ext(fileName))))
		contentType = strings.TrimSpace(strings.Split(contentType, ";")[0])
	}
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0]))
	}
	if !strings.HasPrefix(contentType, "image/") {
		return VideoAsset{}, ErrVideoInvalidMedia
	}
	return VideoAsset{URL: "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data)}, nil
}

func videoJSONString(fields map[string]json.RawMessage, name string) (string, error) {
	raw, exists := fields[name]
	if !exists {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", ErrVideoInvalidRequest
	}
	return value, nil
}

func parseVideoProviderOptions(raw json.RawMessage, request *CanonicalVideoRequest) error {
	options, ok := decodeUniqueVideoJSONObject(raw)
	if !ok {
		return ErrVideoInvalidRequest
	}
	request.ProviderOptions = make(map[string]json.RawMessage, len(options))
	for provider, value := range options {
		if !videoProviderNamePattern.MatchString(provider) {
			return ErrVideoInvalidRequest
		}
		schema, ok := videoProviderOptionSchema(provider)
		if !ok {
			return ErrVideoInvalidRequest
		}
		object, unique := decodeUniqueVideoJSONObject(value)
		if !unique {
			return ErrVideoInvalidRequest
		}
		for name, option := range object {
			kind, allowed := schema[name]
			if !allowed || !isValidVideoProviderOption(kind, option) {
				return ErrVideoInvalidRequest
			}
		}
		request.ProviderOptions[provider] = append(json.RawMessage(nil), value...)
	}
	return nil
}

func decodeUniqueVideoJSONObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, false
	}

	object := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		name, ok := token.(string)
		if !ok {
			return nil, false
		}
		if _, duplicate := object[name]; duplicate {
			return nil, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false
		}
		object[name] = append(json.RawMessage(nil), value...)
	}

	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, false
	}
	return object, true
}

type videoProviderOptionKind uint8

const (
	videoProviderOptionInteger videoProviderOptionKind = iota + 1
	videoProviderOptionBoolean
	videoProviderOptionString
)

func videoProviderOptionSchema(provider string) (map[string]videoProviderOptionKind, bool) {
	switch provider {
	case VideoProviderSeedance:
		return map[string]videoProviderOptionKind{
			"seed":              videoProviderOptionInteger,
			"watermark":         videoProviderOptionBoolean,
			"return_last_frame": videoProviderOptionBoolean,
			"service_tier":      videoProviderOptionString,
		}, true
	default:
		// New namespaces and keys must be added here only after their adapter
		// contract has an explicit provider-side allowlist.
		return nil, false
	}
}

func isValidVideoProviderOption(kind videoProviderOptionKind, raw json.RawMessage) bool {
	switch kind {
	case videoProviderOptionInteger:
		var value *int64
		return json.Unmarshal(raw, &value) == nil && value != nil
	case videoProviderOptionBoolean:
		var value *bool
		return json.Unmarshal(raw, &value) == nil && value != nil
	case videoProviderOptionString:
		var value *string
		return json.Unmarshal(raw, &value) == nil && value != nil && !containsVideoCredential(*value)
	default:
		return false
	}
}

func decodeVideoAssets(raw json.RawMessage) ([]VideoAsset, error) {
	var values []json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, ErrVideoInvalidMedia
	}
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, ErrVideoInvalidMedia
		}
	} else {
		values = []json.RawMessage{raw}
	}
	assets := make([]VideoAsset, 0, len(values))
	for _, value := range values {
		assetURL, err := decodeVideoAssetURL(value)
		if err != nil {
			return nil, err
		}
		if assetURL != "" {
			assets = append(assets, VideoAsset{URL: assetURL})
		}
	}
	return assets, nil
}

func decodeVideoAssetURL(raw json.RawMessage) (string, error) {
	var direct string
	if err := json.Unmarshal(raw, &direct); err == nil {
		return strings.TrimSpace(direct), nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return "", ErrVideoInvalidMedia
	}
	for _, key := range []string{"url", "image_url"} {
		value, exists := object[key]
		if !exists {
			continue
		}
		if err := json.Unmarshal(value, &direct); err == nil {
			return strings.TrimSpace(direct), nil
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(value, &nested); err == nil {
			if nestedURL, exists := nested["url"]; exists && json.Unmarshal(nestedURL, &direct) == nil {
				return strings.TrimSpace(direct), nil
			}
		}
		return "", ErrVideoInvalidMedia
	}
	return "", ErrVideoInvalidMedia
}

func validateCanonicalVideoRequest(request *CanonicalVideoRequest) error {
	if request == nil || request.Model == "" {
		return ErrVideoInvalidRequest
	}
	switch request.Operation {
	case VideoOperationGeneration:
		if request.Prompt == "" && request.assetCount() == 0 {
			return ErrVideoInvalidRequest
		}
	case VideoOperationEdit, VideoOperationExtension:
		if len(request.ReferenceVideos) == 0 {
			return ErrVideoInvalidRequest
		}
	default:
		return ErrVideoInvalidRequest
	}
	if request.DurationSeconds < 0 {
		return ErrVideoInvalidRequest
	}
	if request.Resolution != "" && !videoResolutionPattern.MatchString(request.Resolution) {
		return ErrVideoInvalidRequest
	}
	if request.AspectRatio != "" && !videoAspectRatioPattern.MatchString(request.AspectRatio) {
		return ErrVideoInvalidRequest
	}
	if request.assetCount() > MaxVideoRequestAssets {
		return ErrVideoTooManyAssets
	}
	for index := range request.FirstFrame {
		normalized, err := validateVideoAssetURL(request.FirstFrame[index].URL, true)
		if err != nil {
			return err
		}
		request.FirstFrame[index].URL = normalized
	}
	for index := range request.LastFrame {
		normalized, err := validateVideoAssetURL(request.LastFrame[index].URL, true)
		if err != nil {
			return err
		}
		request.LastFrame[index].URL = normalized
	}
	for index := range request.ReferenceImages {
		normalized, err := validateVideoAssetURL(request.ReferenceImages[index].URL, true)
		if err != nil {
			return err
		}
		request.ReferenceImages[index].URL = normalized
	}
	for index := range request.ReferenceVideos {
		normalized, err := validateVideoAssetURL(request.ReferenceVideos[index].URL, false)
		if err != nil {
			return err
		}
		request.ReferenceVideos[index].URL = normalized
	}
	return nil
}

func (request *CanonicalVideoRequest) assetCount() int {
	if request == nil {
		return 0
	}
	return len(request.FirstFrame) + len(request.LastFrame) + len(request.ReferenceImages) + len(request.ReferenceVideos)
}

func validateVideoAssetURL(raw string, allowDataImage bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrVideoInvalidMedia
	}
	if strings.HasPrefix(strings.ToLower(raw), "data:") {
		if !allowDataImage || !isValidVideoDataImage(raw) {
			return "", ErrVideoInvalidMedia
		}
		return raw, nil
	}
	normalized, err := urlvalidator.ValidateHTTPSURL(raw, urlvalidator.ValidationOptions{AllowPrivate: false})
	if err != nil {
		return "", ErrVideoInvalidMedia
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.User != nil || parsed.Fragment != "" {
		return "", ErrVideoInvalidMedia
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.Contains(host, "%") {
		return "", ErrVideoInvalidMedia
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedVideoIP(ip) {
			return "", ErrVideoInvalidMedia
		}
	} else if isNumericVideoHostname(host) {
		return "", ErrVideoInvalidMedia
	}
	return normalized, nil
}

func isNumericVideoHostname(host string) bool {
	parts := strings.Split(host, ".")
	for _, part := range parts {
		if part == "" {
			return false
		}
		if strings.HasPrefix(part, "0x") {
			if len(part) == 2 {
				return false
			}
			for _, char := range part[2:] {
				if !strings.ContainsRune("0123456789abcdef", char) {
					return false
				}
			}
			continue
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return false
			}
		}
	}
	return true
}

func isBlockedVideoIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	address = address.Unmap()
	for _, prefix := range videoBlockedIPPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return !address.IsGlobalUnicast()
}

func isValidVideoDataImage(raw string) bool {
	header, payload, ok := strings.Cut(raw[len("data:"):], ",")
	if !ok || strings.TrimSpace(payload) == "" {
		return false
	}
	parts := strings.Split(header, ";")
	if len(parts) < 2 || !strings.EqualFold(strings.TrimSpace(parts[len(parts)-1]), "base64") {
		return false
	}
	for _, part := range parts[1 : len(parts)-1] {
		if strings.EqualFold(strings.TrimSpace(part), "base64") {
			return false
		}
	}
	mediaType, _, err := mime.ParseMediaType(strings.Join(parts[:len(parts)-1], ";"))
	if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		return false
	}
	if _, err := base64.StdEncoding.DecodeString(payload); err == nil {
		return true
	}
	_, err = base64.RawStdEncoding.DecodeString(payload)
	return err == nil
}

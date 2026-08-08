package service

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestMinimizedVideoPayloadAcceptsOnlyAllowlistedSnapshotFields(t *testing.T) {
	payload, err := NewMinimizedVideoPayload(map[string]any{
		"prompt":           "a calm ocean at dusk",
		"resolution":       "720p",
		"duration_seconds": 5,
		"audio_mode":       "without_audio",
		"input_image_ref":  "asset/image-123",
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
		"prompt":"a calm ocean at dusk",
		"resolution":"720p",
		"duration_seconds":5,
		"audio_mode":"without_audio",
		"input_image_ref":"asset/image-123"
	}`, string(payload.Bytes()))

	for name, value := range map[string]any{
		"x-api-key":     "secret",
		"Authorization": "Bearer secret",
		"headers":       map[string]any{"x-api-key": "secret"},
		"unknown":       "unreviewed provider field",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewMinimizedVideoPayload(map[string]any{name: value})
			require.ErrorIs(t, err, ErrVideoTaskUnsafePayload)
		})
	}
}

func TestMinimizedVideoPayloadAcceptsOnlyStrictKlingRouteMetadata(t *testing.T) {
	for _, kind := range []string{klingTaskKindTextToVideo, klingTaskKindImageToVideo, klingTaskKindVideoExtend} {
		payload, err := NewMinimizedVideoPayload(map[string]any{
			"provider_task_kind": kind,
			"video_id":           "video_example",
		})
		require.NoError(t, err)
		require.NotEmpty(t, payload.Bytes())
	}
	for _, value := range []map[string]any{
		{"provider_task_kind": "unknown"},
		{"video_id": "https://example.com/video.mp4"},
		{"video_id": ""},
	} {
		_, err := NewMinimizedVideoPayload(value)
		require.ErrorIs(t, err, ErrVideoTaskUnsafePayload)
	}
}

func TestMinimizedVideoPayloadRejectsEncodedUploadStrings(t *testing.T) {
	encodedUpload := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, 1024))
	_, err := NewMinimizedVideoPayload(map[string]any{"prompt": encodedUpload})
	require.ErrorIs(t, err, ErrVideoTaskUnsafePayload)
}

func TestMinimizedVideoPayloadRejectsCredentialsEmbeddedInAllowlistedValues(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
		"eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkphbmUgRG9lIn0." +
		"SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	for name, test := range map[string]struct {
		field string
		value string
	}{
		"authorization bearer":    {field: "prompt", value: "render this header: Authorization: Bearer top-secret-access-token"},
		"basic authorization":     {field: "prompt", value: "Authorization: Basic dXNlcjpwYXNz"},
		"bare bearer":             {field: "negative_prompt", value: "Bearer top-secret-access-token-0123456789"},
		"alphabetic auth bearer":  {field: "prompt", value: "Authorization: Bearer abcdefghijklmnopqrstuvwxyz"},
		"jwt":                     {field: "source_task_id", value: jwt},
		"x api key":               {field: "input_image_ref", value: "x-api-key: sk-live-0123456789abcdef"},
		"api key":                 {field: "upstream_status", value: "api_key=live-secret-key-0123456789"},
		"alphabetic api key":      {field: "upstream_status", value: "api_key=abcdefghijklmnopqrstuvwxyz"},
		"quoted access token":     {field: "error_code", value: `{"access_token":"ya29.secret-token-0123456789"}`},
		"generic token":           {field: "worker_id", value: "token=secret-token-0123456789"},
		"password":                {field: "prompt", value: "password=longsecretvalue"},
		"client secret":           {field: "negative_prompt", value: "client_secret=longsecretvalue"},
		"cookie":                  {field: "upstream_status", value: "cookie=sessionvalue"},
		"password prose word":     {field: "prompt", value: "password=authentication"},
		"client secret prose":     {field: "negative_prompt", value: "client_secret=configured"},
		"cookie prose word":       {field: "upstream_status", value: "cookie=supported"},
		"aws access key":          {field: "prompt", value: "aws_access_key=abcdefghijklmnop"},
		"api key component":       {field: "prompt", value: "api_key_header=abcdefghijklmnop"},
		"access key component":    {field: "negative_prompt", value: "awsaccesskey_id=abcdefghijklmnop"},
		"fused access key id":     {field: "prompt", value: "awsaccesskeyid=AKIAIOSFODNN7EXAMPLE"},
		"camel access key id":     {field: "prompt", value: "awsAccessKeyId=AKIAIOSFODNN7EXAMPLE"},
		"qualified fused key":     {field: "prompt", value: "aws.accesskeyid=AKIAIOSFODNN7EXAMPLE"},
		"normalized access key":   {field: "negative_prompt", value: "awsaccesskey=abcdefghijklmnop"},
		"secret key component":    {field: "upstream_status", value: "awssecretkey_version=abcdefghijklmnop"},
		"normalized secret key":   {field: "upstream_status", value: "awssecretkey=abcdefghijklmnop"},
		"private key":             {field: "prompt", value: "tls_private_key=abcdefghijklmnop"},
		"private key component":   {field: "negative_prompt", value: "tlsprivatekey_path=abcdefghijklmnop"},
		"normalized private key":  {field: "negative_prompt", value: "tlsprivatekey=abcdefghijklmnop"},
		"authorization header":    {field: "upstream_status", value: "authorization_header=abcdefghijklmnop"},
		"fused authorization":     {field: "prompt", value: "authorizationheader=Bearer abcdefghijklmnop"},
		"camel authorization":     {field: "prompt", value: "authorizationHeader=Bearer abcdefghijklmnop"},
		"qualified fused auth":    {field: "prompt", value: "authorizationheader_value=Bearer abcdefghijklmnop"},
		"authorization suffix":    {field: "prompt", value: "customauthorization=abcdefghijklmnop"},
		"secret after safe prose": {field: "prompt", value: "Authorization: Bearer authentication is required topsecrettoken123"},
		"known key prefix":        {field: "billing_status", value: "sk-proj-0123456789abcdefghijklmnopqrstuvwxyz"},
		"short segment jwt":       {field: "prompt", value: "eyJhbGciOiJub25lIn0.e30.c2ln"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewMinimizedVideoPayload(map[string]any{test.field: test.value})
			require.ErrorIs(t, err, ErrVideoTaskUnsafePayload)
		})
	}
}

func TestMinimizedVideoPayloadRejectsShortEncodedBinaryAndMediaValues(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 16)...)
	mp4 := append([]byte("\x00\x00\x00\x18ftypisom"), bytes.Repeat([]byte{0}, 12)...)
	randomBinary := bytes.Repeat([]byte{0x00, 0xff, 0x80, 0x01}, 8)
	textualSVG := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0"/></svg>`)
	for name, value := range map[string]string{
		"short png":             base64.StdEncoding.EncodeToString(png),
		"short mp4":             base64.RawStdEncoding.EncodeToString(mp4),
		"short raw binary":      base64.RawURLEncoding.EncodeToString(randomBinary),
		"tiny padded binary":    base64.StdEncoding.EncodeToString([]byte{0x00, 0x01, 0x02, 0xff, 0xfe, 0xfd, 0x00}),
		"tiny raw binary":       base64.RawStdEncoding.EncodeToString([]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05}),
		"alphabetic controls":   base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x01}, 18)),
		"single control byte":   "AUFBQUFBQUFBQUFBQUFBQUFB",
		"tiny url safe binary":  "_w",
		"snake case raw binary": "photorealistic_cinematic_style",
		"textual svg":           base64.StdEncoding.EncodeToString(textualSVG),
	} {
		t.Run(name, func(t *testing.T) {
			require.Less(t, len(value), 256)
			_, err := NewMinimizedVideoPayload(map[string]any{"input_image_ref": value})
			require.ErrorIs(t, err, ErrVideoTaskUnsafePayload)
		})
	}
}

func TestMinimizedVideoPayloadAllowsOrdinaryCredentialVocabularyAndText(t *testing.T) {
	for name, value := range map[string]string{
		"token noun":                 "a brass token resting on a weathered wooden table",
		"authorization word":         "the word Authorization painted on a storefront sign",
		"jwt and api key words":      "a JWT login icon beside an API key label, with no values shown",
		"bearer noun":                "a bearer walking through a quiet forest at sunrise",
		"bearer prose":               "Bearer authentication is required before rendering the sign",
		"bearer hyphen prose":        "Bearer authentication-method should be illustrated on the sign",
		"authorization prose":        "Authorization: required for access to the gallery",
		"authorization bearer prose": "Authorization: Bearer authentication is required",
		"token prose":                "token: optional in this board-game illustration",
		"short base64 alphabet":      "test",
		"underscored style":          "cinematic_style",
		"long underscored style":     "photorealistic_cinematic_style",
		"key name remains ordinary":  "private_keyframe=photorealistic",
		"camel keyframe is ordinary": "privateKeyframe=photorealistic",
		"single-word style":          "photorealisticcinematiclighting",
		"encoded plain text":         base64.StdEncoding.EncodeToString([]byte("ordinary textual reference")),
		"invalid jwt shape":          "eyJhbGciOiJub25lIn0.bnVsbA.c2ln",
	} {
		t.Run(name, func(t *testing.T) {
			payload, err := NewMinimizedVideoPayload(map[string]any{"prompt": value})
			require.NoError(t, err)
			require.NotEmpty(t, payload.Bytes())
		})
	}
}

func TestMinimizedVideoPayloadAllowsStructuredSafeReferences(t *testing.T) {
	payload, err := NewMinimizedVideoPayload(map[string]any{
		"source_task_id": "vid_0123456789abcdef0123456789abcdef",
		"worker_id":      "worker-0123456789abcdef",
	})
	require.NoError(t, err)
	require.NotEmpty(t, payload.Bytes())
}

func TestVideoTaskErrorIsBoundedAndRedactsCredentialBearingResponses(t *testing.T) {
	taskError := NewVideoTaskError(
		"UPSTREAM_AUTH_FAILED",
		`{"message":"denied","x-api-key":"top-secret-key","authorization":"Bearer top-secret-token"}`,
		true,
	)
	require.Equal(t, "UPSTREAM_AUTH_FAILED", taskError.Code())
	require.True(t, taskError.Retryable())
	require.NotContains(t, taskError.Message(), "top-secret")
	require.NotContains(t, strings.ToLower(taskError.Message()), "x-api-key")
	require.NotContains(t, strings.ToLower(taskError.Message()), "authorization")

	longError := NewVideoTaskError("UPSTREAM_ERROR", strings.Repeat("界", MaxVideoTaskErrorMessageBytes), false)
	require.LessOrEqual(t, len(longError.Message()), MaxVideoTaskErrorMessageBytes)
	require.True(t, utf8.ValidString(longError.Message()))
}

func TestIsVideoRequestIDRequiresCanonicalPublicFormat(t *testing.T) {
	require.True(t, IsVideoRequestID("vid_0123456789abcdef0123456789abcdef"))
	for _, requestID := range []string{
		"",
		"vid_a",
		"vid_0123456789ABCDEF0123456789ABCDEF",
		"img_0123456789abcdef0123456789abcdef",
		"vid_0123456789abcdef0123456789abcdef0",
	} {
		require.False(t, IsVideoRequestID(requestID), requestID)
	}
}

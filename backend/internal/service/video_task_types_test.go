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

func TestMinimizedVideoPayloadRejectsEncodedUploadStrings(t *testing.T) {
	encodedUpload := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, 1024))
	_, err := NewMinimizedVideoPayload(map[string]any{"prompt": encodedUpload})
	require.ErrorIs(t, err, ErrVideoTaskUnsafePayload)
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

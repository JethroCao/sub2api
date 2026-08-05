package service

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeVideoRequestPreservesGrokAliases(t *testing.T) {
	body := []byte(`{"model":"grok-imagine-video-1.5","prompt":"animate","image":{"image_url":"https://example.com/a.png"},"duration":10}`)
	req, err := NormalizeVideoRequest(VideoOperationGeneration, "application/json", body)
	require.NoError(t, err)
	require.Equal(t, "grok-imagine-video-1.5", req.Model)
	require.Equal(t, []VideoAsset{{URL: "https://example.com/a.png"}}, req.FirstFrame)
	require.Equal(t, 10, req.DurationSeconds)
}

func TestNormalizeVideoRequestPreservesCurrentJSONImageForms(t *testing.T) {
	body := []byte(`{
		"model":"grok-imagine-video-1.5",
		"prompt":"animate",
		"image":"data:image/png;base64,QQ==",
		"images":[{"url":"https://example.com/reference-a.png"},{"image_url":"https://example.com/reference-b.png"}],
		"reference_images":"https://example.com/reference-c.png",
		"reference_videos":[{"url":"https://example.com/reference.mp4"}],
		"resolution":"1080P",
		"aspect_ratio":"16:9",
		"audio":true,
		"provider_options":{"seedance":{"seed":42}}
	}`)

	req, err := NormalizeVideoRequest(VideoOperationGeneration, "application/json; charset=utf-8", body)
	require.NoError(t, err)
	require.Equal(t, []VideoAsset{{URL: "data:image/png;base64,QQ=="}}, req.FirstFrame)
	require.Equal(t, []VideoAsset{
		{URL: "https://example.com/reference-a.png"},
		{URL: "https://example.com/reference-b.png"},
		{URL: "https://example.com/reference-c.png"},
	}, req.ReferenceImages)
	require.Equal(t, []VideoAsset{{URL: "https://example.com/reference.mp4"}}, req.ReferenceVideos)
	require.Equal(t, "1080p", req.Resolution)
	require.Equal(t, "16:9", req.AspectRatio)
	require.NotNil(t, req.Audio)
	require.True(t, *req.Audio)
	require.JSONEq(t, `{"seed":42}`, string(req.ProviderOptions[VideoProviderSeedance]))
}

func TestNormalizeVideoRequestPreservesGrokMultipartAliases(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "grok-imagine-video-1.5"))
	require.NoError(t, writer.WriteField("prompt", " animate "))
	require.NoError(t, writer.WriteField("image_url", "https://example.com/first.png"))
	require.NoError(t, writer.WriteField("images[]", "https://example.com/reference.png"))
	require.NoError(t, writer.WriteField("duration", "10"))
	require.NoError(t, writer.WriteField("resolution", "720P"))
	require.NoError(t, writer.WriteField("aspect_ratio", "9:16"))
	require.NoError(t, writer.WriteField("audio", "true"))
	part, err := writer.CreateFormFile("image[]", "second.png")
	require.NoError(t, err)
	_, err = part.Write([]byte("png-bytes"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req, err := NormalizeVideoRequest(VideoOperationGeneration, writer.FormDataContentType(), body.Bytes())
	require.NoError(t, err)
	require.Equal(t, "animate", req.Prompt)
	require.Equal(t, []VideoAsset{{URL: "https://example.com/first.png"}}, req.FirstFrame)
	require.Len(t, req.ReferenceImages, 2)
	require.Equal(t, "https://example.com/reference.png", req.ReferenceImages[0].URL)
	require.True(t, strings.HasPrefix(req.ReferenceImages[1].URL, "data:image/png;base64,"))
	require.Equal(t, 10, req.DurationSeconds)
	require.Equal(t, "720p", req.Resolution)
	require.NotNil(t, req.Audio)
	require.True(t, *req.Audio)
}

func TestNormalizeVideoRequestMapsMultipartFrameFilesAndIgnoresUnknownUploads(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "video-model"))
	first, err := writer.CreateFormFile("first_frame", "first.png")
	require.NoError(t, err)
	_, err = first.Write([]byte("first"))
	require.NoError(t, err)
	last, err := writer.CreateFormFile("last_frame", "last.webp")
	require.NoError(t, err)
	_, err = last.Write([]byte("last"))
	require.NoError(t, err)
	unknown, err := writer.CreateFormFile("unrelated", "note.txt")
	require.NoError(t, err)
	_, err = unknown.Write([]byte("ignored"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req, err := NormalizeVideoRequest(VideoOperationGeneration, writer.FormDataContentType(), body.Bytes())
	require.NoError(t, err)
	require.Len(t, req.FirstFrame, 1)
	require.True(t, strings.HasPrefix(req.FirstFrame[0].URL, "data:image/png;base64,"))
	require.Len(t, req.LastFrame, 1)
	require.True(t, strings.HasPrefix(req.LastFrame[0].URL, "data:image/webp;base64,"))
	require.Empty(t, req.ReferenceImages)
}

func TestNormalizeVideoRequestRejectsInvalidScalarValues(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "zero duration", body: `{"model":"m","prompt":"p","duration":0}`},
		{name: "negative duration", body: `{"model":"m","prompt":"p","duration":-1}`},
		{name: "fractional duration", body: `{"model":"m","prompt":"p","duration":1.5}`},
		{name: "resolution syntax", body: `{"model":"m","prompt":"p","resolution":"full hd"}`},
		{name: "aspect ratio syntax", body: `{"model":"m","prompt":"p","aspect_ratio":"wide"}`},
		{name: "audio type", body: `{"model":"m","prompt":"p","audio":"true"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeVideoRequest(VideoOperationGeneration, "application/json", []byte(tt.body))
			require.ErrorIs(t, err, ErrVideoInvalidRequest)
		})
	}
}

func TestNormalizeVideoRequestRejectsUnsafeRemoteAssets(t *testing.T) {
	urls := []string{
		"http://example.com/a.png",
		"https://localhost/a.png",
		"https://localhost./a.png",
		"https://127.0.0.1/a.png",
		"https://127.1/a.png",
		"https://2130706433/a.png",
		"https://0x7f000001/a.png",
		"https://10.0.0.1/a.png",
		"https://169.254.169.254/latest/meta-data",
		"https://192.0.2.1/a.png",
		"https://[::1]/a.png",
		"https://[fc00::1]/a.png",
		"https://[fe80::1%25en0]/a.png",
		"https://user:secret@example.com/a.png",
	}
	for _, rawURL := range urls {
		t.Run(rawURL, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"model":  "m",
				"prompt": "p",
				"image":  map[string]string{"url": rawURL},
			})
			require.NoError(t, err)
			_, err = NormalizeVideoRequest(VideoOperationGeneration, "application/json", body)
			require.ErrorIs(t, err, ErrVideoInvalidMedia)
		})
	}
}

func TestNormalizeVideoRequestAcceptsPublicIPAsset(t *testing.T) {
	req, err := NormalizeVideoRequest(VideoOperationGeneration, "application/json", []byte(
		`{"model":"m","image":"https://8.8.8.8/a.png"}`,
	))
	require.NoError(t, err)
	require.Equal(t, "https://8.8.8.8/a.png", req.FirstFrame[0].URL)
}

func TestNormalizeVideoRequestAcceptsExistingDataImageFormsOnlyForImages(t *testing.T) {
	for _, dataURL := range []string{
		"data:image/png;base64,QQ==",
		"data:image/jpeg;base64,/9g=",
		"data:image/webp;base64,UklGRg==",
		"data:image/png;base64,abc",
	} {
		body, err := json.Marshal(map[string]any{"model": "m", "image": dataURL})
		require.NoError(t, err)
		req, err := NormalizeVideoRequest(VideoOperationGeneration, "application/json", body)
		require.NoError(t, err)
		require.Equal(t, dataURL, req.FirstFrame[0].URL)
	}

	for _, dataURL := range []string{
		"data:text/plain;base64,QQ==",
		"data:image/png,raw",
		"data:image/png;base64,%%%",
	} {
		body, err := json.Marshal(map[string]any{"model": "m", "image": dataURL})
		require.NoError(t, err)
		_, err = NormalizeVideoRequest(VideoOperationGeneration, "application/json", body)
		require.ErrorIs(t, err, ErrVideoInvalidMedia)
	}

	body := []byte(`{"model":"m","prompt":"edit","video":"data:image/png;base64,QQ=="}`)
	_, err := NormalizeVideoRequest(VideoOperationEdit, "application/json", body)
	require.ErrorIs(t, err, ErrVideoInvalidMedia)
}

func TestNormalizeVideoRequestEnforcesOperationInputs(t *testing.T) {
	tests := []struct {
		name      string
		operation VideoOperation
		body      string
		wantErr   bool
	}{
		{name: "generation requires model", operation: VideoOperationGeneration, body: `{"prompt":"p"}`, wantErr: true},
		{name: "generation requires prompt or asset", operation: VideoOperationGeneration, body: `{"model":"m"}`, wantErr: true},
		{name: "generation accepts reference", operation: VideoOperationGeneration, body: `{"model":"m","reference_images":["https://example.com/a.png"]}`},
		{name: "edit requires source video", operation: VideoOperationEdit, body: `{"model":"m","prompt":"edit"}`, wantErr: true},
		{name: "edit accepts video alias", operation: VideoOperationEdit, body: `{"model":"m","prompt":"edit","video":{"url":"https://example.com/source.mp4"}}`},
		{name: "extension requires source video", operation: VideoOperationExtension, body: `{"model":"m","prompt":"extend"}`, wantErr: true},
		{name: "unknown operation", operation: VideoOperation("other"), body: `{"model":"m","prompt":"p"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := NormalizeVideoRequest(tt.operation, "application/json", []byte(tt.body))
			if tt.wantErr {
				require.ErrorIs(t, err, ErrVideoInvalidRequest)
				return
			}
			require.NoError(t, err)
			if tt.operation == VideoOperationEdit {
				require.Equal(t, []VideoAsset{{URL: "https://example.com/source.mp4"}}, req.ReferenceVideos)
			}
		})
	}
}

func TestNormalizeVideoRequestEnforcesExplicitLimits(t *testing.T) {
	large := bytes.Repeat([]byte(" "), MaxVideoRequestBodyBytes+1)
	_, err := NormalizeVideoRequest(VideoOperationGeneration, "application/json", large)
	require.ErrorIs(t, err, ErrVideoRequestBodyTooLarge)

	assets := make([]map[string]string, MaxVideoRequestAssets+1)
	for index := range assets {
		assets[index] = map[string]string{"url": "https://example.com/a.png"}
	}
	body, err := json.Marshal(map[string]any{
		"model":            "m",
		"reference_images": assets,
	})
	require.NoError(t, err)
	_, err = NormalizeVideoRequest(VideoOperationGeneration, "application/json", body)
	require.ErrorIs(t, err, ErrVideoTooManyAssets)
}

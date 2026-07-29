package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIRequestBodyMayContainImageInput(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "plain text", body: `{"input":"hello"}`, want: false},
		{name: "responses input image", body: `{"input":[{"role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"data:image/png;base64,abc"}]}]}`, want: true},
		{name: "chat image url", body: `{"messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"https://example.test/a.png"}}]}]}`, want: true},
		{name: "anthropic image", body: `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]}]}`, want: true},
		{name: "historical image", body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/a.png"}}]},{"role":"user","content":"continue"}]}`, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, OpenAIRequestBodyMayContainImageInput([]byte(tt.body)))
		})
	}
}

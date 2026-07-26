package service

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// shouldStripOpenAIResponsesLiteForMappedModel keeps the decision local to one
// account attempt. Callers must not mutate the inbound request header because
// the same request can fail over to another account with different settings.
func shouldStripOpenAIResponsesLiteForMappedModel(account *Account, originalModel, upstreamModel string) bool {
	if account == nil || !account.ShouldStripOpenAIResponsesLiteOnModelMapping() {
		return false
	}
	originalModel = strings.TrimSpace(originalModel)
	upstreamModel = strings.TrimSpace(upstreamModel)
	return originalModel != "" && upstreamModel != "" && originalModel != upstreamModel
}

func stripOpenAIResponsesLiteHeader(headers http.Header) {
	deleteHeaderAllForms(headers, responsesLiteHeaderKey)
}

// stripOpenAIResponsesLiteWebSocketMetadata removes the WS representation of
// the Responses Lite request header without disturbing other client metadata.
func stripOpenAIResponsesLiteWebSocketMetadata(body []byte) ([]byte, bool, error) {
	path := "client_metadata." + responsesLiteWSMetadataKey
	if !gjson.GetBytes(body, path).Exists() {
		return body, false, nil
	}
	updated, err := sjson.DeleteBytes(body, path)
	if err != nil {
		return body, false, err
	}
	return updated, true, nil
}

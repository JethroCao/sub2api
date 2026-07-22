package service

import (
	"errors"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func appendOpenAIAccountInstructions(account *Account, body []byte) ([]byte, bool, error) {
	suffix := account.GetOpenAICustomInstructions()
	if suffix == "" {
		return body, false, nil
	}

	current := gjson.GetBytes(body, "instructions")
	if current.Exists() && current.Type != gjson.String {
		return body, false, errors.New("OpenAI instructions must be a string")
	}

	existingRaw := current.String()
	existingTrimmed := strings.TrimSpace(existingRaw)
	if existingTrimmed == suffix || strings.HasSuffix(existingTrimmed, "\n\n"+suffix) {
		return body, false, nil
	}

	combined := suffix
	if existingTrimmed != "" {
		combined = existingRaw + "\n\n" + suffix
	}
	next, err := sjson.SetBytes(body, "instructions", combined)
	return next, err == nil, err
}

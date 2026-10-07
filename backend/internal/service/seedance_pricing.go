package service

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

var (
	ErrSeedancePricingConfiguration = errors.New("Seedance official pricing configuration is invalid")
	ErrSeedancePricingRequest       = errors.New("Seedance request cannot be priced")
	seedancePricedModelPattern      = regexp.MustCompile(`^(?:doubao-)?seedance-(2-[05])(?:-\d{6})?$`)
	seedanceTextResolutionPattern   = regexp.MustCompile(`(?:^|\s)--(?:rs|resolution)\s+(480p|720p|1080p|4k)(?:\s|$)`)
)

// SeedanceBillingSnapshot freezes the official CNY tariffs and FX rate at
// create time. The successful status response may supply the actual resolution.
// These are output-token tariffs: Ark's completion_tokens already includes
// input-video tokens and the minimum billable usage. Do not estimate them again.
type SeedanceBillingSnapshot struct {
	ModelFamily               string             `json:"model_family"`
	Resolution                string             `json:"resolution"`
	HasInputVideo             bool               `json:"has_input_video"`
	Draft                     bool               `json:"draft"`
	USDToCNYRate              float64            `json:"usd_to_cny_rate"`
	OutputPricesCNYPerMillion map[string]float64 `json:"output_prices_cny_per_million"`
}

func (p *SeedanceBillingSnapshot) WithResolution(resolution string) (*SeedanceBillingSnapshot, error) {
	if p == nil {
		return nil, fmt.Errorf("%w: missing task pricing snapshot", ErrSeedancePricingConfiguration)
	}
	copy := *p
	if resolution = strings.ToLower(strings.TrimSpace(resolution)); resolution != "" {
		copy.Resolution = resolution
	}
	price := copy.OutputPricesCNYPerMillion[copy.Resolution]
	if !seedancePositiveFinite(price) || !seedancePositiveFinite(copy.USDToCNYRate) {
		return nil, fmt.Errorf("%w: missing tariff or exchange rate for resolution %q", ErrSeedancePricingConfiguration, copy.Resolution)
	}
	return &copy, nil
}

func seedancePositiveFinite(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func seedanceModelFamily(model, upstreamModel string) string {
	// An explicit upstream model is authoritative. An opaque ep-* endpoint
	// instead uses the public alias, which the administrator maps to that model.
	if strings.HasPrefix(strings.ToLower(upstreamModel), "ep-") || upstreamModel == "" {
		upstreamModel = model
	}
	match := seedancePricedModelPattern.FindStringSubmatch(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(upstreamModel)), ".", "-"))
	if len(match) != 2 {
		return ""
	}
	return strings.ReplaceAll(match[1], "-", ".")
}

// SeedanceDraftTaskID reads Ark's native content[].draft_task.id field.
func SeedanceDraftTaskID(body []byte) (string, error) {
	id := ""
	for _, item := range gjson.GetBytes(body, "content").Array() {
		if item.Get("type").String() != "draft_task" {
			continue
		}
		value := item.Get("draft_task.id")
		if id != "" || value.Type != gjson.String || strings.TrimSpace(value.String()) == "" {
			return "", fmt.Errorf("%w: exactly one valid draft_task.id is required", ErrSeedancePricingRequest)
		}
		id = strings.TrimSpace(value.String())
		if err := validateUpstreamPathSegment("Seedance draft task ID", id); err != nil {
			return "", fmt.Errorf("%w: invalid draft task ID", ErrSeedancePricingRequest)
		}
	}
	return id, nil
}

func prepareSeedanceOfficialBilling(account *Account, body []byte, parent *SeedanceBillingSnapshot) (*SeedanceBillingSnapshot, error) {
	if account == nil || account.GetCredential("seedance_official_pricing") != "true" {
		return nil, nil // Preserve upstream's flat/custom token pricing for other accounts.
	}
	rate, err := strconv.ParseFloat(account.GetCredential("seedance_usd_to_cny_rate"), 64)
	if err != nil || !seedancePositiveFinite(rate) {
		return nil, fmt.Errorf("%w: seedance_usd_to_cny_rate must be positive and finite", ErrSeedancePricingConfiguration)
	}
	model := gjson.GetBytes(body, "model").String()
	family := seedanceModelFamily(model, account.GetMappedModel(model))
	if family == "" {
		return nil, fmt.Errorf("%w: official tariffs are configured for Seedance 2.0 and 2.5 only", ErrSeedancePricingRequest)
	}
	if tier := gjson.GetBytes(body, "service_tier").String(); tier != "" && tier != "default" {
		return nil, fmt.Errorf("%w: this model has no non-default service-tier tariff", ErrSeedancePricingRequest)
	}
	resolution := gjson.GetBytes(body, "resolution")
	if resolution.Exists() && resolution.Type != gjson.String {
		return nil, fmt.Errorf("%w: resolution must be a string", ErrSeedancePricingRequest)
	}
	p := &SeedanceBillingSnapshot{ModelFamily: family, Resolution: strings.ToLower(strings.TrimSpace(resolution.String())), USDToCNYRate: rate, Draft: gjson.GetBytes(body, "draft").Bool()}
	for _, item := range gjson.GetBytes(body, "content").Array() {
		if item.Get("type").String() == "video_url" {
			p.HasInputVideo = true
		}
		if p.Resolution == "" && item.Get("type").String() == "text" {
			matches := seedanceTextResolutionPattern.FindAllStringSubmatch(item.Get("text").String(), -1)
			if len(matches) > 0 {
				p.Resolution = matches[len(matches)-1][1]
			}
		}
	}
	draftID, err := SeedanceDraftTaskID(body)
	if err != nil {
		return nil, err
	}
	if draftID != "" {
		if family != "2.5" || p.Draft || parent == nil || !parent.Draft || parent.ModelFamily != "2.5" {
			return nil, fmt.Errorf("%w: formal generation requires the owned Seedance 2.5 draft pricing snapshot", ErrSeedancePricingRequest)
		}
		// Official step 2 inherits whether step 1 used video input. The draft
		// artifact itself is not an input video for pricing purposes.
		p.HasInputVideo = parent.HasInputVideo
		if p.Resolution != "" && p.Resolution != "1080p" {
			return nil, fmt.Errorf("%w: formal draft generation requires 1080p", ErrSeedancePricingRequest)
		}
		p.Resolution = "1080p"
	}
	if p.Draft {
		if family != "2.5" || (p.Resolution != "" && p.Resolution != "480p") {
			return nil, fmt.Errorf("%w: draft mode requires Seedance 2.5 at 480p", ErrSeedancePricingRequest)
		}
		p.Resolution = "480p"
	}
	if p.Resolution == "" {
		p.Resolution = "720p"
	}
	// Published online list prices (CNY / 1M completion tokens), checked
	// 2026-10-07: https://docs.volcengine.com/docs/ark/model-pricing
	if family == "2.0" {
		p.OutputPricesCNYPerMillion = map[string]float64{"480p": 46, "720p": 46, "1080p": 51, "4k": 26}
		if p.HasInputVideo {
			p.OutputPricesCNYPerMillion = map[string]float64{"480p": 28, "720p": 28, "1080p": 31, "4k": 16}
		}
	} else {
		p.OutputPricesCNYPerMillion = map[string]float64{"480p": 70, "720p": 70, "1080p": 77}
		if p.HasInputVideo {
			p.OutputPricesCNYPerMillion = map[string]float64{"480p": 42, "720p": 42, "1080p": 46}
		}
	}
	if _, ok := p.OutputPricesCNYPerMillion[p.Resolution]; !ok {
		return nil, fmt.Errorf("%w: resolution %q is not supported by Seedance %s", ErrSeedancePricingRequest, p.Resolution, family)
	}
	return p, nil
}

func calculateSeedanceOfficialCost(p *SeedanceBillingSnapshot, completionTokens int, multiplier float64) (*CostBreakdown, error) {
	p, err := p.WithResolution("")
	if err != nil {
		return nil, err
	}
	if completionTokens < 0 || multiplier < 0 || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) {
		return nil, fmt.Errorf("%w: invalid token usage or multiplier", ErrSeedancePricingConfiguration)
	}
	output := float64(completionTokens) * p.OutputPricesCNYPerMillion[p.Resolution] / 1_000_000 / p.USDToCNYRate
	return &CostBreakdown{OutputCost: output, TotalCost: output, ActualCost: output * multiplier, BillingMode: string(BillingModeToken)}, nil
}

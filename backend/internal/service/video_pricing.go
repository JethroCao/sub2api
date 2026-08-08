package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
)

const (
	videoPricingResolutionAny = "*"
	videoPricingAudioAny      = "any"
	videoPricingAudioWith     = "with_audio"
	videoPricingAudioWithout  = "without_audio"
	videoPricingPerRequest    = "per_request"
	videoPricingPerSecond     = "per_output_second"
)

var (
	// ErrVideoPricingUnavailable means no enabled rule can price a video request.
	// Video submission must fail closed instead of using a token-price fallback.
	ErrVideoPricingUnavailable = errors.New("video pricing is unavailable")
	ErrVideoPricingInvalid     = errors.New("video pricing rule or quote is invalid")
)

// VideoPricingRule is the durable rule shape used to construct a price snapshot.
// It intentionally mirrors only the customer-pricing data needed by the resolver.
type VideoPricingRule struct {
	ID               int64
	GroupID          int64
	ExternalModel    string
	Operation        string
	Resolution       string
	AudioMode        string
	Unit             string
	UnitPrice        float64
	UpstreamUnitCost *float64
	Enabled          bool
	Legacy           bool
}

// VideoPricingQuery identifies the video request to price. DurationSeconds is
// consumed only by per-output-second rules.
type VideoPricingQuery struct {
	GroupID         int64
	ExternalModel   string
	Operation       string
	Resolution      string
	Audio           bool
	DurationSeconds float64
}

// VideoPriceQuote is an immutable-at-resolution snapshot: it contains copied
// rule data and derived totals, so later rule edits cannot change an accepted
// request's pricing.
type VideoPriceQuote struct {
	RuleID           int64
	GroupID          int64
	ExternalModel    string
	Operation        string
	Resolution       string
	AudioMode        string
	Unit             string
	UnitPrice        float64
	UpstreamUnitCost *float64
	Units            float64
	HoldAmount       float64
}

type VideoPricingRepository interface {
	ListMatching(context.Context, VideoPricingQuery) ([]VideoPricingRule, error)
}

type VideoPricingService struct {
	repo VideoPricingRepository
}

func NewVideoPricingService(repo VideoPricingRepository) *VideoPricingService {
	return &VideoPricingService{repo: repo}
}

// Quote resolves a single enabled rule. An exact resolution takes precedence
// over a wildcard resolution; within that, an exact audio mode takes precedence
// over the audio wildcard. The database uniqueness constraint prevents duplicate
// rules at the same specificity dimensions.
func (s *VideoPricingService) Quote(ctx context.Context, query VideoPricingQuery) (VideoPriceQuote, error) {
	if s == nil || s.repo == nil || !validVideoPricingQuery(query) {
		return VideoPriceQuote{}, ErrVideoPricingUnavailable
	}

	rules, err := s.repo.ListMatching(ctx, query)
	if err != nil {
		return VideoPriceQuote{}, err
	}

	var selected *VideoPricingRule
	selectedScore := -1
	for i := range rules {
		rule := &rules[i]
		if !videoPricingRuleMatches(*rule, query) {
			continue
		}
		score := videoPricingRuleSpecificity(*rule, query)
		if score > selectedScore {
			selected = rule
			selectedScore = score
		}
	}
	if selected == nil {
		return VideoPriceQuote{}, ErrVideoPricingUnavailable
	}
	return quoteVideoPricingRule(*selected, query)
}

func validVideoPricingQuery(query VideoPricingQuery) bool {
	return query.GroupID > 0 && strings.TrimSpace(query.ExternalModel) != "" && strings.TrimSpace(query.Operation) != ""
}

func videoPricingRuleMatches(rule VideoPricingRule, query VideoPricingQuery) bool {
	// ListMatching owns group/model/operation filtering so that callers can use
	// a compact resolver fake without reproducing repository query logic here.
	if rule.Resolution != videoPricingResolutionAny && rule.Resolution != query.Resolution {
		return false
	}
	return rule.AudioMode == videoPricingAudioAny || rule.AudioMode == videoPricingAudioMode(query.Audio)
}

func videoPricingRuleSpecificity(rule VideoPricingRule, query VideoPricingQuery) int {
	score := 0
	if rule.Resolution == query.Resolution {
		score += 2
	}
	if rule.AudioMode == videoPricingAudioMode(query.Audio) {
		score++
	}
	return score
}

func videoPricingAudioMode(audio bool) string {
	if audio {
		return videoPricingAudioWith
	}
	return videoPricingAudioWithout
}

func quoteVideoPricingRule(rule VideoPricingRule, query VideoPricingQuery) (VideoPriceQuote, error) {
	if (rule.ID <= 0 && !rule.Legacy) || rule.UnitPrice < 0 || math.IsNaN(rule.UnitPrice) || math.IsInf(rule.UnitPrice, 0) {
		return VideoPriceQuote{}, ErrVideoPricingInvalid
	}
	if rule.UpstreamUnitCost != nil && (*rule.UpstreamUnitCost < 0 || math.IsNaN(*rule.UpstreamUnitCost) || math.IsInf(*rule.UpstreamUnitCost, 0)) {
		return VideoPriceQuote{}, ErrVideoPricingInvalid
	}

	units := 1.0
	switch rule.Unit {
	case videoPricingPerRequest:
	case videoPricingPerSecond:
		if query.DurationSeconds <= 0 || math.IsNaN(query.DurationSeconds) || math.IsInf(query.DurationSeconds, 0) {
			return VideoPriceQuote{}, ErrVideoPricingInvalid
		}
		units = query.DurationSeconds
	default:
		return VideoPriceQuote{}, ErrVideoPricingInvalid
	}
	holdAmount := rule.UnitPrice * units
	if math.IsNaN(holdAmount) || math.IsInf(holdAmount, 0) {
		return VideoPriceQuote{}, fmt.Errorf("video hold amount overflows: %w", ErrVideoPricingInvalid)
	}

	quote := VideoPriceQuote{
		RuleID:        rule.ID,
		GroupID:       query.GroupID,
		ExternalModel: query.ExternalModel,
		Operation:     query.Operation,
		Resolution:    rule.Resolution,
		AudioMode:     rule.AudioMode,
		Unit:          rule.Unit,
		UnitPrice:     rule.UnitPrice,
		Units:         units,
		HoldAmount:    holdAmount,
	}
	if rule.UpstreamUnitCost != nil {
		upstreamUnitCost := *rule.UpstreamUnitCost
		quote.UpstreamUnitCost = &upstreamUnitCost
	}
	return quote, nil
}

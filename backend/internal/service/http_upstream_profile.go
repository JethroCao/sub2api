package service

import (
	"context"
	"net"
)

// HTTPUpstreamProfile marks HTTP upstream requests that need provider-specific
// transport policy.
type HTTPUpstreamProfile string

const (
	HTTPUpstreamProfileDefault HTTPUpstreamProfile = ""
	HTTPUpstreamProfileOpenAI  HTTPUpstreamProfile = "openai"
)

type httpUpstreamProfileContextKey struct{}
type httpUpstreamDisableRedirectsContextKey struct{}
type httpUpstreamRequireResolvedIPValidationContextKey struct{}
type httpUpstreamValidatedDialContextKey struct{}

// HTTPUpstreamDialContext dials a validated destination. The shared upstream
// installs it on a request-specific direct transport when present.
type HTTPUpstreamDialContext func(context.Context, string, string) (net.Conn, error)

// WithHTTPUpstreamProfile injects an upstream transport profile into ctx.
func WithHTTPUpstreamProfile(ctx context.Context, profile HTTPUpstreamProfile) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if profile == HTTPUpstreamProfileDefault {
		return ctx
	}
	return context.WithValue(ctx, httpUpstreamProfileContextKey{}, profile)
}

// HTTPUpstreamProfileFromContext resolves the upstream transport profile from ctx.
func HTTPUpstreamProfileFromContext(ctx context.Context) HTTPUpstreamProfile {
	if ctx == nil {
		return HTTPUpstreamProfileDefault
	}
	profile, ok := ctx.Value(httpUpstreamProfileContextKey{}).(HTTPUpstreamProfile)
	if !ok {
		return HTTPUpstreamProfileDefault
	}
	switch profile {
	case HTTPUpstreamProfileOpenAI:
		return profile
	default:
		return HTTPUpstreamProfileDefault
	}
}

// WithHTTPUpstreamRedirectsDisabled prevents credential-bearing probes from
// following redirects through the shared upstream client.
func WithHTTPUpstreamRedirectsDisabled(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, httpUpstreamDisableRedirectsContextKey{}, true)
}

func HTTPUpstreamRedirectsDisabled(ctx context.Context) bool {
	return ctx != nil && ctx.Value(httpUpstreamDisableRedirectsContextKey{}) == true
}

// WithHTTPUpstreamResolvedIPValidation requires the shared HTTP upstream to
// validate the destination immediately before it issues the request. Adapters
// use it for credential-bearing provider URLs even when the global allowlist
// is optional, closing DNS-rebinding windows at the shared transport boundary.
func WithHTTPUpstreamResolvedIPValidation(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, httpUpstreamRequireResolvedIPValidationContextKey{}, true)
}

func HTTPUpstreamResolvedIPValidationRequired(ctx context.Context) bool {
	return ctx != nil && ctx.Value(httpUpstreamRequireResolvedIPValidationContextKey{}) == true
}

func WithHTTPUpstreamValidatedDialContext(ctx context.Context, dial HTTPUpstreamDialContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if dial == nil {
		return ctx
	}
	return context.WithValue(ctx, httpUpstreamValidatedDialContextKey{}, dial)
}

func HTTPUpstreamValidatedDialContextFromContext(ctx context.Context) (HTTPUpstreamDialContext, bool) {
	if ctx == nil {
		return nil, false
	}
	dial, ok := ctx.Value(httpUpstreamValidatedDialContextKey{}).(HTTPUpstreamDialContext)
	return dial, ok && dial != nil
}

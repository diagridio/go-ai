// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package identity

import (
	"context"
	"net/http"
)

// contextKey keeps the identity values out of reach of other packages, which
// share the one context namespace.
type contextKey int

const (
	userKey contextKey = iota
	tokenKey
)

// ContextWithUser returns a copy of ctx carrying user. The middleware calls it
// on every verified request; call it directly only when standing in for the
// middleware, as a test does.
func ContextWithUser(ctx context.Context, user *VerifiedUser) context.Context {
	return context.WithValue(ctx, userKey, user)
}

// UserFromContext returns the verified caller on ctx. The second result is
// false outside a verified request, which is what an unauthenticated route
// admitted by OAuthConfig.RequireAuth being false looks like.
func UserFromContext(ctx context.Context) (*VerifiedUser, bool) {
	user, ok := ctx.Value(userKey).(*VerifiedUser)
	return user, ok && user != nil
}

// ContextWithToken returns a copy of ctx carrying the raw inbound bearer token,
// which the [Transport] behind [NewHTTPClient] reads back at send time.
func ContextWithToken(ctx context.Context, rawToken string) context.Context {
	return context.WithValue(ctx, tokenKey, rawToken)
}

// TokenFromContext returns the raw inbound bearer token on ctx, without the
// Bearer prefix. The second result is false outside a verified request.
func TokenFromContext(ctx context.Context) (string, bool) {
	token, ok := ctx.Value(tokenKey).(string)
	return token, ok && token != ""
}

// outboundIdentityHeaders returns the identity headers for an outbound MCP or
// sub-agent call made while serving a verified caller, and an empty set when
// ctx carries no inbound user, so the header is omitted rather than sent empty.
//
// It stays unexported deliberately: a header an app sets by hand outlives the
// context it came from and skips the origin guard in [Transport], reaching
// whatever URL the request happened to name.
func outboundIdentityHeaders(ctx context.Context) http.Header {
	headers := http.Header{}
	if token, ok := TokenFromContext(ctx); ok {
		headers.Set(UserTokenHeader, BearerPrefix+token)
	}
	return headers
}

// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

// Package identity is the app-side identity surface for Catalyst agents.
//
// Two lines of app code buy verified inbound identity:
//
//	oauth := identity.OAuthConfig{Scopes: []string{"agent.invoke"}}
//	handler := identity.Middleware(oauth)(mux)
//
// Handlers read the verified caller with [UserFromContext].
//
// Outbound on-behalf-of calls then cost nothing beyond the client the app had
// to construct anyway:
//
//	client := identity.NewHTTPClient(nil)
//
// [NewHTTPClient] returns a plain [net/http.Client] that reads the caller's
// token off each request's context at send time, so ordinary Do calls carry
// the caller and an app never assembles identity headers itself. Outside a
// verified request the call goes out unauthenticated, with the header omitted
// rather than sent empty.
package identity

// UserTokenHeader carries the inbound caller's token. It is deliberately not
// Authorization: that header belongs to the app's own auth, and the sidecar
// mints this one separately.
const UserTokenHeader = "X-Diagrid-User-Token"

// BearerPrefix is the scheme prefix on the header value, matched
// case-insensitively on the way in and written verbatim on the way out.
const BearerPrefix = "Bearer "

// OAuthConfig is the policy the middleware enforces on every inbound request.
//
// The zero value is usable and fail-closed: it requires a verified token on
// every request, requires no particular scope, and discovers the issuer, JWKS
// endpoint and audience from the sidecar.
//
// It is policy and nothing else, so it stays copyable and comparable: the
// collaborators the middleware needs arrive as [Option] arguments to
// [Middleware]. See [WithVerifier].
type OAuthConfig struct {
	// Scopes must all be present on the verified token; the middleware
	// returns 403 oauth.missing_scope when any is absent.
	Scopes []string

	// Issuer is the expected iss claim. Normally discovered from the sidecar
	// /v1.0/metadata response; set it explicitly only when the metadata
	// endpoint is unavailable.
	Issuer string

	// Audience is the expected aud claim. Same discovery rules as Issuer.
	// When it is empty the aud claim is not checked.
	Audience string

	// JWKSURI is the JWKS endpoint used for signature verification. Same
	// discovery rules as Issuer.
	JWKSURI string

	// RequireAuth governs the no-token case alone. When it resolves to true,
	// a request without UserTokenHeader is rejected with 401
	// oauth.missing_token; when false, such a request reaches the handler with
	// no verified user in its context, so unauthenticated routes (health,
	// readiness) can share the same app. A token that is present is always
	// verified and an invalid one always rejected, either way.
	//
	// It is a pointer so that nil means unset and resolves to true: a plain
	// bool would default to false and make the zero OAuthConfig fail open.
	// Opting out therefore takes an addressable value:
	//
	//	open := false
	//	cfg := identity.OAuthConfig{RequireAuth: &open}
	RequireAuth *bool

	// AllowInsecureJWKS permits a resolved JWKS endpoint served over plain
	// http, and nothing beyond that: a file:// or otherwise non-http(s) URI
	// stays refused with the flag set, and so does a URI that is not a URL.
	//
	// The key set is the root of trust, so an on-path attacker who rewrites a
	// plaintext response can mint tokens this package accepts. Loopback, where
	// the local sidecar publishes its keys, is exempt without this flag.
	AllowInsecureJWKS bool
}

// VerifiedUser is the verified caller identity the middleware puts in the
// request context. Read it with [UserFromContext].
type VerifiedUser struct {
	// Subject is the sub claim: email, user id, or agent SPIFFE URI.
	Subject string

	// Tenant is the tenant or org claim extracted from the token.
	Tenant string

	// Scopes are the OAuth scopes the token carries, deduplicated and
	// ordinally sorted rather than left in token order, so a handler that
	// echoes them produces the same body for the same token every time.
	Scopes []string

	// Claims is the full decoded JWT payload, for policies that need richer
	// access than the fields above.
	Claims map[string]any

	// IssuerID is the iss value on the verified token.
	IssuerID string
}

// requireAuth resolves RequireAuth, treating the unset pointer as true so the
// zero value of OAuthConfig rejects a request that carries no token.
func (c OAuthConfig) requireAuth() bool {
	return c.RequireAuth == nil || *c.RequireAuth
}

// HasScope reports whether the verified token carries scope.
func (u *VerifiedUser) HasScope(scope string) bool {
	for _, s := range u.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

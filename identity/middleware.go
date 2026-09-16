// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// cacheControlNoStore keeps a rejection out of every cache between here and the
// caller; an authorization decision is good for exactly one request.
const cacheControlNoStore = "no-store"

// verifierBuildTimeout bounds the lazy verifier build — sidecar discovery plus
// the JWKS warm-up — so a hung endpoint cannot block the build forever.
const verifierBuildTimeout = 30 * time.Second

// scopeClaims are the scope spellings the middleware reads, in precedence
// order. Issuers disagree on how they spell scopes.
var scopeClaims = []string{"scp", "scope", "scopes"}

// Tenant claim spellings; see [tenantClaim] for how they are ordered.
const (
	claimTenantID = "tid"
	claimTenant   = "tenant"
)

// Middleware returns net/http middleware that verifies UserTokenHeader on every
// inbound request and puts the verified caller in the request context.
//
// Handlers read it back with [UserFromContext]:
//
//	mux := http.NewServeMux()
//	mux.HandleFunc("/invoke", func(w http.ResponseWriter, r *http.Request) {
//		user, ok := identity.UserFromContext(r.Context())
//		...
//	})
//	http.ListenAndServe(":8080", identity.Middleware(cfg)(mux))
//
// A rejected request never reaches the handler. The response body is
// {"error": "<code>"} with one of the oauth.* codes in this package.
// cfg carries policy; opts carry collaborators. See [WithVerifier].
func Middleware(cfg OAuthConfig, opts ...Option) func(http.Handler) http.Handler {
	var options middlewareOptions
	for _, opt := range opts {
		opt(&options)
	}

	gate := &oauthGate{config: cfg}
	if options.verifier != nil {
		verifier := options.verifier
		gate.verifier.Store(&verifier)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gate.serve(w, r, next)
		})
	}
}

// Option configures [Middleware] beyond the policy in [OAuthConfig]. Options
// carry collaborators, which is what keeps OAuthConfig pure policy.
type Option func(*middlewareOptions)

type middlewareOptions struct {
	verifier TokenVerifier
}

// WithVerifier supplies the [TokenVerifier] the middleware checks inbound
// credentials with.
//
// Without it the middleware builds a [JWKSVerifier] with [BuildVerifier] on the
// first request that needs one, which is what an app running in Catalyst wants.
// Supply one for a test, or for an app that resolves its own coordinates.
func WithVerifier(verifier TokenVerifier) Option {
	return func(o *middlewareOptions) { o.verifier = verifier }
}

// oauthGate holds the policy and the lazily built verifier shared by every
// request the middleware serves.
//
// The gate lives as long as the middleware, so it never closes the verifier it
// builds: that verifier's background JWKS refresher is meant to run for the
// life of the process.
type oauthGate struct {
	config OAuthConfig

	// buildMu serialises the one-time build behind verifier, so a burst of
	// requests at cold start shares one discovery instead of each running its
	// own. Once built, verifier is read without locking.
	verifier atomic.Pointer[TokenVerifier]
	buildMu  sync.Mutex
}

func (g *oauthGate) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	token := trimBearer(r.Header.Get(UserTokenHeader))
	if token == "" {
		if g.config.requireAuth() {
			writeError(w, http.StatusUnauthorized, ErrorCodeMissingToken)
			return
		}
		next.ServeHTTP(w, r)
		return
	}

	verifier, err := g.getVerifier()
	if err != nil {
		// Every request is rejected until coordinates turn up, so an app that
		// discovered nothing would otherwise answer 503 to everything with
		// nothing in its log to explain it.
		slog.Warn("identity verifier is not configured; rejecting request with "+ErrorCodeNotConfigured,
			"error", err)
		writeError(w, http.StatusServiceUnavailable, ErrorCodeNotConfigured)
		return
	}

	claims, err := adjudicate(r, verifier, token)
	if err != nil {
		rejectVerifyError(w, r, err)
		return
	}

	scopes := extractScopes(claims)
	if missing := missingScopes(g.config.Scopes, scopes); missing {
		writeError(w, http.StatusForbidden, ErrorCodeMissingScope)
		return
	}

	user := &VerifiedUser{
		Subject:  stringClaim(claims, claimSubject),
		Tenant:   tenantClaim(claims),
		Scopes:   scopes,
		Claims:   claims,
		IssuerID: stringClaim(claims, claimIssuer),
	}

	ctx := ContextWithUser(r.Context(), user)
	ctx = ContextWithToken(ctx, token)
	next.ServeHTTP(w, r.WithContext(ctx))
}

// adjudicate runs the verifier, turning a panic out of it into an error.
//
// A panic left to unwind reaches net/http, which drops the connection: the
// caller gets no envelope and no status at all. The recover is scoped to this
// one call, so a panic out of the app's own handler still belongs to net/http.
func adjudicate(r *http.Request, verifier TokenVerifier, token string) (claims map[string]any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: %v", errVerifierPanicked, recovered)
		}
	}()
	return verifier.Verify(r.Context(), token)
}

// rejectVerifyError answers a failed verification with the envelope its cause
// earns.
//
// The specific outcomes are matched first and the unanticipated one last: a
// broad match ahead of them would rewrite a correctly rejected token as a
// service fault and mask a missing scope outright.
func rejectVerifyError(w http.ResponseWriter, r *http.Request, err error) {
	var verr *TokenVerificationError
	switch {
	case errors.Is(err, ErrVerifierNotReady):
		writeError(w, http.StatusServiceUnavailable, ErrorCodeVerifierUnavailable)
	case errors.As(err, &verr):
		writeError(w, statusForCode(verr.Code), verr.Code)
	case r.Context().Err() != nil:
		// The caller is gone, so there is nothing to answer and no fault to
		// report: an aborted request belongs to net/http.
	default:
		// An unanticipated failure means the app cannot adjudicate any caller
		// right now, so the presented token is not at fault and 401 would be a
		// lie. The envelope carries only the code, so the cause goes to the log.
		slog.Warn("unexpected identity verification failure; rejecting request with "+ErrorCodeVerifierUnavailable,
			"errorType", fmt.Sprintf("%T", err), "error", err)
		writeError(w, http.StatusServiceUnavailable, ErrorCodeVerifierUnavailable)
	}
}

// getVerifier returns the shared verifier, building it on the first request
// that needs one. A build failure is not cached, so a sidecar that comes up
// late is picked up by a later request.
//
// The build runs on a bounded background context rather than the request's,
// because the verifier outlives the request that triggered it: one client
// disconnecting mid-build must not abort discovery for everyone else.
func (g *oauthGate) getVerifier() (TokenVerifier, error) {
	if verifier := g.verifier.Load(); verifier != nil {
		return *verifier, nil
	}

	g.buildMu.Lock()
	defer g.buildMu.Unlock()
	if verifier := g.verifier.Load(); verifier != nil {
		return *verifier, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), verifierBuildTimeout)
	defer cancel()

	built, err := BuildVerifier(ctx, g.config)
	if err != nil {
		return nil, err
	}
	var verifier TokenVerifier = built
	g.verifier.Store(&verifier)
	return verifier, nil
}

// trimBearer strips the optional scheme prefix, matched case-insensitively
// because the header is written by clients in several languages.
func trimBearer(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= len(BearerPrefix) && strings.EqualFold(value[:len(BearerPrefix)], BearerPrefix) {
		value = value[len(BearerPrefix):]
	}
	return strings.TrimSpace(value)
}

// extractScopes reads the scopes off the payload, accepting either a list or a
// space-delimited string under any of the claim names issuers use.
//
// The result is deduplicated and ordinally sorted rather than left in token
// order, so an app that echoes the scopes produces the same body for the same
// token every time.
func extractScopes(claims map[string]any) []string {
	for _, name := range scopeClaims {
		var scopes []string
		switch value := claims[name].(type) {
		case []string:
			scopes = value
		case []any:
			scopes = make([]string, 0, len(value))
			for _, item := range value {
				if s, ok := item.(string); ok {
					scopes = append(scopes, s)
				}
			}
		case string:
			scopes = strings.Fields(value)
		}
		if len(scopes) > 0 {
			return sortedUnique(scopes)
		}
	}
	return nil
}

// sortedUnique returns the distinct members of scopes in ordinal order, without
// touching the caller's slice.
func sortedUnique(scopes []string) []string {
	seen := make(map[string]struct{}, len(scopes))
	unique := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		unique = append(unique, scope)
	}
	slices.Sort(unique)
	return unique
}

func missingScopes(required, granted []string) bool {
	held := make(map[string]struct{}, len(granted))
	for _, scope := range granted {
		held[scope] = struct{}{}
	}
	for _, scope := range required {
		if _, ok := held[scope]; !ok {
			return true
		}
	}
	return false
}

func stringClaim(claims map[string]any, name string) string {
	value, _ := claims[name].(string)
	return value
}

// tenantClaim resolves the tenant the token names.
//
// claimTenantID decides whenever the key is present, whatever its value: an
// issuer publishing an empty tid is saying the caller has no tenant, not
// deferring to the second spelling.
func tenantClaim(claims map[string]any) string {
	if _, ok := claims[claimTenantID]; ok {
		return stringClaim(claims, claimTenantID)
	}
	return stringClaim(claims, claimTenant)
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", cacheControlNoStore)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

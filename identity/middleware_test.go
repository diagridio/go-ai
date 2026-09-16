// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubVerifier stands in for a JWKS-backed verifier so the middleware's own
// behavior is tested without key material.
type stubVerifier struct {
	claims map[string]any
	err    error
}

func (s stubVerifier) Verify(context.Context, string) (map[string]any, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.claims, nil
}

// echoHandler reports the verified caller, so a 200 proves the request reached
// the handler with the identity attached.
func echoHandler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := UserFromContext(r.Context())
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"subject":   user.Subject,
			"tenant":    user.Tenant,
			"scopes":    user.Scopes,
			"issuer_id": user.IssuerID,
		})
	})
}

// serve runs one request through the middleware and returns the recorded
// response.
func serve(t *testing.T, cfg OAuthConfig, next http.Handler, header string, opts ...Option) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/invoke", nil)
	if header != "" {
		req.Header.Set(UserTokenHeader, header)
	}
	rec := httptest.NewRecorder()
	Middleware(cfg, opts...)(next).ServeHTTP(rec, req)
	return rec
}

// errorBody returns the error code in a rejection response.
func errorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return body.Error
}

func stubClaims() map[string]any {
	return map[string]any{
		"sub": testSubject,
		"tid": "acme-corp",
		"scp": []any{"agent.invoke", "admin"},
		"iss": testIssuer,
		"exp": time.Now().Add(time.Hour).Unix(),
	}
}

func TestMiddlewareValidToken(t *testing.T) {
	cfg := OAuthConfig{Scopes: []string{"agent.invoke"}}

	rec := serve(t, cfg, echoHandler(t), BearerPrefix+"fake.jwt.token",
		WithVerifier(stubVerifier{claims: stubClaims()}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Subject  string   `json:"subject"`
		Tenant   string   `json:"tenant"`
		Scopes   []string `json:"scopes"`
		IssuerID string   `json:"issuer_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Subject != testSubject {
		t.Errorf("subject = %q, want %q", body.Subject, testSubject)
	}
	if body.Tenant != "acme-corp" {
		t.Errorf("tenant = %q, want acme-corp", body.Tenant)
	}
	if body.IssuerID != testIssuer {
		t.Errorf("issuer_id = %q, want %q", body.IssuerID, testIssuer)
	}
}

func TestMiddlewareFailClosed(t *testing.T) {
	reached := func(t *testing.T) http.Handler {
		t.Helper()
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("a rejected request must not reach the handler")
		})
	}

	tests := []struct {
		name       string
		cfg        OAuthConfig
		verifier   TokenVerifier
		header     string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing token",
			verifier:   stubVerifier{claims: stubClaims()},
			header:     "",
			wantStatus: http.StatusUnauthorized,
			wantCode:   ErrorCodeMissingToken,
		},
		{
			name:       "garbage token",
			verifier:   stubVerifier{err: tokenVerificationErrorf(ErrorCodeDecodeError, errors.New("not enough segments"))},
			header:     BearerPrefix + "garbage",
			wantStatus: http.StatusUnauthorized,
			wantCode:   ErrorCodeDecodeError,
		},
		{
			name:       "expired token",
			verifier:   stubVerifier{err: tokenVerificationErrorf(ErrorCodeExpired, nil)},
			header:     BearerPrefix + "expired.jwt",
			wantStatus: http.StatusUnauthorized,
			wantCode:   ErrorCodeExpired,
		},
		{
			name:       "wrong issuer",
			verifier:   stubVerifier{err: tokenVerificationErrorf(ErrorCodeInvalidIssuer, nil)},
			header:     BearerPrefix + "wrong.issuer.jwt",
			wantStatus: http.StatusUnauthorized,
			wantCode:   ErrorCodeInvalidIssuer,
		},
		{
			name:       "wrong audience",
			verifier:   stubVerifier{err: tokenVerificationErrorf(ErrorCodeInvalidAudience, nil)},
			header:     BearerPrefix + "wrong.audience.jwt",
			wantStatus: http.StatusUnauthorized,
			wantCode:   ErrorCodeInvalidAudience,
		},
		{
			name:       "bad signature",
			verifier:   stubVerifier{err: tokenVerificationErrorf(ErrorCodeInvalidSignature, nil)},
			header:     BearerPrefix + "bad.signature.jwt",
			wantStatus: http.StatusUnauthorized,
			wantCode:   ErrorCodeInvalidSignature,
		},
		{
			name:       "missing scope",
			cfg:        OAuthConfig{Scopes: []string{"admin.write"}},
			verifier:   stubVerifier{claims: stubClaims()},
			header:     BearerPrefix + "fake.jwt",
			wantStatus: http.StatusForbidden,
			wantCode:   ErrorCodeMissingScope,
		},
		{
			name:       "verifier not ready",
			verifier:   stubVerifier{err: ErrVerifierNotReady},
			header:     BearerPrefix + "fake.jwt",
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   ErrorCodeVerifierUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, tt.cfg, reached(t), tt.header, WithVerifier(tt.verifier))
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if code := errorBody(t, rec); code != tt.wantCode {
				t.Errorf("error = %q, want %q", code, tt.wantCode)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != cacheControlNoStore {
				t.Errorf("Cache-Control = %q, want %q", cc, cacheControlNoStore)
			}
		})
	}
}

// TestMiddlewareNotConfigured covers a build failure: no coordinates anywhere,
// so no verifier can be made and the caller gets 503 rather than a pass.
func TestMiddlewareNotConfigured(t *testing.T) {
	t.Setenv(envCatalystDaprHTTPPort, "")
	t.Setenv(envDaprHTTPPort, "")
	t.Setenv(envSentryIssuer, "")

	rec := serve(t, OAuthConfig{}, echoHandler(t), BearerPrefix+"fake.jwt")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if code := errorBody(t, rec); code != ErrorCodeNotConfigured {
		t.Errorf("error = %q, want %q", code, ErrorCodeNotConfigured)
	}
}

// TestMiddlewareBuildSurvivesRequestCancellation: the verifier outlives the
// request that happens to trigger its build, so it must not be built on that
// request's context. A client that disconnects mid-build would otherwise abort
// sidecar discovery for everyone and leave the gate answering
// oauth.not_configured.
func TestMiddlewareBuildSurvivesRequestCancellation(t *testing.T) {
	sidecar := newSidecarServer(t)
	t.Setenv(envCatalystDaprHTTPPort, portOf(t, sidecar.URL))
	t.Setenv(envDaprHTTPPort, portOf(t, sidecar.URL))
	t.Setenv(envSentryIssuer, "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodPost, "/invoke", nil).WithContext(ctx)
	req.Header.Set(UserTokenHeader, BearerPrefix+"not-a-jwt")
	rec := httptest.NewRecorder()
	Middleware(OAuthConfig{})(echoHandler(t)).ServeHTTP(rec, req)

	// A decode error means discovery ran and the token itself was judged. Only
	// an aborted build reports oauth.not_configured.
	if code := errorBody(t, rec); code != ErrorCodeDecodeError {
		t.Errorf("error = %q (status %d), want %q", code, rec.Code, ErrorCodeDecodeError)
	}
}

// newSidecarServer serves a /v1.0/metadata identity block whose JWKS endpoint
// it also hosts, so a verifier built from it discovers and warms entirely on
// loopback.
func newSidecarServer(t *testing.T) *httptest.Server {
	t.Helper()
	keys := newSigner(t).public
	mux := http.NewServeMux()
	mux.HandleFunc(jwksPathSuffix, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(keys); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	server := httptest.NewUnstartedServer(mux)
	mux.HandleFunc(metadataPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"test-app","identity":{"issuer":%q,"jwks_uri":%q}}`,
			server.URL, server.URL+jwksPathSuffix)
	})
	server.Start()
	t.Cleanup(server.Close)
	return server
}

// TestMiddlewareAuthorizationHeaderIgnored: the app's own Authorization header
// is not an identity the sidecar vouched for.
func TestMiddlewareAuthorizationHeaderIgnored(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/invoke", nil)
	req.Header.Set("Authorization", BearerPrefix+"some.jwt")
	rec := httptest.NewRecorder()

	Middleware(OAuthConfig{}, WithVerifier(stubVerifier{claims: stubClaims()}))(
		echoHandler(t)).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if code := errorBody(t, rec); code != ErrorCodeMissingToken {
		t.Errorf("error = %q, want %q", code, ErrorCodeMissingToken)
	}
}

// TestMiddlewareRequireAuthDisabled is the opt-out: RequireAuth explicitly
// false lets a request carrying no token through to the handler.
func TestMiddlewareRequireAuthDisabled(t *testing.T) {
	var sawUser bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawUser = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	open := false
	rec := serve(t, OAuthConfig{RequireAuth: &open}, next, "")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	// Downstream code tells "no verified caller" apart by the absence of the
	// value, so an unauthenticated request must not leave a zero one behind.
	if sawUser {
		t.Error("an unauthenticated request must not carry a verified user")
	}
}

// TestMiddlewareZeroConfigRequiresAuth is why RequireAuth is a pointer: a plain
// `RequireAuth bool` would default to false and make OAuthConfig{} fail open.
// The unset pointer must resolve to true instead.
func TestMiddlewareZeroConfigRequiresAuth(t *testing.T) {
	reached := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the zero config must not admit a request carrying no token")
	})

	cfg := OAuthConfig{}
	if cfg.RequireAuth != nil {
		t.Fatalf("RequireAuth = %v, want nil on the zero value", *cfg.RequireAuth)
	}

	rec := serve(t, cfg, reached, "")

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if code := errorBody(t, rec); code != ErrorCodeMissingToken {
		t.Errorf("error = %q, want %q", code, ErrorCodeMissingToken)
	}
}

// TestMiddlewareBearerPrefixOptional: the prefix is stripped case-insensitively
// and a bare token is accepted too.
func TestMiddlewareBearerPrefixOptional(t *testing.T) {
	for _, header := range []string{"Bearer the.raw.token", "bearer the.raw.token", "  the.raw.token  "} {
		t.Run(header, func(t *testing.T) {
			var got string
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, _ = TokenFromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			})

			rec := serve(t, OAuthConfig{}, next, header,
				WithVerifier(stubVerifier{claims: stubClaims()}))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got != "the.raw.token" {
				t.Errorf("token = %q, want the.raw.token", got)
			}
		})
	}
}

// TestMiddlewareOutboundIdentityHeadersDuringRequest: the raw token is on the
// request context for the duration of the handler, and nowhere else.
func TestMiddlewareOutboundIdentityHeadersDuringRequest(t *testing.T) {
	var inside http.Header
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inside = outboundIdentityHeaders(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	serve(t, OAuthConfig{}, next, BearerPrefix+"the.raw.token",
		WithVerifier(stubVerifier{claims: stubClaims()}))

	if want := BearerPrefix + "the.raw.token"; inside.Get(UserTokenHeader) != want {
		t.Errorf("outbound header = %q, want %q", inside.Get(UserTokenHeader), want)
	}
	if outside := outboundIdentityHeaders(context.Background()); len(outside) != 0 {
		t.Errorf("outside a request the outbound headers must be empty, got %v", outside)
	}
}

// TestMiddlewareConcurrentCallersKeepOwnToken: one middleware instance serves
// many callers at once, and each request's context carries only its own token.
func TestMiddlewareConcurrentCallersKeepOwnToken(t *testing.T) {
	const callers = 32

	var mu sync.Mutex
	seen := map[string]string{}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _ := TokenFromContext(r.Context())
		mu.Lock()
		seen[r.Header.Get("X-Test-Caller")] = token
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	handler := Middleware(OAuthConfig{}, WithVerifier(stubVerifier{claims: stubClaims()}))(next)

	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			caller := string(rune('a' + i%26))
			req := httptest.NewRequest(http.MethodPost, "/invoke", nil)
			req.Header.Set(UserTokenHeader, BearerPrefix+"token-"+caller)
			req.Header.Set("X-Test-Caller", caller)
			handler.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()

	for caller, token := range seen {
		if want := "token-" + caller; token != want {
			t.Errorf("caller %s saw token %q, want %q", caller, token, want)
		}
	}
}

func TestExtractScopes(t *testing.T) {
	tests := []struct {
		name   string
		claims map[string]any
		want   []string
	}{
		{"scp list", map[string]any{"scp": []any{"read", "write"}}, []string{"read", "write"}},
		{"scope space delimited", map[string]any{"scope": "read write"}, []string{"read", "write"}},
		{"scopes list", map[string]any{"scopes": []any{"read"}}, []string{"read"}},
		{"scp wins over scope", map[string]any{"scp": []any{"read"}, "scope": "write"}, []string{"read"}},
		{"empty scp falls through to scope", map[string]any{"scp": []any{}, "scope": "write"}, []string{"write"}},
		{"none", map[string]any{}, nil},
		// Scopes are a set, so a handler echoing them produces the same body
		// for the same token every time.
		{"list sorted ordinally", map[string]any{"scp": []any{"zeta", "alpha", "mu", "Beta"}}, []string{"Beta", "alpha", "mu", "zeta"}},
		{"string sorted ordinally", map[string]any{"scope": "zeta alpha mu"}, []string{"alpha", "mu", "zeta"}},
		{"list deduped", map[string]any{"scp": []any{"read", "write", "read"}}, []string{"read", "write"}},
		{"string deduped", map[string]any{"scope": "write read write"}, []string{"read", "write"}},
		{"typed slice sorted and deduped", map[string]any{"scopes": []string{"write", "read", "write"}}, []string{"read", "write"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractScopes(tt.claims)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("scopes = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestExtractScopesLeavesClaimSliceAlone: the sort must not reorder the slice
// the claims map still holds, which VerifiedUser.Claims hands to the handler.
func TestExtractScopesLeavesClaimSliceAlone(t *testing.T) {
	claim := []string{"zeta", "alpha"}
	claims := map[string]any{"scp": claim}

	if got, want := extractScopes(claims), []string{"alpha", "zeta"}; !slices.Equal(got, want) {
		t.Fatalf("scopes = %v, want %v", got, want)
	}
	if want := []string{"zeta", "alpha"}; !slices.Equal(claim, want) {
		t.Errorf("claim slice = %v, want it untouched as %v", claim, want)
	}
}

func TestTenantClaimFallback(t *testing.T) {
	claims := stubClaims()
	delete(claims, "tid")
	claims["tenant"] = "fallback-corp"

	var tenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _ := UserFromContext(r.Context())
		tenant = user.Tenant
		w.WriteHeader(http.StatusOK)
	})

	serve(t, OAuthConfig{}, next, BearerPrefix+"t", WithVerifier(stubVerifier{claims: claims}))

	if tenant != "fallback-corp" {
		t.Errorf("tenant = %q, want fallback-corp", tenant)
	}
}

// TestTenantClaimPrecedence: tid decides whenever the key is present, whatever
// its value, and tenant is consulted only when tid is absent entirely. An
// issuer publishing an empty tid is saying the caller has no tenant, not
// deferring to a second spelling.
func TestTenantClaimPrecedence(t *testing.T) {
	tests := []struct {
		name   string
		tenant map[string]any
		want   string
	}{
		{"tid wins", map[string]any{"tid": "acme", "tenant": "other"}, "acme"},
		{"empty tid still wins", map[string]any{"tid": "", "tenant": "acme"}, ""},
		{"tenant only when tid absent", map[string]any{"tenant": "acme"}, "acme"},
		{"neither", map[string]any{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := stubClaims()
			delete(claims, "tid")
			for name, value := range tt.tenant {
				claims[name] = value
			}

			var got string
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, _ := UserFromContext(r.Context())
				got = user.Tenant
				w.WriteHeader(http.StatusOK)
			})

			serve(t, OAuthConfig{}, next, BearerPrefix+"t", WithVerifier(stubVerifier{claims: claims}))

			if got != tt.want {
				t.Errorf("tenant = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMiddlewareNotConfiguredWarns: an app that discovered nothing answers 503
// to every token-carrying request, and must say so in its log rather than leave
// the operator with a silent outage.
func TestMiddlewareNotConfiguredWarns(t *testing.T) {
	t.Setenv(envCatalystDaprHTTPPort, "")
	t.Setenv(envDaprHTTPPort, "")
	t.Setenv(envDaprHTTPEndpoint, "")
	t.Setenv(envSentryIssuer, "")
	logs := captureWarnings(t)

	rec := serve(t, OAuthConfig{}, echoHandler(t), BearerPrefix+"fake.jwt")

	if code := errorBody(t, rec); code != ErrorCodeNotConfigured {
		t.Fatalf("error = %q, want %q", code, ErrorCodeNotConfigured)
	}
	got := logs.String()
	if !strings.Contains(got, ErrorCodeNotConfigured) {
		t.Errorf("warning must name %s; logs: %s", ErrorCodeNotConfigured, got)
	}
}

// TestOAuthConfigCarriesNoCollaborators: OAuthConfig is the policy the
// middleware enforces and nothing else, so it stays a pure, copyable value an
// app can build, log and compare. An interface-typed field is the signature of
// a collaborator that has leaked onto it.
func TestOAuthConfigCarriesNoCollaborators(t *testing.T) {
	typ := reflect.TypeOf(OAuthConfig{})
	for i := range typ.NumField() {
		field := typ.Field(i)
		if field.Type.Kind() == reflect.Interface {
			t.Errorf("OAuthConfig.%s is an interface (%s): supply collaborators "+
				"as a Middleware Option instead", field.Name, field.Type)
		}
	}
}

// TestMiddlewareWithVerifierOption: a verifier is supplied to the middleware,
// not through OAuthConfig. Discovery is unset here, so a middleware that
// ignored the option would build nothing and answer 503.
func TestMiddlewareWithVerifierOption(t *testing.T) {
	clearDiscoveryEnv(t)

	rec := serve(t, OAuthConfig{}, echoHandler(t), BearerPrefix+"fake.jwt",
		WithVerifier(stubVerifier{claims: stubClaims()}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// TestMiddlewareLastVerifierOptionWins: options are applied in order, so a
// later WithVerifier replaces an earlier one rather than being ignored.
func TestMiddlewareLastVerifierOptionWins(t *testing.T) {
	clearDiscoveryEnv(t)

	rec := serve(t, OAuthConfig{}, echoHandler(t), BearerPrefix+"fake.jwt",
		WithVerifier(stubVerifier{err: tokenVerificationErrorf(ErrorCodeExpired, nil)}),
		WithVerifier(stubVerifier{claims: stubClaims()}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// TestMiddlewareZeroConfigWithVerifierStillRequiresAuth: supplying a verifier
// says nothing about the no-token case, so the zero OAuthConfig stays
// fail-closed alongside one.
func TestMiddlewareZeroConfigWithVerifierStillRequiresAuth(t *testing.T) {
	reached := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the zero config must not admit a request carrying no token")
	})

	rec := serve(t, OAuthConfig{}, reached, "",
		WithVerifier(stubVerifier{claims: stubClaims()}))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if code := errorBody(t, rec); code != ErrorCodeMissingToken {
		t.Errorf("error = %q, want %q", code, ErrorCodeMissingToken)
	}
}

// panicVerifier panics instead of returning an error, standing in for a
// collaborator that fails in a way the TokenVerifier contract does not describe.
type panicVerifier struct {
	value any
}

func (p panicVerifier) Verify(context.Context, string) (map[string]any, error) {
	panic(p.value)
}

// TestMiddlewareUnexpectedVerifierFailureAnswersEnvelope: an unanticipated
// failure means the app cannot adjudicate any caller right now, so it answers
// 503 oauth.verifier_unavailable rather than blaming the presented token with a
// 401, and never escapes to net/http as a dropped connection or a raw 500.
func TestMiddlewareUnexpectedVerifierFailureAnswersEnvelope(t *testing.T) {
	tests := []struct {
		name     string
		verifier TokenVerifier
	}{
		{name: "plain error", verifier: stubVerifier{err: errors.New("verifier exploded")}},
		{name: "panic with error", verifier: panicVerifier{value: errors.New("verifier exploded")}},
		{name: "panic with string", verifier: panicVerifier{value: "verifier exploded"}},
		{name: "nil map dereference", verifier: panicVerifier{value: runtimePanicValue()}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("an unadjudicated request must not reach the handler")
			})

			rec := serve(t, OAuthConfig{}, reached, BearerPrefix+"fake.jwt",
				WithVerifier(tt.verifier))

			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503 (%s)", rec.Code, rec.Body.String())
			}
			if code := errorBody(t, rec); code != ErrorCodeVerifierUnavailable {
				t.Errorf("error = %q, want %q", code, ErrorCodeVerifierUnavailable)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != cacheControlNoStore {
				t.Errorf("Cache-Control = %q, want %q", cc, cacheControlNoStore)
			}
		})
	}
}

// runtimePanicValue is a runtime panic value, produced the only way there is:
// by provoking one.
func runtimePanicValue() any {
	var value any
	func() {
		defer func() { value = recover() }()
		var nilMap map[string]string
		nilMap["key"] = "value"
	}()
	return value
}

// TestMiddlewareUnexpectedVerifierFailureWarns: the 503 is a service fault, so
// the type and message of what caused it belong in the log — the response body
// carries only the code.
func TestMiddlewareUnexpectedVerifierFailureWarns(t *testing.T) {
	logs := captureWarnings(t)

	rec := serve(t, OAuthConfig{}, echoHandler(t), BearerPrefix+"fake.jwt",
		WithVerifier(stubVerifier{err: errors.New("verifier exploded")}))

	if code := errorBody(t, rec); code != ErrorCodeVerifierUnavailable {
		t.Fatalf("error = %q, want %q", code, ErrorCodeVerifierUnavailable)
	}
	got := logs.String()
	for _, want := range []string{ErrorCodeVerifierUnavailable, "verifier exploded"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning must name %q; logs: %s", want, got)
		}
	}
}

// TestMiddlewareRejectedTokenKeepsItsOwnCode is the regression the broad
// 503 route has to survive: it is matched after the specific outcomes, so a
// correctly rejected token still reports what was wrong with it. Ordering the
// broad case first would turn every one of these into a 503 and mask the 403
// outright.
func TestMiddlewareRejectedTokenKeepsItsOwnCode(t *testing.T) {
	tests := []struct {
		name       string
		cfg        OAuthConfig
		verifier   TokenVerifier
		wantStatus int
		wantCode   string
	}{
		{
			name:       "expired",
			verifier:   stubVerifier{err: tokenVerificationErrorf(ErrorCodeExpired, errors.New("exp is in the past"))},
			wantStatus: http.StatusUnauthorized,
			wantCode:   ErrorCodeExpired,
		},
		{
			name:       "missing scope",
			cfg:        OAuthConfig{Scopes: []string{"admin.write"}},
			verifier:   stubVerifier{claims: stubClaims()},
			wantStatus: http.StatusForbidden,
			wantCode:   ErrorCodeMissingScope,
		},
		{
			name:       "missing scope reported by the verifier",
			verifier:   stubVerifier{err: tokenVerificationErrorf(ErrorCodeMissingScope, nil)},
			wantStatus: http.StatusForbidden,
			wantCode:   ErrorCodeMissingScope,
		},
		{
			name:       "key material unavailable",
			verifier:   stubVerifier{err: fmt.Errorf("%w: jwks unreachable", ErrVerifierNotReady)},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   ErrorCodeVerifierUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, tt.cfg, echoHandler(t), BearerPrefix+"fake.jwt",
				WithVerifier(tt.verifier))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if code := errorBody(t, rec); code != tt.wantCode {
				t.Errorf("error = %q, want %q", code, tt.wantCode)
			}
		})
	}
}

// TestMiddlewareAbortedRequestIsNotAnswered: a caller that hung up gets no
// envelope. Answering one would report a service fault for a client-side
// disconnect and log a 503 nobody caused.
func TestMiddlewareAbortedRequestIsNotAnswered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodPost, "/invoke", nil).WithContext(ctx)
	req.Header.Set(UserTokenHeader, BearerPrefix+"fake.jwt")
	rec := httptest.NewRecorder()

	reached := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("an unadjudicated request must not reach the handler")
	})
	Middleware(OAuthConfig{}, WithVerifier(stubVerifier{err: context.Canceled}))(
		reached).ServeHTTP(rec, req)

	if body := rec.Body.String(); body != "" {
		t.Errorf("body = %q, want nothing written", body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("Cache-Control = %q, want nothing written", cc)
	}
}

// TestMiddlewareVerifierAbortPanicIsAnError: a verifier that panics gets its
// panic reported as an error, whatever it panicked with. http.ErrAbortHandler
// is no exception: it means abandon the response, which is net/http's answer to
// a handler and not something a verifier can ask for.
func TestMiddlewareVerifierAbortPanicIsAnError(t *testing.T) {
	res := serve(t, OAuthConfig{}, echoHandler(t), BearerPrefix+"fake.jwt",
		WithVerifier(panicVerifier{value: http.ErrAbortHandler}))

	if res.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
	if got := errorBody(t, res); got != ErrorCodeVerifierUnavailable {
		t.Errorf("code = %q, want %q", got, ErrorCodeVerifierUnavailable)
	}
}

// TestMiddlewareHandlerPanicIsNotSwallowed: the middleware recovers only its
// own adjudication. A panic out of the app's handler belongs to net/http and
// its own recovery, and turning it into 503 oauth.verifier_unavailable would
// report an identity failure for an application bug.
func TestMiddlewareHandlerPanicIsNotSwallowed(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Error("a handler panic must propagate to net/http")
		}
	}()

	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler bug")
	})
	serve(t, OAuthConfig{}, panicking, BearerPrefix+"fake.jwt",
		WithVerifier(stubVerifier{claims: stubClaims()}))
}

// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package identity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jws"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

// TestMain installs a transport that refuses every non-loopback request, so a
// test reaching for the real internet fails this package instead of quietly
// stalling on it or, worse, passing only where that host resolves.
func TestMain(m *testing.M) {
	guard := &loopbackOnlyTransport{next: http.DefaultTransport, expected: map[string]struct{}{}}
	netGuard = guard
	http.DefaultTransport = guard

	code := m.Run()

	if blocked := guard.blockedURLs(); len(blocked) > 0 {
		fmt.Fprintf(os.Stderr, "identity tests must not touch the network; blocked: %v\n", blocked)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// netGuard is the transport TestMain installs. A test that deliberately points
// the verifier at an unreachable non-loopback host registers it with
// expectBlockedHost.
var netGuard *loopbackOnlyTransport

// loopbackOnlyTransport passes loopback requests through and records and fails
// everything else.
type loopbackOnlyTransport struct {
	next http.RoundTripper

	mu       sync.Mutex
	blocked  []string
	expected map[string]struct{}
}

func (t *loopbackOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if isLoopbackHost(req.URL.Hostname()) {
		return t.next.RoundTrip(req)
	}
	t.mu.Lock()
	t.blocked = append(t.blocked, req.URL.String())
	t.mu.Unlock()
	return nil, fmt.Errorf("identity tests must not touch the network: blocked %s", req.URL)
}

// expectBlockedHost exempts host from the end-of-run check. The request is
// still refused; only the verdict on the package changes, so a test asserting
// on a JWKS URI it never means to reach does not read as an escape to the
// internet.
func (t *loopbackOnlyTransport) expectBlockedHost(host string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expected[host] = struct{}{}
}

func (t *loopbackOnlyTransport) blockedURLs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	unexpected := make([]string, 0, len(t.blocked))
	for _, raw := range t.blocked {
		parsed, err := url.Parse(raw)
		if err == nil {
			if _, ok := t.expected[parsed.Hostname()]; ok {
				continue
			}
		}
		unexpected = append(unexpected, raw)
	}
	return unexpected
}

const (
	testIssuer   = "https://oidc.example.com"
	testJWKSURI  = "https://oidc.example.com/jwks.json"
	testAudience = "catalyst"
	testKeyID    = "test-key"
	testSubject  = "alice@example.com"
)

// signer is a fixture RS256 keypair. Hand-crafting tokens against it keeps the
// fail-closed cases honest without an Auth0 tenant or a live JWKS server.
type signer struct {
	private jwk.Key
	public  jwk.Set
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	return newSignerWithKeyID(t, testKeyID)
}

// newSignerWithKeyID is newSigner under a chosen kid, so a test can stand up
// the key a rotation replaces the fixture one with.
func newSignerWithKeyID(t *testing.T, keyID string) *signer {
	t.Helper()
	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	private, err := jwk.FromRaw(raw)
	if err != nil {
		t.Fatalf("wrap private key: %v", err)
	}
	if err := private.Set(jwk.KeyIDKey, keyID); err != nil {
		t.Fatalf("set kid: %v", err)
	}
	if err := private.Set(jwk.AlgorithmKey, jwa.RS256); err != nil {
		t.Fatalf("set alg: %v", err)
	}
	public, err := private.PublicKey()
	if err != nil {
		t.Fatalf("derive public key: %v", err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(public); err != nil {
		t.Fatalf("add public key: %v", err)
	}
	return &signer{private: private, public: set}
}

// sign returns a token carrying claims, signed with the fixture key.
func (s *signer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	return s.signWith(t, jwa.RS256, s.private, claims)
}

func (s *signer) signWith(t *testing.T, alg jwa.SignatureAlgorithm, key any, claims map[string]any) string {
	t.Helper()
	token := jwt.New()
	for name, value := range claims {
		if err := token.Set(name, value); err != nil {
			t.Fatalf("set claim %q: %v", name, err)
		}
	}
	raw, err := jwt.Sign(token, jwt.WithKey(alg, key))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return string(raw)
}

// verifier returns a verifier serving the fixture key set instead of fetching
// a real JWKS.
func (s *signer) verifier(issuer, audience string) *JWKSVerifier {
	v := NewJWKSVerifier(issuer, testJWKSURI, audience)
	v.keys = func(context.Context) (jwk.Set, error) { return s.public, nil }
	return v
}

// validClaims is a token payload that passes every check.
func validClaims() map[string]any {
	now := time.Now()
	return map[string]any{
		"sub": testSubject,
		"iss": testIssuer,
		"exp": now.Add(time.Hour).Unix(),
		"iat": now.Unix(),
	}
}

// requireCode asserts that err is a verification failure carrying want.
func requireCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error %s, got nil", want)
	}
	var verr *TokenVerificationError
	if !errors.As(err, &verr) {
		t.Fatalf("want *TokenVerificationError %s, got %T: %v", want, err, err)
	}
	if verr.Code != want {
		t.Fatalf("want code %s, got %s (%v)", want, verr.Code, err)
	}
}

func TestVerifyValidToken(t *testing.T) {
	s := newSigner(t)
	token := s.sign(t, validClaims())

	claims, err := s.verifier(testIssuer, "").Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims["sub"] != testSubject {
		t.Errorf("sub = %v, want %s", claims["sub"], testSubject)
	}
	if claims["iss"] != testIssuer {
		t.Errorf("iss = %v, want %s", claims["iss"], testIssuer)
	}
}

func TestVerifyFailClosed(t *testing.T) {
	s := newSigner(t)
	other := newSigner(t)
	now := time.Now()

	tests := []struct {
		name     string
		issuer   string
		audience string
		token    func(t *testing.T) string
		wantCode string
	}{
		{
			name:   "expired token",
			issuer: testIssuer,
			token: func(t *testing.T) string {
				claims := validClaims()
				claims["exp"] = now.Add(-time.Hour).Unix()
				claims["iat"] = now.Add(-2 * time.Hour).Unix()
				return s.sign(t, claims)
			},
			wantCode: ErrorCodeExpired,
		},
		{
			name:   "wrong issuer",
			issuer: testIssuer,
			token: func(t *testing.T) string {
				claims := validClaims()
				claims["iss"] = "https://wrong-issuer.com"
				return s.sign(t, claims)
			},
			wantCode: ErrorCodeInvalidIssuer,
		},
		{
			name:     "wrong audience",
			issuer:   testIssuer,
			audience: testAudience,
			token: func(t *testing.T) string {
				claims := validClaims()
				claims["aud"] = []string{"some-other-service"}
				return s.sign(t, claims)
			},
			wantCode: ErrorCodeInvalidAudience,
		},
		{
			name:   "signature from an unknown key",
			issuer: testIssuer,
			token: func(t *testing.T) string {
				// Signed by a different keypair whose public half the fixture
				// JWKS also publishes under the same kid.
				return other.sign(t, validClaims())
			},
			wantCode: ErrorCodeInvalidSignature,
		},
		{
			name:     "garbage token",
			issuer:   testIssuer,
			token:    func(*testing.T) string { return "not-a-jwt" },
			wantCode: ErrorCodeDecodeError,
		},
		{
			name:   "missing sub claim",
			issuer: testIssuer,
			token: func(t *testing.T) string {
				claims := validClaims()
				delete(claims, "sub")
				return s.sign(t, claims)
			},
			wantCode: ErrorCodeInvalidToken,
		},
		{
			name:   "missing exp claim",
			issuer: testIssuer,
			token: func(t *testing.T) string {
				claims := validClaims()
				delete(claims, "exp")
				return s.sign(t, claims)
			},
			wantCode: ErrorCodeInvalidToken,
		},
		{
			name:   "hs256 token signed with the public key as a secret",
			issuer: testIssuer,
			token: func(t *testing.T) string {
				return s.signWith(t, jwa.HS256, []byte("public-key-as-secret"), validClaims())
			},
			wantCode: ErrorCodeInvalidToken,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := s.verifier(tt.issuer, tt.audience)
			_, err := v.Verify(context.Background(), tt.token(t))
			requireCode(t, err, tt.wantCode)
		})
	}
}

// TestVerifyRejectsUnsignedToken is the alg=none case: a token asserting it
// needs no signature must never be accepted, whatever its claims say.
func TestVerifyRejectsUnsignedToken(t *testing.T) {
	s := newSigner(t)
	token := jwt.New()
	for name, value := range validClaims() {
		if err := token.Set(name, value); err != nil {
			t.Fatalf("set claim %q: %v", name, err)
		}
	}
	raw, err := jwt.Sign(token, jwt.WithInsecureNoSignature())
	if err != nil {
		t.Fatalf("sign unsigned token: %v", err)
	}

	_, err = s.verifier(testIssuer, "").Verify(context.Background(), string(raw))
	requireCode(t, err, ErrorCodeInvalidToken)
}

// TestVerifyAudienceUncheckedWhenUnset: aud is enforced only when an audience
// is configured.
func TestVerifyAudienceUncheckedWhenUnset(t *testing.T) {
	s := newSigner(t)
	claims := validClaims()
	claims["aud"] = []string{"some-other-service"}

	if _, err := s.verifier(testIssuer, "").Verify(context.Background(), s.sign(t, claims)); err != nil {
		t.Fatalf("want aud ignored when no audience is configured, got %v", err)
	}
}

// TestVerifyWithinClockSkew proves the 120s leeway: a token that expired a
// minute ago is still accepted.
func TestVerifyWithinClockSkew(t *testing.T) {
	s := newSigner(t)
	claims := validClaims()
	claims["exp"] = time.Now().Add(-60 * time.Second).Unix()

	if _, err := s.verifier(testIssuer, "").Verify(context.Background(), s.sign(t, claims)); err != nil {
		t.Fatalf("want a token inside the clock skew accepted, got %v", err)
	}
}

// TestVerifyUnknownKeyIDIsNotReady distinguishes a key-distribution problem
// from a bad token: the caller gets 503, not 401.
func TestVerifyUnknownKeyIDIsNotReady(t *testing.T) {
	s := newSigner(t)
	v := NewJWKSVerifier(testIssuer, testJWKSURI, "")
	v.keys = func(context.Context) (jwk.Set, error) { return jwk.NewSet(), nil }

	_, err := v.Verify(context.Background(), s.sign(t, validClaims()))
	if !errors.Is(err, ErrVerifierNotReady) {
		t.Fatalf("want ErrVerifierNotReady, got %v", err)
	}
}

// TestVerifyKeyFetchFailureIsNotReady covers a JWKS endpoint that is down.
func TestVerifyKeyFetchFailureIsNotReady(t *testing.T) {
	s := newSigner(t)
	v := NewJWKSVerifier(testIssuer, testJWKSURI, "")
	v.keys = func(context.Context) (jwk.Set, error) { return nil, errors.New("connection refused") }

	_, err := v.Verify(context.Background(), s.sign(t, validClaims()))
	if !errors.Is(err, ErrVerifierNotReady) {
		t.Fatalf("want ErrVerifierNotReady, got %v", err)
	}
}

// TestVerifyRefetchesJWKSAfterKeyRotation is the signing-key rotation case: a
// kid the cached JWKS does not hold must trigger one forced refetch, not a
// 503 that lasts until the cache lifetime expires.
func TestVerifyRefetchesJWKSAfterKeyRotation(t *testing.T) {
	stale := newSigner(t)
	rotated := newSignerWithKeyID(t, "rotated-key")

	var refreshes atomic.Int32
	v := NewJWKSVerifier(testIssuer, testJWKSURI, "")
	v.keys = func(context.Context) (jwk.Set, error) { return stale.public, nil }
	v.refreshKeys = func(context.Context) (jwk.Set, error) {
		refreshes.Add(1)
		return rotated.public, nil
	}

	claims, err := v.Verify(context.Background(), rotated.sign(t, validClaims()))
	if err != nil {
		t.Fatalf("want the rotated key picked up by a forced refetch, got %v", err)
	}
	if claims["sub"] != testSubject {
		t.Errorf("sub = %v, want %s", claims["sub"], testSubject)
	}
	if got := refreshes.Load(); got != 1 {
		t.Errorf("forced refreshes = %d, want 1", got)
	}
}

// TestVerifyRateLimitsForcedJWKSRefresh proves the refetch above cannot be
// turned into a lever on the JWKS endpoint: a stream of unknown kid values
// buys exactly one refetch.
func TestVerifyRateLimitsForcedJWKSRefresh(t *testing.T) {
	s := newSigner(t)

	var refreshes atomic.Int32
	v := NewJWKSVerifier(testIssuer, testJWKSURI, "")
	v.keys = func(context.Context) (jwk.Set, error) { return jwk.NewSet(), nil }
	v.refreshKeys = func(context.Context) (jwk.Set, error) {
		refreshes.Add(1)
		return jwk.NewSet(), nil
	}

	const attempts = 5
	for i := range attempts {
		_, err := v.Verify(context.Background(), s.sign(t, validClaims()))
		if !errors.Is(err, ErrVerifierNotReady) {
			t.Fatalf("attempt %d: want ErrVerifierNotReady, got %v", i, err)
		}
	}
	if got := refreshes.Load(); got != 1 {
		t.Errorf("forced refreshes over %d unknown kids = %d, want 1", attempts, got)
	}
}

// TestVerifyRejectsMultiSignatureToken covers the JSON serialization jws.Parse
// accepts but a JWT never uses. The algorithm allowlist can vet only one
// signature, so a token carrying more than one must be refused outright rather
// than verified against whichever signature happens to match a key.
func TestVerifyRejectsMultiSignatureToken(t *testing.T) {
	s := newSigner(t)

	token := jwt.New()
	for name, value := range validClaims() {
		if err := token.Set(name, value); err != nil {
			t.Fatalf("set claim %q: %v", name, err)
		}
	}
	payload, err := jwt.NewSerializer().Serialize(token)
	if err != nil {
		t.Fatalf("serialize claims: %v", err)
	}

	raw, err := jws.Sign(payload,
		jws.WithJSON(),
		// The allowlist only ever sees this one.
		jws.WithKey(jwa.RS256, s.private),
		// And never this one.
		jws.WithKey(jwa.HS256, []byte("attacker-secret")),
	)
	if err != nil {
		t.Fatalf("sign multi-signature token: %v", err)
	}

	_, err = s.verifier(testIssuer, "").Verify(context.Background(), string(raw))
	requireCode(t, err, ErrorCodeDecodeError)
}

// TestVerifyAfterCloseIsNotReady proves Close is final: a closed verifier must
// not quietly start a second background refresher on the next request.
func TestVerifyAfterCloseIsNotReady(t *testing.T) {
	s := newSigner(t)
	v := NewJWKSVerifier(testIssuer, testJWKSURI, "")
	if err := v.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err := v.Verify(context.Background(), s.sign(t, validClaims()))
	if !errors.Is(err, ErrVerifierNotReady) {
		t.Fatalf("want ErrVerifierNotReady, got %v", err)
	}
}

// TestCloseStopsBackgroundRefresher is the leak case: every verifier starts a
// JWKS refresher goroutine on its first fetch, and a process that builds more
// than one verifier must be able to stop them again.
func TestCloseStopsBackgroundRefresher(t *testing.T) {
	jwks := newJWKSServer(t, newSigner(t).public)

	// Warm and close one verifier first, so anything the JWKS machinery or the
	// HTTP client keeps around once per process — idle connections included —
	// is already in the baseline.
	warmup := NewJWKSVerifier(testIssuer, jwks.URL, "")
	if err := warmup.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if err := warmup.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	baseline := settledGoroutines(t)

	const verifiers = 5
	built := make([]*JWKSVerifier, 0, verifiers)
	for range verifiers {
		v := NewJWKSVerifier(testIssuer, jwks.URL, "")
		if err := v.Warm(context.Background()); err != nil {
			t.Fatalf("warm: %v", err)
		}
		built = append(built, v)
	}
	if grown := runtime.NumGoroutine(); grown <= baseline {
		t.Fatalf("goroutines = %d after warming %d verifiers, want more than the baseline %d: "+
			"the test cannot prove a leak it never reproduced", grown, verifiers, baseline)
	}

	for _, v := range built {
		if err := v.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	if left := settledGoroutines(t); left > baseline {
		t.Errorf("goroutines = %d after closing %d verifiers, want back to the baseline %d",
			left, verifiers, baseline)
	}
}

// settledGoroutines waits for the goroutine count to stop moving and returns
// it. Goroutines stop asynchronously, so a count read the instant after a
// Close says nothing.
func settledGoroutines(t *testing.T) int {
	t.Helper()
	const (
		limit       = 5 * time.Second
		step        = 20 * time.Millisecond
		stableReads = 5
	)
	count := runtime.NumGoroutine()
	stable := 0
	for deadline := time.Now().Add(limit); time.Now().Before(deadline); {
		time.Sleep(step)
		switch next := runtime.NumGoroutine(); {
		case next != count:
			count, stable = next, 0
		case stable >= stableReads:
			return count
		default:
			stable++
		}
	}
	return count
}

func TestVerifyErrorsMatchSentinels(t *testing.T) {
	s := newSigner(t)
	claims := validClaims()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()

	_, err := s.verifier(testIssuer, "").Verify(context.Background(), s.sign(t, claims))
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("want errors.Is(err, ErrExpired), got %v", err)
	}
	if errors.Is(err, ErrInvalidSignature) {
		t.Fatal("expiry must not match the invalid-signature sentinel")
	}
}

func TestDiscoverFromEnv(t *testing.T) {
	t.Setenv(envSentryIssuer, "https://oidc.test.com/org/region")

	coords := discoverFromEnv()
	if coords == nil {
		t.Fatal("want coordinates, got nil")
	}
	if coords.issuer != "https://oidc.test.com/org/region" {
		t.Errorf("issuer = %q", coords.issuer)
	}
	if want := "https://oidc.test.com/org/region/jwks.json"; coords.jwksURI != want {
		t.Errorf("jwksURI = %q, want %q", coords.jwksURI, want)
	}
}

func TestDiscoverFromEnvUnset(t *testing.T) {
	t.Setenv(envSentryIssuer, "")
	if coords := discoverFromEnv(); coords != nil {
		t.Fatalf("want nil, got %+v", coords)
	}
}

func TestDiscoverFromMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != metadataPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"test-app","identity":{` +
			`"issuer":"https://oidc.test.com/org/region",` +
			`"jwks_uri":"https://oidc.test.com/org/region/jwks.json"}}`))
	}))
	defer server.Close()

	t.Setenv(envDaprHTTPPort, portOf(t, server.URL))

	coords := discoverFromMetadataAt(t, server.URL)
	if coords == nil {
		t.Fatal("want coordinates, got nil")
	}
	if coords.issuer != "https://oidc.test.com/org/region" {
		t.Errorf("issuer = %q", coords.issuer)
	}
}

func TestDiscoverFromMetadataNoPort(t *testing.T) {
	t.Setenv(envCatalystDaprHTTPPort, "")
	t.Setenv(envDaprHTTPPort, "")
	if coords := discoverFromMetadata(context.Background()); coords != nil {
		t.Fatalf("want nil, got %+v", coords)
	}
}

func TestDiscoverFromMetadataNoIdentityBlock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"test-app"}`))
	}))
	defer server.Close()

	if coords := discoverFromMetadataAt(t, server.URL); coords != nil {
		t.Fatalf("want nil, got %+v", coords)
	}
}

// The remote sidecar is the source an app running on a developer's machine
// needs: nothing is listening on loopback, so the endpoint is the only way to
// reach one.

func TestDiscoverFromRemote(t *testing.T) {
	sidecar := newMetadataServer(t, `{"id":"test-app","identity":{`+
		`"issuer":"https://oidc.test.com/org/region",`+
		`"jwks_uri":"https://oidc.test.com/org/region/jwks.json"}}`)

	// The trailing slash is deliberate: a published endpoint can carry one and
	// metadataPath brings its own.
	t.Setenv(envDaprHTTPEndpoint, sidecar.URL+"/")

	coords := discoverFromRemote(context.Background())
	if coords == nil {
		t.Fatal("want coordinates, got nil")
	}
	if want := "https://oidc.test.com/org/region"; coords.issuer != want {
		t.Errorf("issuer = %q, want %q", coords.issuer, want)
	}
	if want := "https://oidc.test.com/org/region/jwks.json"; coords.jwksURI != want {
		t.Errorf("jwksURI = %q, want %q", coords.jwksURI, want)
	}

	requests := sidecar.received()
	if len(requests) != 1 {
		t.Fatalf("sidecar received %d requests, want 1", len(requests))
	}
	if requests[0].URL.Path != metadataPath {
		t.Errorf("requested path = %q, want %q", requests[0].URL.Path, metadataPath)
	}
}

func TestDiscoverFromRemoteNoEndpoint(t *testing.T) {
	t.Setenv(envDaprHTTPEndpoint, "")
	if coords := discoverFromRemote(context.Background()); coords != nil {
		t.Fatalf("want nil, got %+v", coords)
	}
}

// TestDiscoverFromRemoteSendsAPIToken asserts on the request the sidecar
// received, not on the header the caller believes it set.
func TestDiscoverFromRemoteSendsAPIToken(t *testing.T) {
	sidecar := newMetadataServer(t, `{"identity":{"issuer":"https://oidc.test.com/org/region"}}`)
	t.Setenv(envDaprHTTPEndpoint, sidecar.URL)
	t.Setenv(envDaprAPIToken, testAPIToken)
	captureWarnings(t)

	if coords := discoverFromRemote(context.Background()); coords == nil {
		t.Fatal("want coordinates, got nil")
	}

	requests := sidecar.received()
	if len(requests) != 1 {
		t.Fatalf("sidecar received %d requests, want 1", len(requests))
	}
	if got := requests[0].Header.Get(apiTokenHeader); got != testAPIToken {
		t.Errorf("%s header = %q, want %q", apiTokenHeader, got, testAPIToken)
	}
}

func TestDiscoverFromRemoteOmitsAPITokenWhenUnset(t *testing.T) {
	sidecar := newMetadataServer(t, `{"identity":{"issuer":"https://oidc.test.com/org/region"}}`)
	t.Setenv(envDaprHTTPEndpoint, sidecar.URL)
	t.Setenv(envDaprAPIToken, "")

	if coords := discoverFromRemote(context.Background()); coords == nil {
		t.Fatal("want coordinates, got nil")
	}

	requests := sidecar.received()
	if len(requests) != 1 {
		t.Fatalf("sidecar received %d requests, want 1", len(requests))
	}
	if values := requests[0].Header.Values(apiTokenHeader); len(values) != 0 {
		t.Errorf("%s header = %q, want it absent entirely", apiTokenHeader, values)
	}
}

// TestDiscoverFromRemoteRequestFailure serves a perfectly good identity block
// under a 500, so the status is what the case turns on rather than the body.
func TestDiscoverFromRemoteRequestFailure(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"identity":{"issuer":"https://oidc.test.com/org/region"}}`))
	}))
	t.Cleanup(sidecar.Close)

	t.Setenv(envDaprHTTPEndpoint, sidecar.URL)
	captureWarnings(t)

	if coords := discoverFromRemote(context.Background()); coords != nil {
		t.Fatalf("want nil, got %+v", coords)
	}
}

func TestDiscoverFromRemoteWarnsWhenUnreachable(t *testing.T) {
	endpoint := closedLoopbackURL(t)
	t.Setenv(envDaprHTTPEndpoint, endpoint)
	t.Setenv(envDaprAPIToken, testAPIToken)
	logs := captureWarnings(t)

	if coords := discoverFromRemote(context.Background()); coords != nil {
		t.Fatalf("want nil, got %+v", coords)
	}

	got := logs.String()
	if want := endpoint + metadataPath; !strings.Contains(got, want) {
		t.Errorf("warning must name %q; logs: %s", want, got)
	}
	if !strings.Contains(got, "trying the next source") {
		t.Errorf("warning must say the next source is tried; logs: %s", got)
	}
	if strings.Contains(got, testAPIToken) {
		t.Errorf("logs must never carry the api token; logs: %s", got)
	}
}

// TestDiscoverFromMetadataWarnsWhenUnreachable covers the local source at the
// same level as the remote one: a sidecar that was configured and did not
// answer is worth a warning either way.
func TestDiscoverFromMetadataWarnsWhenUnreachable(t *testing.T) {
	port := portOf(t, closedLoopbackURL(t))
	t.Setenv(envCatalystDaprHTTPPort, port)
	t.Setenv(envDaprHTTPPort, "")
	logs := captureWarnings(t)

	if coords := discoverFromMetadata(context.Background()); coords != nil {
		t.Fatalf("want nil, got %+v", coords)
	}

	got := logs.String()
	if want := fmt.Sprintf("http://%s:%s%s", metadataHost, port, metadataPath); !strings.Contains(got, want) {
		t.Errorf("warning must name %q; logs: %s", want, got)
	}
	if !strings.Contains(got, "trying the next source") {
		t.Errorf("warning must say the next source is tried; logs: %s", got)
	}
}

// TestDiscoverFromRemoteSilentWhenEndpointUnset is the other half of the
// warning: an unconfigured source is absent, not broken, and must not be
// reported as a failure on every app that has no remote sidecar.
func TestDiscoverFromRemoteSilentWhenEndpointUnset(t *testing.T) {
	t.Setenv(envDaprHTTPEndpoint, "")
	logs := captureWarnings(t)

	if coords := discoverFromRemote(context.Background()); coords != nil {
		t.Fatalf("want nil, got %+v", coords)
	}
	if got := logs.String(); got != "" {
		t.Errorf("an unconfigured source must log nothing; logs: %s", got)
	}
}

// TestDiscoverFromRemoteWarnsOnPlaintextToken: the token still goes, because a
// self-hosted sidecar on plain http is a valid setup. The warning is the whole
// response, not a refusal.
func TestDiscoverFromRemoteWarnsOnPlaintextToken(t *testing.T) {
	// httptest serves plain http on loopback, which is the transport the
	// warning is about.
	sidecar := newMetadataServer(t, `{"identity":{"issuer":"https://oidc.test.com/org/region"}}`)
	t.Setenv(envDaprHTTPEndpoint, sidecar.URL)
	t.Setenv(envDaprAPIToken, testAPIToken)
	logs := captureWarnings(t)

	if coords := discoverFromRemote(context.Background()); coords == nil {
		t.Fatal("want coordinates, got nil: the token is warned about, not withheld")
	}

	got := logs.String()
	if !strings.Contains(got, "clear text") {
		t.Errorf("want a clear-text warning; logs: %s", got)
	}
	if !strings.Contains(got, sidecar.URL) {
		t.Errorf("warning must name the endpoint %q; logs: %s", sidecar.URL, got)
	}
	if strings.Contains(got, testAPIToken) {
		t.Errorf("logs must never carry the api token; logs: %s", got)
	}

	requests := sidecar.received()
	if len(requests) != 1 {
		t.Fatalf("sidecar received %d requests, want 1", len(requests))
	}
	if value := requests[0].Header.Get(apiTokenHeader); value != testAPIToken {
		t.Errorf("%s header = %q, want the token sent anyway", apiTokenHeader, value)
	}
}

// TestDiscoverFromRemoteNoPlaintextWarningOverHTTPS pins the other side of the
// transport check. Nothing listens on the endpoint and nothing needs to:
// whether the request succeeds is beside the point, the assertion is on the
// warning the scheme does not produce.
func TestDiscoverFromRemoteNoPlaintextWarningOverHTTPS(t *testing.T) {
	t.Setenv(envDaprHTTPEndpoint, "https://127.0.0.1:1")
	t.Setenv(envDaprAPIToken, testAPIToken)
	logs := captureWarnings(t)

	if coords := discoverFromRemote(context.Background()); coords != nil {
		t.Fatalf("want nil, got %+v", coords)
	}

	got := logs.String()
	if strings.Contains(got, "clear text") {
		t.Errorf("https must not warn about clear text; logs: %s", got)
	}
	if strings.Contains(got, testAPIToken) {
		t.Errorf("logs must never carry the api token; logs: %s", got)
	}
}

// TestDiscoverFromRemoteMalformedBody: a body that is not the expected shape
// is no discovery, not an error the caller has to handle.
func TestDiscoverFromRemoteMalformedBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not an object", `["not","an","object"]`},
		{"issuer is not a string", `{"identity":{"issuer":123}}`},
		{"no identity block", `{"id":"test-app"}`},
		{"identity block carries no issuer", `{"identity":{"audience":"aud"}}`},
		{"not json at all", `<html>nope</html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sidecar := newMetadataServer(t, tc.body)
			t.Setenv(envDaprHTTPEndpoint, sidecar.URL)
			captureWarnings(t)

			if coords := discoverFromRemote(context.Background()); coords != nil {
				t.Fatalf("want nil, got %+v", coords)
			}
		})
	}
}

// TestBuildVerifierPrefersLocalOverRemote pins the deliberate order: a
// deployed in-cluster app keeps using the loopback call rather than paying for
// a network round trip, so the remote endpoint must not be asked at all when
// the local one answered.
func TestBuildVerifierPrefersLocalOverRemote(t *testing.T) {
	jwks := newJWKSServer(t, newSigner(t).public)
	localIssuer := jwks.URL

	local := newMetadataServer(t, fmt.Sprintf(`{"id":"test-app","identity":{"issuer":%q,"jwks_uri":%q}}`,
		localIssuer, jwks.URL+jwksPathSuffix))
	remote := newMetadataServer(t, fmt.Sprintf(`{"id":"test-app","identity":{"issuer":%q,"jwks_uri":%q}}`,
		"https://remote.test.com", "https://remote.test.com/jwks.json"))

	t.Setenv(envCatalystDaprHTTPPort, portOf(t, local.URL))
	t.Setenv(envDaprHTTPPort, portOf(t, local.URL))
	t.Setenv(envDaprHTTPEndpoint, remote.URL)
	t.Setenv(envSentryIssuer, "")

	v, err := BuildVerifier(context.Background(), OAuthConfig{})
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })

	if v.issuer != localIssuer {
		t.Errorf("issuer = %q, want the local sidecar's %q", v.issuer, localIssuer)
	}
	if n := len(remote.received()); n != 0 {
		t.Errorf("remote sidecar received %d requests, want none once the local one answered", n)
	}
}

func TestBuildVerifierFallsBackToRemote(t *testing.T) {
	jwks := newJWKSServer(t, newSigner(t).public)
	remoteIssuer := jwks.URL
	remoteJWKSURI := jwks.URL + "/keys/jwks"

	remote := newMetadataServer(t, fmt.Sprintf(`{"id":"test-app","identity":{"issuer":%q,"jwks_uri":%q}}`,
		remoteIssuer, remoteJWKSURI))

	// No local sidecar to answer first.
	t.Setenv(envCatalystDaprHTTPPort, "")
	t.Setenv(envDaprHTTPPort, "")
	t.Setenv(envDaprHTTPEndpoint, remote.URL)
	t.Setenv(envSentryIssuer, "")

	v, err := BuildVerifier(context.Background(), OAuthConfig{})
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })

	if v.issuer != remoteIssuer {
		t.Errorf("issuer = %q, want the remote sidecar's %q", v.issuer, remoteIssuer)
	}
	if v.jwksURI != remoteJWKSURI {
		t.Errorf("jwksURI = %q, want the remote sidecar's %q", v.jwksURI, remoteJWKSURI)
	}
	if n := len(remote.received()); n != 1 {
		t.Errorf("remote sidecar received %d requests, want 1", n)
	}
}

func TestBuildVerifierRemoteFailureFallsBackToEnv(t *testing.T) {
	jwks := newJWKSServer(t, newSigner(t).public)
	envIssuer := jwks.URL

	t.Setenv(envCatalystDaprHTTPPort, "")
	t.Setenv(envDaprHTTPPort, "")
	t.Setenv(envDaprHTTPEndpoint, closedLoopbackURL(t))
	t.Setenv(envSentryIssuer, envIssuer)
	captureWarnings(t)

	v, err := BuildVerifier(context.Background(), OAuthConfig{})
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })

	if v.issuer != envIssuer {
		t.Errorf("issuer = %q, want the environment's %q", v.issuer, envIssuer)
	}
	if want := defaultJWKSURI(envIssuer); v.jwksURI != want {
		t.Errorf("jwksURI = %q, want %q", v.jwksURI, want)
	}
}

func TestBuildVerifierExplicitConfigSkipsDiscovery(t *testing.T) {
	// BuildVerifier warms the verifier, so the JWKS must be a loopback server:
	// a public URL here would make the test depend on the internet.
	jwks := newJWKSServer(t, newSigner(t).public)

	// Ports that would fail discovery prove the explicit config wins. Both
	// spellings are set, so a machine running a sidecar cannot supply one.
	t.Setenv(envCatalystDaprHTTPPort, "1")
	t.Setenv(envDaprHTTPPort, "1")
	t.Setenv(envDaprHTTPEndpoint, "")
	t.Setenv(envSentryIssuer, "https://env-issuer.example.com")

	wantJWKSURI := jwks.URL + jwksPathSuffix
	v, err := BuildVerifier(context.Background(), OAuthConfig{
		Issuer:  testIssuer,
		JWKSURI: wantJWKSURI,
	})
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })

	if v.issuer != testIssuer {
		t.Errorf("issuer = %q, want %q", v.issuer, testIssuer)
	}
	if v.jwksURI != wantJWKSURI {
		t.Errorf("jwksURI = %q, want %q", v.jwksURI, wantJWKSURI)
	}
}

func TestBuildVerifierDerivesJWKSURIFromIssuer(t *testing.T) {
	jwks := newJWKSServer(t, newSigner(t).public)

	// Both port variables must be cleared: with either one set, a machine
	// running a sidecar would exercise metadata discovery instead of the
	// derivation this test is named for.
	t.Setenv(envCatalystDaprHTTPPort, "")
	t.Setenv(envDaprHTTPPort, "")
	t.Setenv(envDaprHTTPEndpoint, "")
	t.Setenv(envSentryIssuer, "")

	v, err := BuildVerifier(context.Background(), OAuthConfig{Issuer: jwks.URL + "/"})
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })

	if want := jwks.URL + jwksPathSuffix; v.jwksURI != want {
		t.Errorf("jwksURI = %q, want %q", v.jwksURI, want)
	}
}

// TestBuildVerifierPinnedIssuerIgnoresForeignJWKSURI guards the other half of
// the fallback order: a config that names its own issuer must never be checked
// against a different issuer's keys. Adopting the advertised jwks_uri there
// would let a token minted by the advertised issuer, claiming the pinned one,
// verify.
func TestBuildVerifierPinnedIssuerIgnoresForeignJWKSURI(t *testing.T) {
	// The pinned issuer is served locally so the derived default it produces
	// stays in-process; newJWKSServer answers the key set on any path.
	pinned := newJWKSServer(t, newSigner(t).public)
	pinnedIssuer := pinned.URL

	advertised := newJWKSServer(t, newSigner(t).public)
	advertisedJWKSURI := advertised.URL + "/keys/jwks"

	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != metadataPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"test-app","identity":{"issuer":%q,"jwks_uri":%q}}`,
			"https://other.acme.example", advertisedJWKSURI)
	}))
	t.Cleanup(sidecar.Close)

	t.Setenv(envCatalystDaprHTTPPort, portOf(t, sidecar.URL))
	t.Setenv(envDaprHTTPPort, portOf(t, sidecar.URL))
	t.Setenv(envDaprHTTPEndpoint, "")
	t.Setenv(envSentryIssuer, "")

	v, err := BuildVerifier(context.Background(), OAuthConfig{
		Issuer:            pinnedIssuer,
		AllowInsecureJWKS: true, // httptest serves plain http
	})
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })

	if v.issuer != pinnedIssuer {
		t.Errorf("issuer = %q, want the pinned %q", v.issuer, pinnedIssuer)
	}
	if want := defaultJWKSURI(pinnedIssuer); v.jwksURI != want {
		t.Errorf("jwksURI = %q, want %q derived from the pinned issuer; the sidecar advertised %q for a different issuer",
			v.jwksURI, want, advertisedJWKSURI)
	}
}

// TestBuildVerifierPrefersPublishedJWKSURI pins the fallback order: explicit
// config, then what the sidecar publishes, and only then issuer+/jwks.json.
// A sidecar whose jwks_uri sits away from its issuer is the case the derived
// default gets wrong, and every request 503s when it does.
func TestBuildVerifierPrefersPublishedJWKSURI(t *testing.T) {
	const publishedIssuer = "https://sentry.acme.example"

	var publishedJWKSURI string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != metadataPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"test-app","identity":{"issuer":%q,"jwks_uri":%q}}`,
			publishedIssuer, publishedJWKSURI)
	}))
	t.Cleanup(sidecar.Close)
	// The keys live on a different host from the issuer, which is exactly what
	// the derived default cannot express.
	publishedJWKSURI = newJWKSServer(t, newSigner(t).public).URL + "/keys/jwks"

	t.Setenv(envCatalystDaprHTTPPort, portOf(t, sidecar.URL))
	t.Setenv(envDaprHTTPPort, portOf(t, sidecar.URL))
	t.Setenv(envDaprHTTPEndpoint, "")
	t.Setenv(envSentryIssuer, "")

	v, err := BuildVerifier(context.Background(), OAuthConfig{})
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })

	if v.issuer != publishedIssuer {
		t.Errorf("issuer = %q, want %q", v.issuer, publishedIssuer)
	}
	if v.jwksURI != publishedJWKSURI {
		t.Errorf("jwksURI = %q, want the published %q (derived default would be %q)",
			v.jwksURI, publishedJWKSURI, defaultJWKSURI(publishedIssuer))
	}
}

// newJWKSServer serves set at /jwks.json on loopback, so a test that builds a
// real verifier warms against it rather than the public internet.
func newJWKSServer(t *testing.T, set jwk.Set) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(set); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestBuildVerifierWithoutCoordinatesFails(t *testing.T) {
	t.Setenv(envCatalystDaprHTTPPort, "")
	t.Setenv(envDaprHTTPPort, "")
	t.Setenv(envDaprHTTPEndpoint, "")
	t.Setenv(envSentryIssuer, "")

	_, err := BuildVerifier(context.Background(), OAuthConfig{})
	if err == nil {
		t.Fatal("want an error when no coordinates can be discovered, got nil")
	}
	// A caller has to be able to tell "nothing configured yet" apart from a
	// real failure, so the condition is a sentinel rather than a bare error.
	if !errors.Is(err, ErrIdentityNotConfigured) {
		t.Errorf("want errors.Is(err, ErrIdentityNotConfigured), got %v", err)
	}
	// Both ways to reach a sidecar are named, so a developer running against a
	// hosted one is not told only about the local port they do not have.
	for _, want := range []string{envDaprHTTPPort, envDaprHTTPEndpoint, envSentryIssuer} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %s; got %v", want, err)
		}
	}
}

// The JWKS is the whole root of trust, so an on-path attacker who rewrites a
// plaintext response mints tokens this package then accepts. These three cases
// are the whole policy: plaintext is refused, the local sidecar's loopback
// endpoint is exempt, and an app can opt out explicitly.
const insecureJWKSHost = "jwks.insecure.example"

func TestBuildVerifierRejectsInsecureJWKSURI(t *testing.T) {
	t.Setenv(envCatalystDaprHTTPPort, "")
	t.Setenv(envDaprHTTPPort, "")
	t.Setenv(envDaprHTTPEndpoint, "")
	t.Setenv(envSentryIssuer, "")

	insecure := "http://" + insecureJWKSHost + jwksPathSuffix
	v, err := BuildVerifier(context.Background(), OAuthConfig{
		Issuer:  testIssuer,
		JWKSURI: insecure,
	})
	if err == nil {
		_ = v.Close()
		t.Fatal("want a plaintext jwks uri refused, got a verifier")
	}
	if !errors.Is(err, ErrIdentityNotConfigured) {
		t.Errorf("want errors.Is(err, ErrIdentityNotConfigured), got %v", err)
	}
}

// TestBuildVerifierAllowsLoopbackInsecureJWKSURI: the local sidecar publishes
// its keys over http on loopback, where there is no path to be on.
func TestBuildVerifierAllowsLoopbackInsecureJWKSURI(t *testing.T) {
	jwks := newJWKSServer(t, newSigner(t).public)
	t.Setenv(envCatalystDaprHTTPPort, "")
	t.Setenv(envDaprHTTPPort, "")
	t.Setenv(envDaprHTTPEndpoint, "")
	t.Setenv(envSentryIssuer, "")

	// httptest serves on 127.0.0.1; assert that rather than assume it.
	if !isLoopbackHost(mustHostname(t, jwks.URL)) {
		t.Fatalf("jwks server %q is not on loopback", jwks.URL)
	}

	v, err := BuildVerifier(context.Background(), OAuthConfig{
		Issuer:  testIssuer,
		JWKSURI: jwks.URL + jwksPathSuffix,
	})
	if err != nil {
		t.Fatalf("want a loopback plaintext jwks uri allowed, got %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
}

// TestBuildVerifierAllowInsecureJWKSOptsIn covers the escape hatch itself: the
// same URI the case above refuses is accepted once the app says so.
func TestBuildVerifierAllowInsecureJWKSOptsIn(t *testing.T) {
	netGuard.expectBlockedHost(insecureJWKSHost)
	t.Setenv(envCatalystDaprHTTPPort, "")
	t.Setenv(envDaprHTTPPort, "")
	t.Setenv(envDaprHTTPEndpoint, "")
	t.Setenv(envSentryIssuer, "")

	insecure := "http://" + insecureJWKSHost + jwksPathSuffix
	v, err := BuildVerifier(context.Background(), OAuthConfig{
		Issuer:            testIssuer,
		JWKSURI:           insecure,
		AllowInsecureJWKS: true,
	})
	if err != nil {
		t.Fatalf("want the opt-in to accept a plaintext jwks uri, got %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })

	if v.jwksURI != insecure {
		t.Errorf("jwksURI = %q, want %q", v.jwksURI, insecure)
	}
}

// TestBuildVerifierRejectsInsecureDiscoveredJWKSURI: the check is on the
// resolved URI, not on what the app passed, so an issuer arriving over http
// from the environment is refused too.
func TestBuildVerifierRejectsInsecureDiscoveredJWKSURI(t *testing.T) {
	t.Setenv(envCatalystDaprHTTPPort, "")
	t.Setenv(envDaprHTTPPort, "")
	t.Setenv(envDaprHTTPEndpoint, "")
	t.Setenv(envSentryIssuer, "http://"+insecureJWKSHost)

	_, err := BuildVerifier(context.Background(), OAuthConfig{})
	if !errors.Is(err, ErrIdentityNotConfigured) {
		t.Fatalf("want errors.Is(err, ErrIdentityNotConfigured), got %v", err)
	}
}

func mustHostname(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return parsed.Hostname()
}

// discoverFromMetadataAt points discovery at a test server by setting the port
// env var the real lookup reads. The server must be on loopback, which
// httptest guarantees.
func discoverFromMetadataAt(t *testing.T, serverURL string) *identityCoordinates {
	t.Helper()
	t.Setenv(envCatalystDaprHTTPPort, portOf(t, serverURL))
	return discoverFromMetadata(context.Background())
}

func portOf(t *testing.T, serverURL string) string {
	t.Helper()
	for i := len(serverURL) - 1; i >= 0; i-- {
		if serverURL[i] == ':' {
			return serverURL[i+1:]
		}
	}
	t.Fatalf("no port in %q", serverURL)
	return ""
}

// testAPIToken stands in for the sidecar API token. It is asserted absent from
// every log line these tests capture, so it must be distinctive.
const testAPIToken = "diagrid://v1/org/prj/token"

// metadataServer is a loopback stand-in for a sidecar metadata endpoint that
// records the requests it received, so a test asserts on what actually
// arrived rather than on what the code under test believed it sent.
type metadataServer struct {
	*httptest.Server

	mu       sync.Mutex
	requests []*http.Request
}

// newMetadataServer serves body verbatim at metadataPath and 404s elsewhere.
func newMetadataServer(t *testing.T, body string) *metadataServer {
	t.Helper()
	sidecar := &metadataServer{}
	sidecar.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sidecar.mu.Lock()
		sidecar.requests = append(sidecar.requests, r.Clone(r.Context()))
		sidecar.mu.Unlock()

		if r.URL.Path != metadataPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(sidecar.Close)
	return sidecar
}

func (s *metadataServer) received() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.requests...)
}

// closedLoopbackURL is the URL of a loopback server that has stopped
// listening, so a request to it is refused without touching the network.
func closedLoopbackURL(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()
	return server.URL
}

// logBuffer is a mutex-guarded io.Writer, so a handler writing from any
// goroutine cannot race the test reading back what it wrote.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureWarnings redirects the default slog logger into a buffer for the
// duration of the test, so a test can assert on the warnings discovery emits —
// and on the ones it must not.
func captureWarnings(t *testing.T) *logBuffer {
	t.Helper()
	logs := &logBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

// TestBuildVerifierAllowInsecureJWKSRelaxesHTTPOnly pins what the opt-in means.
// Saying yes to plaintext http says nothing about loading signing keys off the
// local filesystem or over some other scheme entirely, so the flag widens the
// rule to http and stops there.
func TestBuildVerifierAllowInsecureJWKSRelaxesHTTPOnly(t *testing.T) {
	uris := []string{
		"file:///etc/diagrid/jwks.json",
		"ftp://" + insecureJWKSHost + jwksPathSuffix,
		"jwks.insecure.example/jwks.json", // no scheme at all
	}

	for _, jwksURI := range uris {
		t.Run(jwksURI, func(t *testing.T) {
			clearDiscoveryEnv(t)

			v, err := BuildVerifier(context.Background(), OAuthConfig{
				Issuer:            testIssuer,
				JWKSURI:           jwksURI,
				AllowInsecureJWKS: true,
			})
			if err == nil {
				_ = v.Close()
				t.Fatalf("want %q refused even with AllowInsecureJWKS, got a verifier", jwksURI)
			}
			if !errors.Is(err, ErrIdentityNotConfigured) {
				t.Errorf("want errors.Is(err, ErrIdentityNotConfigured), got %v", err)
			}
		})
	}
}

// TestBuildVerifierRejectsUnparseableJWKSURI: a JWKS URI that is not a URL is a
// configuration error, and the opt-in flag does not turn it into a usable one.
func TestBuildVerifierRejectsUnparseableJWKSURI(t *testing.T) {
	for _, allowInsecure := range []bool{false, true} {
		t.Run(fmt.Sprintf("allowInsecure=%v", allowInsecure), func(t *testing.T) {
			clearDiscoveryEnv(t)

			v, err := BuildVerifier(context.Background(), OAuthConfig{
				Issuer:            testIssuer,
				JWKSURI:           "http://[::1]:namedport/jwks.json",
				AllowInsecureJWKS: allowInsecure,
			})
			if err == nil {
				_ = v.Close()
				t.Fatal("want an unparseable jwks uri refused, got a verifier")
			}
			if !errors.Is(err, ErrIdentityNotConfigured) {
				t.Errorf("want errors.Is(err, ErrIdentityNotConfigured), got %v", err)
			}
		})
	}
}

// clearDiscoveryEnv unsets every discovery source, so a test asserting on what
// an explicit config resolves to is not reading a developer's environment.
func clearDiscoveryEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		envCatalystDaprHTTPPort, envDaprHTTPPort, envDaprHTTPEndpoint,
		envSentryIssuer, envSentryAudience,
	} {
		t.Setenv(name, "")
	}
}

// TestBuildVerifierWarnsOnWarmUpFailure: the warm-up error is not fatal — the
// next Verify retries the fetch — but discarding it silently leaves an app
// whose JWKS endpoint is unreachable with nothing in its log until a token
// arrives.
func TestBuildVerifierWarnsOnWarmUpFailure(t *testing.T) {
	clearDiscoveryEnv(t)
	jwksURI := closedLoopbackURL(t) + jwksPathSuffix
	logs := captureWarnings(t)

	v, err := BuildVerifier(context.Background(), OAuthConfig{
		Issuer:  testIssuer,
		JWKSURI: jwksURI,
	})
	if err != nil {
		t.Fatalf("a warm-up failure must not fail the build, got %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })

	got := logs.String()
	if !strings.Contains(got, jwksURI) {
		t.Errorf("warning must name the jwks endpoint %q; logs: %s", jwksURI, got)
	}
	if !strings.Contains(got, "warm") {
		t.Errorf("warning must say the warm-up failed; logs: %s", got)
	}
}

// TestBuildVerifierSilentWhenWarmUpSucceeds is the other half: a successful
// warm-up is the ordinary path and says nothing.
func TestBuildVerifierSilentWhenWarmUpSucceeds(t *testing.T) {
	clearDiscoveryEnv(t)
	jwks := newJWKSServer(t, newSigner(t).public)
	logs := captureWarnings(t)

	v, err := BuildVerifier(context.Background(), OAuthConfig{
		Issuer:  testIssuer,
		JWKSURI: jwks.URL + jwksPathSuffix,
	})
	if err != nil {
		t.Fatalf("BuildVerifier: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })

	if got := logs.String(); got != "" {
		t.Errorf("a successful warm-up must log nothing; logs: %s", got)
	}
}

// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jws"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

const (
	// clockSkew is the leeway allowed on time-based claims.
	clockSkew = 120 * time.Second

	// jwksCacheLifetime is how long fetched key material is served before a
	// background refresh.
	jwksCacheLifetime = 300 * time.Second

	// metadataTimeout bounds the sidecar discovery call.
	metadataTimeout = 5 * time.Second

	// jwksPathSuffix is appended to the issuer when no JWKS endpoint is given.
	jwksPathSuffix = "/jwks.json"

	// metadataPath is the sidecar endpoint carrying the identity block.
	metadataPath = "/v1.0/metadata"

	// metadataHost is the loopback address the sidecar listens on.
	metadataHost = "127.0.0.1"

	// apiTokenHeader carries the sidecar API token on a remote metadata
	// request. net/http canonicalises the spelling on the wire, where header
	// names are case-insensitive.
	apiTokenHeader = "dapr-api-token"

	// jwksForcedRefreshInterval rate-limits the refetch triggered by a token
	// naming an unknown key, so a stream of junk kid values cannot be turned
	// into a stream of requests to the JWKS endpoint.
	jwksForcedRefreshInterval = 30 * time.Second

	// schemeHTTPS is the only transport a JWKS endpoint may be fetched over,
	// loopback and OAuthConfig.AllowInsecureJWKS aside; both relax it to
	// schemeHTTP and to nothing else.
	schemeHTTPS = "https"
	schemeHTTP  = "http"

	// loopbackHostname is the name half of the loopback exemption; the address
	// half goes through net.IP.IsLoopback.
	loopbackHostname = "localhost"
)

// ErrIdentityNotConfigured reports that no usable identity coordinates could be
// resolved, so no verifier could be built: nothing was discovered, or what was
// discovered is not safe to fetch keys from. The middleware answers 503
// oauth.not_configured. Callers match it with [errors.Is].
var ErrIdentityNotConfigured = errors.New("identity: identity is not configured")

// errVerifierClosed is returned once [JWKSVerifier.Close] has stopped the
// background refresher, so a closed verifier fails closed rather than starting
// a second one.
var errVerifierClosed = errors.New("identity: verifier is closed")

// Environment variables consulted during discovery.
const (
	envCatalystDaprHTTPPort = "CATALYST_DAPR_HTTP_PORT"
	envDaprHTTPPort         = "DAPR_HTTP_PORT"
	envDaprHTTPEndpoint     = "DAPR_HTTP_ENDPOINT"
	envDaprAPIToken         = "DAPR_API_TOKEN"
	envSentryIssuer         = "DIAGRID_DP_SENTRY_ISSUER"
	envSentryAudience       = "DIAGRID_DP_SENTRY_AUDIENCE"
)

// Registered claims every token must carry.
const (
	claimExpiry  = "exp"
	claimIssuer  = "iss"
	claimSubject = "sub"
)

// allowedAlgorithms is the signing allowlist. A token signed with anything
// else — an HMAC algorithm that would accept the JWKS public key as a secret,
// or none — is rejected before its signature is ever checked.
var allowedAlgorithms = []jwa.SignatureAlgorithm{jwa.RS256, jwa.ES256}

// TokenVerifier verifies a raw inbound token and returns its claims.
//
// Implementations must fail closed: any doubt about the token is an error, not
// an empty claim set.
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (map[string]any, error)
}

// JWKSVerifier fetches JWKS from a public HTTPS endpoint, caches the keys, and
// verifies dp-Sentry JWTs against them. It is safe for concurrent use.
//
// The verifier owns a background refresher goroutine, which starts on the first
// key fetch and runs until [JWKSVerifier.Close] stops it. A verifier kept for
// the life of the process need never be closed, but anything building them
// repeatedly — a reconfiguration, a test, a per-tenant gate — must close the
// ones it discards, or the refreshers accumulate.
type JWKSVerifier struct {
	issuer   string
	jwksURI  string
	audience string

	mu     sync.RWMutex
	cache  *jwk.Cache
	cancel context.CancelFunc
	closed bool

	// refreshMu guards lastForcedRefresh alone, so the rate-limit check on a
	// kid miss never contends with an ordinary key lookup.
	refreshMu         sync.Mutex
	lastForcedRefresh time.Time

	// keys overrides JWKS fetching. Tests set it to serve a fixture key set
	// without standing up a JWKS server.
	keys func(ctx context.Context) (jwk.Set, error)

	// refreshKeys overrides the forced refetch on a kid miss, falling back to
	// keys when nil.
	refreshKeys func(ctx context.Context) (jwk.Set, error)
}

// NewJWKSVerifier returns a verifier for tokens from issuer, checked against
// the key set at jwksURI. An empty audience leaves the aud claim unchecked.
//
// The returned verifier starts a background JWKS refresher on its first key
// fetch. The caller owns it; see [JWKSVerifier.Close].
func NewJWKSVerifier(issuer, jwksURI, audience string) *JWKSVerifier {
	return &JWKSVerifier{issuer: issuer, jwksURI: jwksURI, audience: audience}
}

// Close stops the background JWKS refresher this verifier started on its first
// key fetch. It is safe to call more than once, and on a verifier that never
// fetched anything. A closed verifier fails every subsequent Verify with
// [ErrVerifierNotReady] rather than quietly starting a new refresher.
func (v *JWKSVerifier) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.closed = true
	if v.cancel != nil {
		v.cancel()
		v.cancel = nil
	}
	v.cache = nil
	return nil
}

// Warm fetches the JWKS eagerly so the first Verify does not block on it. Its
// error is informational: a failure here is retried on the next Verify.
func (v *JWKSVerifier) Warm(ctx context.Context) error {
	_, err := v.keySet(ctx)
	return err
}

// keySet returns the current key set, creating the refreshing cache on first
// use.
func (v *JWKSVerifier) keySet(ctx context.Context) (jwk.Set, error) {
	if v.keys != nil {
		return v.keys(ctx)
	}
	cache, err := v.jwksCache()
	if err != nil {
		return nil, err
	}
	set, err := cache.Get(ctx, v.jwksURI)
	if err != nil {
		return nil, fmt.Errorf("identity: fetch jwks %q: %w", v.jwksURI, err)
	}
	return set, nil
}

// jwksCache returns the refreshing cache, creating it on first use.
//
// The cache outlives any one request, so its refresher runs on a context this
// verifier owns and cancels in Close, rather than on the caller's.
func (v *JWKSVerifier) jwksCache() (*jwk.Cache, error) {
	v.mu.RLock()
	cache, closed := v.cache, v.closed
	v.mu.RUnlock()
	if closed {
		return nil, errVerifierClosed
	}
	if cache != nil {
		return cache, nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil, errVerifierClosed
	}
	if v.cache == nil {
		ctx, cancel := context.WithCancel(context.Background())
		fresh := jwk.NewCache(ctx)
		if err := fresh.Register(v.jwksURI, jwk.WithMinRefreshInterval(jwksCacheLifetime)); err != nil {
			cancel()
			return nil, fmt.Errorf("identity: register jwks %q: %w", v.jwksURI, err)
		}
		v.cache = fresh
		v.cancel = cancel
	}
	return v.cache, nil
}

// refreshKeySet forces a JWKS refetch, bypassing the cache lifetime, and
// returns a nil set when the rate limit declines to run one.
func (v *JWKSVerifier) refreshKeySet(ctx context.Context) (jwk.Set, error) {
	if !v.allowForcedRefresh() {
		return nil, nil
	}
	if v.refreshKeys != nil {
		return v.refreshKeys(ctx)
	}
	if v.keys != nil {
		return v.keys(ctx)
	}
	cache, err := v.jwksCache()
	if err != nil {
		return nil, err
	}
	set, err := cache.Refresh(ctx, v.jwksURI)
	if err != nil {
		return nil, fmt.Errorf("identity: refresh jwks %q: %w", v.jwksURI, err)
	}
	return set, nil
}

// allowForcedRefresh reports whether jwksForcedRefreshInterval has elapsed
// since the last forced refetch, recording this one when it has.
func (v *JWKSVerifier) allowForcedRefresh() bool {
	v.refreshMu.Lock()
	defer v.refreshMu.Unlock()
	now := time.Now()
	if !v.lastForcedRefresh.IsZero() && now.Sub(v.lastForcedRefresh) < jwksForcedRefreshInterval {
		return false
	}
	v.lastForcedRefresh = now
	return true
}

// Verify checks the signature and claims of rawToken and returns the decoded
// payload.
//
// It returns an error wrapping [ErrVerifierNotReady] when key material is
// unavailable, and a [*TokenVerificationError] for every way the token itself can
// fail.
func (v *JWKSVerifier) Verify(ctx context.Context, rawToken string) (map[string]any, error) {
	raw := []byte(rawToken)

	// Parse the structure first, so a malformed token reports a decode error
	// rather than looking like a signature failure.
	if _, err := jwt.ParseInsecure(raw); err != nil {
		return nil, tokenVerificationErrorf(ErrorCodeDecodeError, err)
	}

	headers, err := protectedHeaders(raw)
	if err != nil {
		return nil, tokenVerificationErrorf(ErrorCodeDecodeError, err)
	}
	if !algorithmAllowed(headers.Algorithm()) {
		return nil, tokenVerificationErrorf(ErrorCodeInvalidToken,
			fmt.Errorf("signing algorithm %q is not allowed", headers.Algorithm()))
	}

	set, err := v.keySet(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrVerifierNotReady, err)
	}
	key, err := v.signingKey(ctx, set, headers.KeyID())
	if err != nil {
		return nil, err
	}

	// The algorithm is pinned to the one the header declares and the allowlist
	// approved. Inferring it from the key type instead would accept any
	// algorithm that key happens to support, leaving the allowlist guarding a
	// claim nothing enforces.
	token, err := jwt.Parse(raw,
		jwt.WithKey(headers.Algorithm(), key),
		jwt.WithValidate(false),
	)
	if err != nil {
		return nil, tokenVerificationErrorf(ErrorCodeInvalidSignature, err)
	}

	if err := jwt.Validate(token, v.validateOptions()...); err != nil {
		return nil, tokenVerificationErrorf(codeForValidationError(err), err)
	}

	claims, err := token.AsMap(ctx)
	if err != nil {
		return nil, tokenVerificationErrorf(ErrorCodeDecodeError, err)
	}
	return claims, nil
}

// signingKey returns the JWKS key the token names.
//
// A kid the cached set does not hold usually means the key rotated since the
// last fetch, so one rate-limited refetch is tried first; without it every
// rotation is an auth outage lasting the full jwksCacheLifetime. A kid still
// unknown afterwards is a key-distribution problem rather than a bad token, so
// it reports [ErrVerifierNotReady] and the caller is not blamed.
func (v *JWKSVerifier) signingKey(ctx context.Context, set jwk.Set, kid string) (jwk.Key, error) {
	if key, found := set.LookupKeyID(kid); found {
		return key, nil
	}

	refreshed, err := v.refreshKeySet(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrVerifierNotReady, err)
	}
	if refreshed != nil {
		if key, found := refreshed.LookupKeyID(kid); found {
			return key, nil
		}
	}
	return nil, fmt.Errorf("%w: no signing key matches kid %q", ErrVerifierNotReady, kid)
}

// validateOptions builds the claim checks: the required claims and the clock
// skew always, the issuer and audience only when they are configured.
func (v *JWKSVerifier) validateOptions() []jwt.ValidateOption {
	opts := []jwt.ValidateOption{
		jwt.WithAcceptableSkew(clockSkew),
		jwt.WithRequiredClaim(claimExpiry),
		jwt.WithRequiredClaim(claimIssuer),
		jwt.WithRequiredClaim(claimSubject),
	}
	if v.issuer != "" {
		opts = append(opts, jwt.WithIssuer(v.issuer))
	}
	if v.audience != "" {
		opts = append(opts, jwt.WithAudience(v.audience))
	}
	return opts
}

// protectedHeaders returns the protected header of the token's single
// signature.
//
// A JWT is a JWS in compact serialization, which carries exactly one signature.
// A JSON serialization carrying several is rejected outright: the allowlist can
// vet only one of them while verification is free to accept a different one, so
// it would be guarding a signature nothing else checked.
func protectedHeaders(raw []byte) (jws.Headers, error) {
	msg, err := jws.Parse(raw)
	if err != nil {
		return nil, err
	}
	signatures := msg.Signatures()
	switch {
	case len(signatures) == 0:
		return nil, errors.New("token carries no signature")
	case len(signatures) > 1:
		return nil, fmt.Errorf("token carries %d signatures, want exactly 1", len(signatures))
	}
	return signatures[0].ProtectedHeaders(), nil
}

func algorithmAllowed(alg jwa.SignatureAlgorithm) bool {
	for _, allowed := range allowedAlgorithms {
		if alg == allowed {
			return true
		}
	}
	return false
}

// codeForValidationError maps a jwx claim failure onto the cross-SDK error
// code. Anything unrecognised is an invalid token rather than a server fault,
// because the claims did parse.
func codeForValidationError(err error) string {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired()):
		return ErrorCodeExpired
	case errors.Is(err, jwt.ErrInvalidIssuer()):
		return ErrorCodeInvalidIssuer
	case errors.Is(err, jwt.ErrInvalidAudience()):
		return ErrorCodeInvalidAudience
	default:
		return ErrorCodeInvalidToken
	}
}

// identityCoordinates is where tokens come from and how to check them.
type identityCoordinates struct {
	issuer   string
	jwksURI  string
	audience string
}

// BuildVerifier returns a verifier built from cfg, falling back to the sidecar
// metadata endpoint and then to environment variables for whatever cfg leaves
// unset. Priority: explicit config, the local /v1.0/metadata, the remote
// /v1.0/metadata, environment. The local sidecar is asked first so a deployed
// in-cluster app answers from loopback rather than a network round trip.
//
// It returns an error wrapping [ErrIdentityNotConfigured] when no issuer and
// JWKS endpoint can be resolved, or when the resolved JWKS endpoint is
// plaintext and neither on loopback nor permitted by
// OAuthConfig.AllowInsecureJWKS. The middleware reports either as
// oauth.not_configured.
//
// The caller owns the returned verifier and the background JWKS refresher it
// starts; see [JWKSVerifier.Close].
func BuildVerifier(ctx context.Context, cfg OAuthConfig) (*JWKSVerifier, error) {
	var discovered *identityCoordinates
	if cfg.Issuer == "" || cfg.JWKSURI == "" {
		discovered = discoverFromMetadata(ctx)
		if discovered == nil {
			discovered = discoverFromRemote(ctx)
		}
		if discovered == nil {
			discovered = discoverFromEnv()
		}
	}

	issuer := cfg.Issuer
	if issuer == "" && discovered != nil {
		issuer = discovered.issuer
	}
	// Explicit beats discovered beats derived: a sidecar publishing a jwks_uri
	// away from its issuer means it, and deriving issuer+/jwks.json ahead of
	// that would point at an endpoint which does not exist.
	//
	// The discovered jwks_uri is adopted only when it describes the issuer
	// actually being verified. Otherwise a config pinning one issuer while the
	// sidecar advertises another would check the pinned issuer against the
	// other one's keys, and a token minted by the advertised issuer but
	// claiming the pinned one would verify.
	jwksURI := cfg.JWKSURI
	if jwksURI == "" && discovered != nil && discovered.issuer == issuer {
		jwksURI = discovered.jwksURI
	}
	if jwksURI == "" && issuer != "" {
		jwksURI = defaultJWKSURI(issuer)
	}
	audience := cfg.Audience
	if audience == "" && discovered != nil {
		audience = discovered.audience
	}

	if issuer == "" || jwksURI == "" {
		return nil, fmt.Errorf("%w: cannot discover identity coordinates: "+
			"set Issuer and JWKSURI explicitly, configure the sidecar metadata endpoint "+
			"(%s locally or %s for a remote sidecar), or set %s",
			ErrIdentityNotConfigured, envDaprHTTPPort, envDaprHTTPEndpoint, envSentryIssuer)
	}
	if err := checkJWKSTransport(jwksURI, cfg.AllowInsecureJWKS); err != nil {
		return nil, err
	}

	verifier := NewJWKSVerifier(issuer, jwksURI, audience)
	// Not fatal — the next Verify retries the fetch — but worth reporting: an
	// unreachable JWKS endpoint otherwise stays silent until the first token
	// arrives and is answered with a 503.
	if err := verifier.Warm(ctx); err != nil {
		slog.Warn("identity jwks warm-up failed; the next verification retries the fetch",
			"jwksUri", jwksURI, "error", err)
	}
	return verifier, nil
}

// checkJWKSTransport refuses a resolved JWKS URI that is not fetched over
// https.
//
// The key set is the entire root of trust, so a plaintext JWKS endpoint is a
// complete authentication bypass rather than an eavesdropping risk: an on-path
// attacker who rewrites the response substitutes their own signing key and
// mints tokens this package accepts. Loopback is exempt because that is where
// the local sidecar publishes its keys and there is no path to be on.
//
// allowInsecure widens the rule to plain http and stops there: a file:// or
// otherwise non-http(s) URI stays refused with the flag set, and so does a URI
// that is not a URL at all. Hence the parse before the flag is consulted.
func checkJWKSTransport(jwksURI string, allowInsecure bool) error {
	parsed, err := url.Parse(jwksURI)
	if err != nil {
		return fmt.Errorf("%w: jwks uri %q is not a valid url: %w",
			ErrIdentityNotConfigured, jwksURI, err)
	}
	if parsed.Scheme == schemeHTTPS {
		return nil
	}
	if parsed.Scheme == schemeHTTP && (allowInsecure || isLoopbackHost(parsed.Hostname())) {
		return nil
	}
	return fmt.Errorf("%w: jwks uri %q must use https, because the key set is "+
		"the root of trust; set AllowInsecureJWKS to allow plain http",
		ErrIdentityNotConfigured, jwksURI)
}

// isLoopbackHost reports whether host is the loopback interface, by name or by
// address.
func isLoopbackHost(host string) bool {
	if host == loopbackHostname {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// defaultJWKSURI is where an issuer publishes its keys when it does not say.
func defaultJWKSURI(issuer string) string {
	return strings.TrimRight(issuer, "/") + jwksPathSuffix
}

// metadataResponse is the slice of the sidecar /v1.0/metadata body this package
// reads.
type metadataResponse struct {
	Identity struct {
		Issuer   string `json:"issuer"`
		JWKSURI  string `json:"jwks_uri"`
		Audience string `json:"audience"`
	} `json:"identity"`
}

// discoverFromMetadata asks the local sidecar for its identity block, and
// returns nil whenever that is unavailable or carries no issuer — discovery
// failing is ordinary, and the next source is tried.
func discoverFromMetadata(ctx context.Context) *identityCoordinates {
	port := firstEnv(envCatalystDaprHTTPPort, envDaprHTTPPort)
	if port == "" {
		return nil
	}
	return fetchCoordinates(ctx, fmt.Sprintf("http://%s:%s%s", metadataHost, port, metadataPath), "")
}

// discoverFromRemote asks the sidecar the project endpoint names, for the case
// where there is no local one to ask: an app running on a developer's machine
// against a hosted sidecar has nothing listening on loopback. An unset
// endpoint is an absent source rather than a failure, and says nothing.
func discoverFromRemote(ctx context.Context) *identityCoordinates {
	endpoint := strings.TrimRight(os.Getenv(envDaprHTTPEndpoint), "/")
	if endpoint == "" {
		return nil
	}

	apiToken := os.Getenv(envDaprAPIToken)
	if apiToken != "" && !strings.HasPrefix(endpoint, schemeHTTPS+"://") {
		// Warned about, not withheld: a self-hosted sidecar on plain http is a
		// valid setup, and refusing to talk to it would break it outright.
		slog.Warn("DAPR_API_TOKEN will be sent in clear text to a non-https sidecar endpoint",
			"env", envDaprAPIToken, "endpoint", endpoint)
	}
	return fetchCoordinates(ctx, endpoint+metadataPath, apiToken)
}

// fetchCoordinates GETs a sidecar metadata endpoint and returns the identity
// block it publishes, or nil when the endpoint cannot be reached, answers with
// something other than the expected body, or publishes no issuer. apiToken is
// sent as [apiTokenHeader] when non-empty, and the header is omitted when it
// is not.
func fetchCoordinates(ctx context.Context, metadataURL, apiToken string) *identityCoordinates {
	coords, err := requestCoordinates(ctx, metadataURL, apiToken)
	if err != nil {
		// A configured source that did not answer explains why the coordinates
		// the app ends up with came from somewhere else.
		slog.Warn("identity discovery failed; trying the next source",
			"url", metadataURL, "errorType", fmt.Sprintf("%T", err), "error", err)
		return nil
	}
	return coords
}

// requestCoordinates is the request half of [fetchCoordinates], split out so
// every way it can fail arrives at one place as an error.
func requestCoordinates(ctx context.Context, metadataURL, apiToken string) (*identityCoordinates, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return nil, err
	}
	if apiToken != "" {
		req.Header.Set(apiTokenHeader, apiToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("metadata endpoint answered %s", resp.Status)
	}

	var body metadataResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return coordsFromMetadata(body), nil
}

// coordsFromMetadata reads the identity block out of a metadata response body.
// A body carrying no issuer is no discovery rather than an error: the sidecar
// answered, it just has no identity to publish.
func coordsFromMetadata(body metadataResponse) *identityCoordinates {
	if body.Identity.Issuer == "" {
		return nil
	}
	jwksURI := body.Identity.JWKSURI
	if jwksURI == "" {
		jwksURI = defaultJWKSURI(body.Identity.Issuer)
	}
	return &identityCoordinates{
		issuer:   body.Identity.Issuer,
		jwksURI:  jwksURI,
		audience: body.Identity.Audience,
	}
}

// discoverFromEnv falls back to the environment, and returns nil when no
// issuer is set there.
func discoverFromEnv() *identityCoordinates {
	issuer := os.Getenv(envSentryIssuer)
	if issuer == "" {
		return nil
	}
	return &identityCoordinates{
		issuer:   issuer,
		jwksURI:  defaultJWKSURI(issuer),
		audience: os.Getenv(envSentryAudience),
	}
}

// firstEnv returns the value of the first of names that is set and non-empty.
func firstEnv(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

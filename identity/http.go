// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package identity

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

const (
	// Default ports for the two schemes, so origins that leave the port implied
	// compare equal to the ones that spell it out.
	httpDefaultPort  = "80"
	httpsDefaultPort = "443"

	// maxRedirectChainWalk bounds the walk back through the redirect chain, so
	// a malformed chain that loops cannot spin here. net/http stops after ten
	// hops by default; this leaves room for a client that raised the cap.
	maxRedirectChainWalk = 64
)

var _ http.RoundTripper = (*Transport)(nil)

// NewHTTPClient returns an [http.Client] that carries the calling user's
// identity on every outbound request, so an on-behalf-of call costs nothing
// beyond the client the app had to construct anyway:
//
//	client := identity.NewHTTPClient(nil)
//
//	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, mcpURL, nil)
//	resp, err := client.Do(req)
//
// The token is read from the request context at send time rather than baked in
// here. That is what makes one long-lived, shared client safe: concurrent
// requests each carry their own caller's token, where a token captured at
// construction would send whichever user happened to be current when the
// client was built.
//
// A request whose context carries no inbound caller — a scheduled, pub/sub or
// cron trigger — is not an error: it is sent unauthenticated, with the header
// omitted rather than sent empty.
//
// base supplies the transport, cookie jar, timeout and redirect policy; nil
// means a default client. It is copied rather than modified, and its transport
// is kept and wrapped, so a caller's own [http.RoundTripper] still runs. The
// result is a plain *http.Client, so it goes wherever one is expected.
//
// The token only ever goes to the origin the request addressed, because a
// redirect away from it drops the header. Past that the client is as wide as
// you make it, so call third-party APIs with a plain [http.Client] instead.
func NewHTTPClient(base *http.Client) *http.Client {
	client := &http.Client{}
	if base != nil {
		*client = *base
	}
	client.Transport = &Transport{Base: client.Transport}
	return client
}

// Transport is the [http.RoundTripper] behind [NewHTTPClient], exported on its
// own for an app that already owns a client it cannot replace:
//
//	owned.Transport = &identity.Transport{Base: owned.Transport}
//
// The origin guard travels with it, so that path is not the weaker one.
//
// It attaches the header and then delegates, so a Base that writes
// UserTokenHeader itself wins over identity; Go's RoundTripper composition
// offers no way to run underneath a transport the caller installed.
type Transport struct {
	// Base is the next RoundTripper. nil means [http.DefaultTransport].
	Base http.RoundTripper
}

// RoundTrip attaches the calling user's identity to a copy of req and sends it
// on through Base.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A RoundTripper must not modify the request it is given. Clearing the
	// header on the copy first means a request never carries an identity the
	// current context does not hold, whatever set it.
	clone := req.Clone(req.Context())
	clone.Header.Del(UserTokenHeader)

	for name, values := range identityHeadersFor(req) {
		clone.Header[name] = values
	}
	return t.base().RoundTrip(clone)
}

// base is the next RoundTripper, resolved at send time so a process that swaps
// [http.DefaultTransport] is honoured.
func (t *Transport) base() http.RoundTripper {
	if t.Base != nil {
		return t.Base
	}
	return http.DefaultTransport
}

// identityHeadersFor is the identity headers req may carry, and an empty set
// when it may carry none. Neither empty case is an error: a trigger with no
// inbound caller, and a request redirected off its original origin, both go
// out unauthenticated.
func identityHeadersFor(req *http.Request) http.Header {
	if !addressesOriginalOrigin(req) {
		slog.Debug("identity withheld: not the origin the caller addressed",
			"url", req.URL.Redacted())
		return nil
	}
	headers := outboundIdentityHeaders(req.Context())
	if len(headers) == 0 {
		slog.Debug("no inbound user context; calling unauthenticated",
			"url", req.URL.Redacted())
	}
	return headers
}

// addressesOriginalOrigin reports whether req still addresses the origin the
// caller asked for.
//
// net/http follows redirects above the round tripper, so this runs again on
// every hop, and on a cross-origin hop it strips only its own sensitive
// headers — UserTokenHeader is not one of them. Without this check a redirect
// from the callee would hand the caller's on-behalf-of token to whatever host
// the redirect names.
//
// A hop whose chain cannot be walked is treated as off-origin: withholding the
// token costs one unauthenticated request, attaching it leaks an identity.
func addressesOriginalOrigin(req *http.Request) bool {
	original, ok := originalRequestURL(req)
	if !ok {
		return false
	}
	return sameIdentityOrigin(original, req.URL)
}

// originalRequestURL walks back to the URL the app passed to [http.Client.Do].
//
// net/http records the redirect response that caused each hop on the request it
// builds for it, and a response names the request it answered, so the chain
// leads back to the first request, which has no such response. The second
// result is false when the chain breaks first.
func originalRequestURL(req *http.Request) (*url.URL, bool) {
	hop := req
	for range maxRedirectChainWalk {
		if hop.Response == nil {
			return hop.URL, hop.URL != nil
		}
		if hop.Response.Request == nil {
			return nil, false
		}
		hop = hop.Response.Request
	}
	return nil, false
}

// sameIdentityOrigin reports whether a token addressed to original may be sent
// to target: scheme, host and port must match, the one exception being a
// same-host upgrade from http on port 80 to https on port 443, which hands the
// token to the same server over a better transport.
func sameIdentityOrigin(original, target *url.URL) bool {
	if !strings.EqualFold(original.Hostname(), target.Hostname()) {
		return false
	}
	if original.Scheme == target.Scheme && originPort(original) == originPort(target) {
		return true
	}
	return original.Scheme == schemeHTTP && originPort(original) == httpDefaultPort &&
		target.Scheme == schemeHTTPS && originPort(target) == httpsDefaultPort
}

// originPort is the port a URL addresses, filled in from the scheme when the
// URL leaves it implied.
func originPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if u.Scheme == schemeHTTPS {
		return httpsDefaultPort
	}
	return httpDefaultPort
}

// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package identity

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// headerSink is a test server that records the identity header each request
// arrived with, keyed by path, so two callers driven through one client can be
// told apart. A path never requested reads back nil, which is how a test
// distinguishes an absent header from an empty one.
type headerSink struct {
	mu   sync.Mutex
	seen map[string][]string
	hits map[string]int
}

func newHeaderSink() *headerSink {
	return &headerSink{seen: map[string][]string{}, hits: map[string]int{}}
}

func (s *headerSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.seen[r.URL.Path] = r.Header.Values(UserTokenHeader)
	s.hits[r.URL.Path]++
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (s *headerSink) header(path string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[path]
}

func (s *headerSink) hitCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

// drive makes one GET through client with token on the request context, an
// empty token standing for a trigger with no inbound caller.
func drive(t *testing.T, client *http.Client, url, token string) {
	t.Helper()
	ctx := context.Background()
	if token != "" {
		ctx = ContextWithToken(ctx, token)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Errorf("new request %s: %v", url, err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Errorf("GET %s: %v", url, err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET %s status = %d, want %d", url, resp.StatusCode, http.StatusOK)
	}
}

// TestNewHTTPClientReadsTokenAtSendTime covers the property that makes a single
// long-lived client safe: the token is read from the request context at send
// time, where one captured at construction would send whichever user was
// current when the client was built.
func TestNewHTTPClientReadsTokenAtSendTime(t *testing.T) {
	sink := newHeaderSink()
	server := httptest.NewServer(sink)
	defer server.Close()

	client := NewHTTPClient(nil)

	callers := []struct{ path, token string }{
		{"/alice", "alice-token"},
		{"/bob", "bob-token"},
	}
	var wg sync.WaitGroup
	for _, caller := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			drive(t, client, server.URL+caller.path, caller.token)
		}()
	}
	wg.Wait()

	for _, caller := range callers {
		got := sink.header(caller.path)
		want := BearerPrefix + caller.token
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s on %s = %v, want [%q]", UserTokenHeader, caller.path, got, want)
		}
	}
}

// TestNewHTTPClientOmitsHeaderWithoutContext: a scheduled, pub/sub or cron
// trigger has no caller, so the call proceeds unauthenticated with the header
// omitted rather than sent empty, and without an error.
func TestNewHTTPClientOmitsHeaderWithoutContext(t *testing.T) {
	sink := newHeaderSink()
	server := httptest.NewServer(sink)
	defer server.Close()

	drive(t, NewHTTPClient(nil), server.URL+"/cron", "")

	if got := sink.header("/cron"); got != nil {
		t.Errorf("%s = %v, want the header absent", UserTokenHeader, got)
	}
}

// TestNewHTTPClientClearsStaleHeader: the header is cleared before identity is
// applied, so a request never carries an identity the current context does not
// hold, whatever set it before.
func TestNewHTTPClientClearsStaleHeader(t *testing.T) {
	sink := newHeaderSink()
	server := httptest.NewServer(sink)
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/stale", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(UserTokenHeader, BearerPrefix+"someone-elses-token")

	resp, err := NewHTTPClient(nil).Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if got := sink.header("/stale"); got != nil {
		t.Errorf("%s = %v, want the stale header cleared", UserTokenHeader, got)
	}
}

// TestNewHTTPClientDropsHeaderOffOrigin: the token goes only to the origin the
// caller addressed. A redirect to a different origin drops it — without that,
// a redirect from the callee hands the caller's on-behalf-of token to whatever
// host the redirect names.
func TestNewHTTPClientDropsHeaderOffOrigin(t *testing.T) {
	elsewhere := newHeaderSink()
	other := httptest.NewServer(elsewhere)
	defer other.Close()

	addressed := newHeaderSink()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addressed.ServeHTTP(httptest.NewRecorder(), r)
		http.Redirect(w, r, other.URL+"/elsewhere", http.StatusFound)
	}))
	defer origin.Close()

	drive(t, NewHTTPClient(nil), origin.URL+"/addressed", "caller-token")

	if got, want := addressed.header("/addressed"), BearerPrefix+"caller-token"; len(got) != 1 || got[0] != want {
		t.Errorf("%s on the addressed origin = %v, want [%q]", UserTokenHeader, got, want)
	}
	if elsewhere.hitCount("/elsewhere") != 1 {
		t.Fatalf("the redirect target was hit %d times, want 1", elsewhere.hitCount("/elsewhere"))
	}
	if got := elsewhere.header("/elsewhere"); got != nil {
		t.Errorf("%s leaked to the redirect target: %v", UserTokenHeader, got)
	}
}

// TestNewHTTPClientKeepsHeaderOnSameOriginRedirect: the guard drops the header
// only when the hop leaves the origin, so an ordinary same-origin redirect
// still carries the caller.
func TestNewHTTPClientKeepsHeaderOnSameOriginRedirect(t *testing.T) {
	sink := newHeaderSink()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved" {
			http.Redirect(w, r, "/here", http.StatusFound)
			return
		}
		sink.ServeHTTP(w, r)
	}))
	defer server.Close()

	drive(t, NewHTTPClient(nil), server.URL+"/moved", "caller-token")

	if got, want := sink.header("/here"), BearerPrefix+"caller-token"; len(got) != 1 || got[0] != want {
		t.Errorf("%s after a same-origin redirect = %v, want [%q]", UserTokenHeader, got, want)
	}
}

// recordingTransport is a caller-supplied RoundTripper, so a test can prove
// NewHTTPClient kept it rather than replacing it.
type recordingTransport struct {
	calls  int
	header string
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.calls++
	rt.header = req.Header.Get(UserTokenHeader)
	return http.DefaultTransport.RoundTrip(req)
}

// TestNewHTTPClientPreservesBaseClient: a caller's own transport and settings
// survive, identity is applied on top of them, and the client the caller
// handed in is not mutated.
func TestNewHTTPClientPreservesBaseClient(t *testing.T) {
	sink := newHeaderSink()
	server := httptest.NewServer(sink)
	defer server.Close()

	recorder := &recordingTransport{}
	base := &http.Client{Transport: recorder, Timeout: verifierBuildTimeout}

	client := NewHTTPClient(base)
	drive(t, client, server.URL+"/based", "caller-token")

	if recorder.calls != 1 {
		t.Errorf("base transport called %d times, want 1", recorder.calls)
	}
	if want := BearerPrefix + "caller-token"; recorder.header != want {
		t.Errorf("base transport saw %s = %q, want %q", UserTokenHeader, recorder.header, want)
	}
	if client.Timeout != base.Timeout {
		t.Errorf("client.Timeout = %v, want the base's %v", client.Timeout, base.Timeout)
	}
	if base.Transport != http.RoundTripper(recorder) {
		t.Error("the base client's transport was replaced; it must not be mutated")
	}
	if client == base {
		t.Error("want a new client, not the base one")
	}
}

// TestTransportOnCallerOwnedClient: an app that already owns a client it cannot
// replace installs the Transport alone, and gets both the identity header and
// the origin guard through that path.
func TestTransportOnCallerOwnedClient(t *testing.T) {
	elsewhere := newHeaderSink()
	other := httptest.NewServer(elsewhere)
	defer other.Close()

	addressed := newHeaderSink()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addressed.ServeHTTP(httptest.NewRecorder(), r)
		http.Redirect(w, r, other.URL+"/elsewhere", http.StatusFound)
	}))
	defer origin.Close()

	owned := &http.Client{Transport: &Transport{}}
	drive(t, owned, origin.URL+"/addressed", "caller-token")

	if got, want := addressed.header("/addressed"), BearerPrefix+"caller-token"; len(got) != 1 || got[0] != want {
		t.Errorf("%s on the addressed origin = %v, want [%q]", UserTokenHeader, got, want)
	}
	if got := elsewhere.header("/elsewhere"); got != nil {
		t.Errorf("%s leaked to the redirect target: %v", UserTokenHeader, got)
	}
}

// TestTransportDoesNotMutateRequest: a RoundTripper must not modify the request
// it is given, so the identity header goes on a clone.
func TestTransportDoesNotMutateRequest(t *testing.T) {
	sink := newHeaderSink()
	server := httptest.NewServer(sink)
	defer server.Close()

	ctx := ContextWithToken(context.Background(), "caller-token")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/clone", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := (&Transport{}).RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if got := req.Header.Get(UserTokenHeader); got != "" {
		t.Errorf("the request passed to RoundTrip was mutated: %s = %q", UserTokenHeader, got)
	}
	if got, want := sink.header("/clone"), BearerPrefix+"caller-token"; len(got) != 1 || got[0] != want {
		t.Errorf("%s = %v, want [%q]", UserTokenHeader, got, want)
	}
}

// TestSameIdentityOrigin covers the origin rule directly, including the one
// exception a redirect test on loopback cannot reach: a same-host upgrade from
// http on port 80 to https on port 443 is allowed, every other change of
// scheme, host or port is not.
func TestSameIdentityOrigin(t *testing.T) {
	cases := []struct {
		name             string
		original, target string
		want             bool
	}{
		{"identical", "https://mcp.example.com/a", "https://mcp.example.com/b", true},
		{"implied port matches explicit", "https://mcp.example.com/a", "https://mcp.example.com:443/b", true},
		{"host case is insignificant", "https://MCP.example.com/a", "https://mcp.example.com/b", true},
		{"upgrade to https on the default ports", "http://mcp.example.com/a", "https://mcp.example.com/b", true},
		{"upgrade from a non-default port", "http://mcp.example.com:8080/a", "https://mcp.example.com/b", false},
		{"downgrade to http", "https://mcp.example.com/a", "http://mcp.example.com/b", false},
		{"another port", "https://mcp.example.com/a", "https://mcp.example.com:8443/b", false},
		{"another host", "https://mcp.example.com/a", "https://attacker.example.com/b", false},
		{"a subdomain is another host", "https://mcp.example.com/a", "https://mcp.example.com.evil.test/b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original, err := url.Parse(tc.original)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.original, err)
			}
			target, err := url.Parse(tc.target)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.target, err)
			}
			if got := sameIdentityOrigin(original, target); got != tc.want {
				t.Errorf("sameIdentityOrigin(%q, %q) = %v, want %v",
					tc.original, tc.target, got, tc.want)
			}
		})
	}
}

// TestIdentityHeadersWithheldOnUnwalkableChain: a request that claims to be a
// redirect hop but whose chain does not lead back to a first request carries no
// identity. Withholding the token from a call that would have been allowed
// costs one unauthenticated request; attaching it to a call that should not
// have it leaks an identity, so the broken case fails closed.
func TestIdentityHeadersWithheldOnUnwalkableChain(t *testing.T) {
	ctx := ContextWithToken(context.Background(), "caller-token")
	newReq := func(t *testing.T) *http.Request {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:1/hop", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		return req
	}

	t.Run("chain ends without a request", func(t *testing.T) {
		req := newReq(t)
		req.Response = &http.Response{}
		if got := identityHeadersFor(req); len(got) != 0 {
			t.Errorf("headers = %v, want none", got)
		}
	})

	t.Run("chain cycles", func(t *testing.T) {
		first, second := newReq(t), newReq(t)
		first.Response = &http.Response{Request: second}
		second.Response = &http.Response{Request: first}
		if got := identityHeadersFor(first); len(got) != 0 {
			t.Errorf("headers = %v, want none", got)
		}
	})

	t.Run("an ordinary first request still carries the caller", func(t *testing.T) {
		got := identityHeadersFor(newReq(t))
		if want := BearerPrefix + "caller-token"; got.Get(UserTokenHeader) != want {
			t.Errorf("%s = %q, want %q", UserTokenHeader, got.Get(UserTokenHeader), want)
		}
	})
}

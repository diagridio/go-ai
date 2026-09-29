// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package goai

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestMain defaults usageEndpoint to "" (no-op) for the whole binary. Every
// test that needs to send an event must opt back in explicitly with
// useEndpoint, so a test that forgets to set it up cannot reach the real
// Scarf endpoint.
func TestMain(m *testing.M) {
	usageEndpoint = ""
	os.Exit(m.Run())
}

// allAnalyticsEnvVars are every environment variable this file reads.
var allAnalyticsEnvVars = func() []string {
	var names []string
	names = append(names, optOutEnvVars...)
	names = append(names, ciTruthyEnvVars...)
	names = append(names, ciPresenceEnvVars...)
	names = append(names, daprEndpointEnvVars...)
	names = append(names, "DAPR_API_TOKEN")
	return names
}()

// setupTest gives each test an unreported guard and a clean environment: off
// CI, no opt-out, no Dapr endpoint or token set. usageEndpoint itself is left
// at whatever TestMain (or the previous test's cleanup) restored it to,
// currently "".
func setupTest(t *testing.T) {
	t.Helper()
	for _, name := range allAnalyticsEnvVars {
		t.Setenv(name, "")
	}
	resetReportedGuard()
	t.Cleanup(resetReportedGuard)
}

func resetReportedGuard() {
	reportedMu.Lock()
	defer reportedMu.Unlock()
	reportedPackages = map[string]bool{}
}

// useEndpoint points usageEndpoint at endpoint for the duration of the test,
// restoring the previous value afterward.
func useEndpoint(t *testing.T, endpoint string) {
	t.Helper()
	original := usageEndpoint
	usageEndpoint = endpoint
	t.Cleanup(func() { usageEndpoint = original })
}

type capturedRequest struct {
	query     url.Values
	userAgent string
}

// newCapturingServer starts a server that records each request's query
// string and User-Agent header, then answers 200. The channel is buffered so
// the handler never blocks on a test that only wants to assert no event
// arrived.
func newCapturingServer(t *testing.T) (*httptest.Server, <-chan capturedRequest) {
	t.Helper()
	captured := make(chan capturedRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- capturedRequest{query: r.URL.Query(), userAgent: r.Header.Get("User-Agent")}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server, captured
}

// waitForRequest waits for one captured request, failing the test after a
// generous timeout rather than hanging CI on a regression that stops sending.
func waitForRequest(t *testing.T, ch <-chan capturedRequest) capturedRequest {
	t.Helper()
	select {
	case req := <-ch:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a usage event")
		return capturedRequest{}
	}
}

// assertNoRequest waits a short interval and fails if a request arrives. The
// interval is short on purpose: this only needs to be longer than the async
// send takes to reach a local httptest server, not a real network round trip.
func assertNoRequest(t *testing.T, ch <-chan capturedRequest) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("unexpected usage event sent")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestReportUsage_QueryParametersAndUserAgent(t *testing.T) {
	setupTest(t)
	server, requests := newCapturingServer(t)
	useEndpoint(t, server.URL)

	reportUsage("go-ai", map[string]string{
		"kind":              "agent",
		"framework":         "langchaingo",
		"framework_version": "v1.2.3",
	})

	req := waitForRequest(t, requests)

	want := map[string]string{
		"package":           "go-ai",
		"kind":              "agent",
		"framework":         "langchaingo",
		"framework_version": "v1.2.3",
		"ci":                "false",
		"target":            "dapr",
	}
	for k, v := range want {
		if got := req.query.Get(k); got != v {
			t.Errorf("query[%q] = %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"version", "os", "arch", "go_version"} {
		if req.query.Get(k) == "" {
			t.Errorf("query[%q] is empty, want non-empty", k)
		}
	}
	if !strings.HasPrefix(req.userAgent, "go-ai/") {
		t.Errorf("User-Agent = %q, want prefix %q", req.userAgent, "go-ai/")
	}
}

func TestReportUsage_OncePerPackagePerProcess(t *testing.T) {
	setupTest(t)
	server, requests := newCapturingServer(t)
	useEndpoint(t, server.URL)

	reportUsage("go-ai", nil)
	waitForRequest(t, requests)

	// A second call for the same package sends nothing more.
	reportUsage("go-ai", nil)
	assertNoRequest(t, requests)

	// A distinct package still gets its own event.
	reportUsage("go-ai-other", nil)
	waitForRequest(t, requests)
}

func TestReportUsage_EmptyEndpointSendsNothing(t *testing.T) {
	setupTest(t)
	_, requests := newCapturingServer(t) // server exists; usageEndpoint is not pointed at it
	useEndpoint(t, "")

	reportUsage("go-ai-empty-endpoint", map[string]string{"kind": "agent"})
	assertNoRequest(t, requests)

	// Not marked as reported either, so a later call with an endpoint still
	// fires.
	reportedMu.Lock()
	reported := reportedPackages["go-ai-empty-endpoint"]
	reportedMu.Unlock()
	if reported {
		t.Error("package marked as reported while usageEndpoint is empty")
	}
}

func TestReportUsage_NoEventSentWhenOptedOut(t *testing.T) {
	setupTest(t)
	server, requests := newCapturingServer(t)
	useEndpoint(t, server.URL)
	t.Setenv("DO_NOT_TRACK", "1")

	reportUsage("go-ai-opted-out", nil)
	assertNoRequest(t, requests)
}

func TestReportUsage_DoesNotBlockOnSlowServer(t *testing.T) {
	setupTest(t)
	release := make(chan struct{})
	captured := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the response so the caller would block on a real network
		captured <- capturedRequest{query: r.URL.Query(), userAgent: r.Header.Get("User-Agent")}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	useEndpoint(t, server.URL)

	start := time.Now()
	reportUsage("go-ai-slow", nil)
	elapsed := time.Since(start)

	if elapsed >= 100*time.Millisecond {
		t.Fatalf("reportUsage blocked for %s, want < 100ms", elapsed)
	}

	// Let the handler respond and wait for it before this test returns. The
	// background goroutine reportUsage started reads the package-level
	// usageEndpoint once, at the very start of building the request, which by
	// now has already happened; without this wait that goroutine could still
	// be doing so concurrently with a later test's useEndpoint call on the
	// same var, which is what a -race run would (correctly) flag.
	close(release)
	waitForRequest(t, captured)
}

func TestSendUsageEvent_UnreachableEndpointDoesNotPanic(t *testing.T) {
	setupTest(t)
	// Port 1 is reserved; nothing listens there, so the connection is refused
	// immediately without leaving the loopback interface.
	useEndpoint(t, "http://127.0.0.1:1")

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("sendUsageEvent panicked: %v", r)
		}
	}()
	sendUsageEvent("go-ai-unreachable", nil)
}

func TestSendUsageEvent_InvalidURLDoesNotPanic(t *testing.T) {
	setupTest(t)
	// A raw control character makes net/url reject the URL outright.
	useEndpoint(t, "http://example.invalid/\x7f")

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("sendUsageEvent panicked: %v", r)
		}
	}()
	sendUsageEvent("go-ai-invalid-url", nil)
}

func TestBuildUsageURL_CallerDimensionsOverrideDefaultsAndEmptyOnesAreDropped(t *testing.T) {
	setupTest(t)

	got := buildUsageURL("go-ai", "0.5.0", map[string]string{
		"target":    "catalyst",
		"framework": "",
		"kind":      "",
	})

	values, err := url.ParseQuery(strings.TrimPrefix(got, "?"))
	if err != nil {
		t.Fatalf("parse query from %q: %v", got, err)
	}
	if v := values.Get("target"); v != "catalyst" {
		t.Errorf("target = %q, want %q", v, "catalyst")
	}
	if values.Has("framework") {
		t.Error("framework should be dropped when the caller passes an empty value")
	}
	if values.Has("kind") {
		t.Error("kind should be dropped when the caller passes an empty value")
	}
	if v := values.Get("package"); v != "go-ai" {
		t.Errorf("package = %q, want %q", v, "go-ai")
	}
	if v := values.Get("version"); v != "0.5.0" {
		t.Errorf("version = %q, want %q", v, "0.5.0")
	}
}

func TestCleanDimension_BoundsAt64Bytes(t *testing.T) {
	long := "  " + strings.Repeat("x", 200) + "  "
	got := cleanDimension(long)
	if len(got) != dimensionMaxLen {
		t.Fatalf("len(got) = %d, want %d", len(got), dimensionMaxLen)
	}
	if got != strings.Repeat("x", dimensionMaxLen) {
		t.Fatalf("got = %q", got)
	}
}

func TestCleanDimension_DoesNotSplitAMultiByteRune(t *testing.T) {
	// "é" is 2 bytes in UTF-8. Prefixing with one ASCII byte makes byte offset
	// 64 land in the middle of the 32nd "é", which is exactly the case the
	// rune-safe trim exists for.
	long := "a" + strings.Repeat("é", 40)

	got := cleanDimension(long)

	if !utf8.ValidString(got) {
		t.Fatalf("cleanDimension produced invalid UTF-8: %q", got)
	}
	if len(got) > dimensionMaxLen {
		t.Fatalf("len(got) = %d, want <= %d", len(got), dimensionMaxLen)
	}
	want := "a" + strings.Repeat("é", 31) // 1 + 31*2 = 63 bytes; a 32nd rune would be 65
	if got != want {
		t.Fatalf("got = %q, want %q", got, want)
	}
}

func TestUsageReportingDisabled_NoOptOutSet(t *testing.T) {
	setupTest(t)
	if usageReportingDisabled() {
		t.Error("usageReportingDisabled() = true, want false with nothing set")
	}
}

func TestUsageReportingDisabled_EachOptOutVariable(t *testing.T) {
	for _, name := range optOutEnvVars {
		t.Run(name, func(t *testing.T) {
			setupTest(t)
			t.Setenv(name, "1")
			if !usageReportingDisabled() {
				t.Errorf("%s=1 should disable reporting", name)
			}
		})
	}
}

func TestUsageReportingDisabled_FalsyValuesDoNotDisable(t *testing.T) {
	for _, value := range []string{"0", "false", "no", "off", ""} {
		t.Run(value, func(t *testing.T) {
			setupTest(t)
			t.Setenv("DO_NOT_TRACK", value)
			if usageReportingDisabled() {
				t.Errorf("DO_NOT_TRACK=%q should not disable reporting", value)
			}
		})
	}
}

func TestUsageReportingDisabled_TruthyValuesAreCaseAndSpaceInsensitive(t *testing.T) {
	for _, value := range []string{"1", "TRUE", " yes ", "On"} {
		t.Run(value, func(t *testing.T) {
			setupTest(t)
			t.Setenv("DO_NOT_TRACK", value)
			if !usageReportingDisabled() {
				t.Errorf("DO_NOT_TRACK=%q should disable reporting", value)
			}
		})
	}
}

func TestDetectTarget(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no env set", nil, "dapr"},
		{"http endpoint localhost", map[string]string{"DAPR_HTTP_ENDPOINT": "http://localhost:3500"}, "dapr"},
		{"grpc endpoint under diagrid.io", map[string]string{"DAPR_GRPC_ENDPOINT": "https://grpc-prj1.api.cloud.diagrid.io:443"}, "catalyst"},
		{"http endpoint under diagrid.io", map[string]string{"DAPR_HTTP_ENDPOINT": "https://http-prj1.api.cloud.diagrid.io"}, "catalyst"},
		{"grpc endpoint not diagrid.io", map[string]string{"DAPR_GRPC_ENDPOINT": "https://notdiagrid.io:443"}, "dapr"},
		{"grpc endpoint without a scheme still resolves", map[string]string{"DAPR_GRPC_ENDPOINT": "grpc-prj1.api.cloud.diagrid.io:443"}, "catalyst"},
		{"api token set", map[string]string{"DAPR_API_TOKEN": "diagrid://abc"}, "catalyst"},
		{"api token blank", map[string]string{"DAPR_API_TOKEN": "   "}, "dapr"},
		{"invalid endpoint URL falls through", map[string]string{"DAPR_GRPC_ENDPOINT": "http://\x7f"}, "dapr"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setupTest(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := detectTarget(); got != tc.want {
				t.Errorf("detectTarget() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunningInCI(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"no env set", nil, false},
		{"CI true", map[string]string{"CI": "true"}, true},
		{"CI falsy zero", map[string]string{"CI": "0"}, false},
		{"GITHUB_ACTIONS true", map[string]string{"GITHUB_ACTIONS": "true"}, true},
		{"GITLAB_CI true", map[string]string{"GITLAB_CI": "true"}, true},
		{"CIRCLECI true", map[string]string{"CIRCLECI": "true"}, true},
		{"TRAVIS true", map[string]string{"TRAVIS": "true"}, true},
		{"TF_BUILD True", map[string]string{"TF_BUILD": "True"}, true},
		{"BUILDKITE present", map[string]string{"BUILDKITE": "true"}, true},
		{"JENKINS_URL present", map[string]string{"JENKINS_URL": "https://ci.example.invalid/"}, true},
		{"JENKINS_URL blank does not count", map[string]string{"JENKINS_URL": "   "}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setupTest(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := runningInCI(); got != tc.want {
				t.Errorf("runningInCI() = %v, want %v", got, tc.want)
			}

			wantCI := "ci=false"
			if tc.want {
				wantCI = "ci=true"
			}
			useEndpoint(t, "https://example.invalid/go-ai")
			if u := buildUsageURL("go-ai", "0.5.0", nil); !strings.Contains(u, wantCI) {
				t.Errorf("buildUsageURL() = %q, want it to contain %q", u, wantCI)
			}
		})
	}
}

func TestUnknownEndpointHostnameDoesNotPanic(t *testing.T) {
	if _, ok := endpointHostname("http://\x7f"); ok {
		t.Error("endpointHostname should report ok=false for an unparsable URL")
	}
}

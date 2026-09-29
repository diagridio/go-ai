// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package goai

// Anonymous usage reporting.
//
// The Go module proxy publishes no download counts at all, so this reports one
// event per package per process with the package version and the host
// platform. No application data is collected. See the "Usage analytics" section of the
// README, including how to opt out.
//
// One event per process means one event per replica per restart on
// Kubernetes. The numbers count process starts, not deployments or users.
//
// reportUsage never blocks the caller and never panics or returns an error:
// the send runs on its own goroutine, guarded by a deferred recover, and
// every failure is swallowed. http.Client's Timeout bounds the whole round
// trip — connect, TLS, request and response — not just the socket read, so
// unlike a language where the timeout only covers a blocking read, DNS
// resolution that never returns cannot outlive it. Blocked egress and
// air-gapped clusters are normal conditions, not faults.
//
// Set usageEndpoint to an empty string to turn this file into a no-op. There
// is no public API for that: tests reach the package-level var directly.

import (
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// usageEndpoint is the Scarf event-collection route for go-ai. The route
// records the request and redirects nowhere. An empty string disables
// reporting entirely. Tests point this at an httptest server or blank it;
// there is no other way to change it.
var usageEndpoint = "https://diagrid.gateway.scarf.sh/go-ai"

// usageTimeout bounds the entire HTTP round trip for a usage event.
const usageTimeout = time.Second

// goAIModulePath identifies this module in build info, so its own version can
// be read from runtime/debug.ReadBuildInfo when go-ai is imported by an
// application rather than run as the main module.
const goAIModulePath = "github.com/diagridio/go-ai"

// dimensionMaxLen bounds every query-string value.
const dimensionMaxLen = 64

// optOutEnvVars are the cross-ecosystem DO_NOT_TRACK convention, Scarf's own
// variable, and a Diagrid-specific opt-out. Any of them set to a truthy value
// disables reporting.
var optOutEnvVars = []string{"DO_NOT_TRACK", "SCARF_NO_ANALYTICS", "DIAGRID_NO_ANALYTICS"}

// truthyValues are the trimmed, lower-cased values that count as "on".
var truthyValues = map[string]bool{"1": true, "true": true, "yes": true, "on": true}

// ciTruthyEnvVars follow the CI convention most vendors set: a flag that must
// itself be truthy.
var ciTruthyEnvVars = []string{"CI", "GITHUB_ACTIONS", "GITLAB_CI", "CIRCLECI", "TRAVIS", "TF_BUILD"}

// ciPresenceEnvVars are vendors that set a value rather than a boolean flag.
// Presence (a non-empty, trimmed value) is enough.
var ciPresenceEnvVars = []string{"BUILDKITE", "JENKINS_URL"}

// daprEndpointEnvVars are the Dapr SDK variables that point a process at
// Catalyst.
var daprEndpointEnvVars = []string{"DAPR_GRPC_ENDPOINT", "DAPR_HTTP_ENDPOINT"}

// catalystHostSuffix is the host, or host suffix, that marks a Dapr endpoint
// as Catalyst rather than a self-hosted sidecar.
const catalystHostSuffix = "diagrid.io"

var (
	reportedMu       sync.Mutex
	reportedPackages = map[string]bool{}
)

// reportUsage sends one anonymous usage event for pkg, once per package per
// process, from a background goroutine. It never blocks the caller and never
// panics or returns an error: every failure, including a panic anywhere in
// this call, is swallowed. Does nothing while usageEndpoint is empty or when
// the user opted out.
//
// dimensions are extra query parameters such as kind and framework. A
// dimension overrides the default of the same name; an empty one is dropped.
func reportUsage(pkg string, dimensions map[string]string) {
	defer func() {
		_ = recover()
	}()

	if usageEndpoint == "" {
		return
	}

	reportedMu.Lock()
	alreadyReported := reportedPackages[pkg]
	reportedPackages[pkg] = true
	reportedMu.Unlock()
	if alreadyReported {
		return
	}

	if usageReportingDisabled() {
		slog.Debug("usage reporting disabled by the environment", "package", pkg)
		return
	}

	go sendUsageEvent(pkg, dimensions)
}

// sendUsageEvent builds and sends one event, swallowing every failure. Runs
// on its own goroutine started by reportUsage: the deferred recover here is
// load-bearing, since a panic on a goroutine with no recover of its own
// crashes the process regardless of what the caller does.
func sendUsageEvent(pkg string, dimensions map[string]string) {
	defer func() {
		_ = recover()
	}()

	version := moduleVersion()

	req, err := http.NewRequest(http.MethodGet, buildUsageURL(pkg, version, dimensions), nil)
	if err != nil {
		slog.Debug("usage event not sent", "package", pkg, "error", err)
		return
	}
	req.Header.Set("User-Agent", pkg+"/"+version)

	client := &http.Client{Timeout: usageTimeout}
	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("usage event not sent", "package", pkg, "error", err)
		return
	}
	defer resp.Body.Close()

	slog.Debug("usage event sent", "package", pkg, "version", version)
}

// buildUsageURL builds the event URL for pkg. The defaults describe the
// package and the host; dimensions are added by the caller (for example kind
// and framework) and override a default of the same name. Every value is
// trimmed, capped at dimensionMaxLen bytes without splitting a rune, and
// dropped if empty after cleaning.
func buildUsageURL(pkg, version string, dimensions map[string]string) string {
	params := map[string]string{
		"package":    pkg,
		"version":    version,
		"os":         runtime.GOOS,
		"arch":       runtime.GOARCH,
		"go_version": runtime.Version(),
		"target":     detectTarget(),
		"ci":         ciDimension(runningInCI()),
	}
	for k, v := range dimensions {
		params[k] = v
	}

	values := url.Values{}
	for k, v := range params {
		cleaned := cleanDimension(v)
		if cleaned != "" {
			values.Set(k, cleaned)
		}
	}
	return usageEndpoint + "?" + values.Encode()
}

func ciDimension(ci bool) string {
	if ci {
		return "true"
	}
	return "false"
}

// cleanDimension trims value and caps it at dimensionMaxLen bytes, backing off
// far enough to avoid splitting a multi-byte rune.
func cleanDimension(value string) string {
	v := strings.TrimSpace(value)
	if len(v) <= dimensionMaxLen {
		return v
	}
	b := v[:dimensionMaxLen]
	for len(b) > 0 {
		r, size := utf8.DecodeLastRuneInString(b)
		if r != utf8.RuneError || size != 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}

func isTruthy(value string) bool {
	return truthyValues[strings.ToLower(strings.TrimSpace(value))]
}

// usageReportingDisabled returns true when the user opted out through any
// supported variable.
func usageReportingDisabled() bool {
	for _, name := range optOutEnvVars {
		if isTruthy(os.Getenv(name)) {
			return true
		}
	}
	return false
}

// runningInCI returns true when a well-known CI variable is set. Reported as
// the ci dimension so pipeline runs can be separated from real usage on the
// dashboard.
func runningInCI() bool {
	for _, name := range ciTruthyEnvVars {
		if isTruthy(os.Getenv(name)) {
			return true
		}
	}
	for _, name := range ciPresenceEnvVars {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	return false
}

// detectTarget returns "catalyst" when the process points at Diagrid
// Catalyst, else "dapr".
//
// Catalyst is configured through the Dapr SDK variables: the endpoint host is
// diagrid.io or a subdomain of it, and Catalyst issues DAPR_API_TOKEN. A
// self-hosted sidecar with API token authentication also reads as "catalyst".
// That is an approximation, and the dashboard reads it as one.
func detectTarget() string {
	for _, name := range daprEndpointEnvVars {
		raw := strings.TrimSpace(os.Getenv(name))
		if raw == "" {
			continue
		}
		if host, ok := endpointHostname(raw); ok && isCatalystHost(host) {
			return "catalyst"
		}
	}
	if strings.TrimSpace(os.Getenv("DAPR_API_TOKEN")) != "" {
		return "catalyst"
	}
	return "dapr"
}

func isCatalystHost(host string) bool {
	return host == catalystHostSuffix || strings.HasSuffix(host, "."+catalystHostSuffix)
}

// endpointHostname parses raw as a URL, prepending a scheme when it has none,
// and returns its hostname. Returns ok=false on a parse error rather than
// panicking or propagating one.
func endpointHostname(raw string) (host string, ok bool) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	return u.Hostname(), true
}

// moduleVersion returns this module's own version: the github.com/diagridio/go-ai
// entry in build-info Deps when go-ai is imported by an application, else the
// main module's version. Returns "unknown" when build info is unavailable, or
// the version is empty or "(devel)" (an unversioned local build, which is what
// `go test` itself produces for this module).
func moduleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, dep := range info.Deps {
		if dep.Path == goAIModulePath {
			return cleanModuleVersion(dep.Version)
		}
	}
	return cleanModuleVersion(info.Main.Version)
}

func cleanModuleVersion(v string) string {
	if v == "" || v == "(devel)" {
		return "unknown"
	}
	return v
}

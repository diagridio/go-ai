// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

// Command identity is a two-route HTTP service that verifies the inbound
// Catalyst user token and propagates the caller's identity on an outbound
// call. It is deliberately minimal: no agent, no model, no workflow.
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/diagridio/go-ai/identity"
)

const (
	// listenAddr is the port the Catalyst sidecar forwards inbound calls to.
	listenAddr = ":8080"

	// defaultDownstreamURL is where /downstream calls when DOWNSTREAM_URL is unset.
	defaultDownstreamURL = "http://localhost:8081/whoami"

	// readScope is the scope /whoami reports on; the middleware requires none.
	readScope = "read"
)

// whoamiResponse is what /whoami returns: the caller the middleware verified.
type whoamiResponse struct {
	Subject string   `json:"subject"`
	Tenant  string   `json:"tenant"`
	Scopes  []string `json:"scopes"`
	HasRead bool     `json:"hasRead"`
}

func main() {
	// One client for the life of the process. It reads the caller's token off
	// each request's context at send time, so sharing it across concurrent
	// requests is safe.
	client := identity.NewHTTPClient(nil)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /whoami", whoami)
	mux.HandleFunc("GET /downstream", downstream(client))

	// The zero OAuthConfig is fail-closed: a verified token on every request,
	// with issuer, audience and JWKS discovered from the sidecar.
	oauth := identity.OAuthConfig{}
	handler := identity.Middleware(oauth)(mux)

	log.Printf("listening on %s", listenAddr)
	if err := http.ListenAndServe(listenAddr, handler); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func whoami(w http.ResponseWriter, r *http.Request) {
	user, ok := identity.UserFromContext(r.Context())
	if !ok {
		// Unreachable while RequireAuth is at its default true.
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	scopes := user.Scopes
	if scopes == nil {
		scopes = []string{} // a JSON array even when the token carries none
	}
	writeJSON(w, http.StatusOK, whoamiResponse{
		Subject: user.Subject,
		Tenant:  user.Tenant,
		Scopes:  scopes,
		HasRead: user.HasScope(readScope),
	})
}

// downstream calls another service on behalf of the verified caller. There is
// no identity code in the handler: the client carries the caller as long as the
// outbound request is built with the inbound request's context.
func downstream(client *http.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, downstreamURL(), nil)
		if err != nil {
			unreachable(w, err)
			return
		}

		resp, err := client.Do(req)
		if err != nil {
			unreachable(w, err)
			return
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			unreachable(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"downstream": string(body)})
	}
}

func downstreamURL() string {
	if url := os.Getenv("DOWNSTREAM_URL"); url != "" {
		return url
	}
	return defaultDownstreamURL
}

// unreachable answers a failed outbound call, with this example's own code.
func unreachable(w http.ResponseWriter, err error) {
	log.Printf("downstream call failed: %v", err)
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "downstream_unreachable"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

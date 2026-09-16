// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package identity

import (
	"errors"
	"fmt"
	"net/http"
)

// The error codes the middleware writes as the error field of its JSON
// response. They are a fixed cross-SDK contract: a client switching languages
// sees the same string for the same failure, so the set does not grow or
// change spelling.
const (
	// ErrorCodeMissingToken is returned when the request carries no user token
	// and OAuthConfig.RequireAuth resolves to true. 401.
	ErrorCodeMissingToken = "oauth.missing_token"

	// ErrorCodeNotConfigured is returned when no identity coordinates could be
	// discovered, so no verifier could be built. 503.
	ErrorCodeNotConfigured = "oauth.not_configured"

	// ErrorCodeVerifierUnavailable is returned when the verifier could not
	// answer: JWKS key material has not loaded, or adjudication failed in a
	// way nothing anticipated. Either way the token was neither accepted nor
	// rejected. 503.
	ErrorCodeVerifierUnavailable = "oauth.verifier_unavailable"

	// ErrorCodeExpired is returned when the exp claim is in the past. 401.
	ErrorCodeExpired = "oauth.expired"

	// ErrorCodeInvalidIssuer is returned when the iss claim does not match the
	// configured or discovered issuer. 401.
	ErrorCodeInvalidIssuer = "oauth.invalid_issuer"

	// ErrorCodeInvalidAudience is returned when an audience is configured
	// and the aud claim does not contain it. 401.
	ErrorCodeInvalidAudience = "oauth.invalid_audience"

	// ErrorCodeInvalidSignature is returned when the signature does not verify
	// against the JWKS key the token names. 401.
	ErrorCodeInvalidSignature = "oauth.invalid_signature"

	// ErrorCodeDecodeError is returned when the token is not a well-formed
	// JWT. 401.
	ErrorCodeDecodeError = "oauth.decode_error"

	// ErrorCodeInvalidToken is returned when the token is well-formed and
	// correctly signed but otherwise unacceptable: a signing algorithm
	// outside the allowlist, or a missing required claim. 401.
	ErrorCodeInvalidToken = "oauth.invalid_token"

	// ErrorCodeMissingScope is returned when the verified token lacks a scope
	// OAuthConfig.Scopes requires. 403.
	ErrorCodeMissingScope = "oauth.missing_scope"
)

// ErrVerifierNotReady reports that JWKS key material is unavailable, so the
// token was neither accepted nor rejected. The middleware answers 503 rather
// than 401, because a key-fetch problem is the service's fault, not the
// caller's.
var ErrVerifierNotReady = errors.New("identity: jwks key material is not loaded")

// errVerifierPanicked reports that a [TokenVerifier] panicked instead of
// returning an error. It reaches the caller as 503 oauth.verifier_unavailable,
// the same as any other unanticipated adjudication failure.
var errVerifierPanicked = errors.New("identity: verifier panicked")

// TokenVerificationError is a signature or claim validation failure. Code is
// the stable cross-SDK error code the middleware reports; compare it with
// [errors.Is] against one of the sentinels below.
type TokenVerificationError struct {
	// Code is one of the oauth.* codes above.
	Code string

	// Err is the underlying detail, if any.
	Err error
}

// Sentinels for [errors.Is]. Only Code participates in the comparison, so
// errors.Is(err, identity.ErrExpired) matches any expiry failure whatever
// detail it wraps.
var (
	ErrExpired          = &TokenVerificationError{Code: ErrorCodeExpired}
	ErrInvalidIssuer    = &TokenVerificationError{Code: ErrorCodeInvalidIssuer}
	ErrInvalidAudience  = &TokenVerificationError{Code: ErrorCodeInvalidAudience}
	ErrInvalidSignature = &TokenVerificationError{Code: ErrorCodeInvalidSignature}
	ErrDecode           = &TokenVerificationError{Code: ErrorCodeDecodeError}
	ErrInvalidToken     = &TokenVerificationError{Code: ErrorCodeInvalidToken}
	ErrMissingScope     = &TokenVerificationError{Code: ErrorCodeMissingScope}
)

// Error returns the code, with the wrapped detail appended when there is one.
func (e *TokenVerificationError) Error() string {
	if e.Err == nil {
		return e.Code
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Err)
}

// Unwrap returns the wrapped detail.
func (e *TokenVerificationError) Unwrap() error { return e.Err }

// Is matches on Code alone, so a sentinel compares equal to any error carrying
// the same code.
func (e *TokenVerificationError) Is(target error) bool {
	other, ok := target.(*TokenVerificationError)
	return ok && other.Code == e.Code
}

// tokenVerificationErrorf builds a TokenVerificationError whose wrapped
// detail is formatted from cause.
func tokenVerificationErrorf(code string, cause error) *TokenVerificationError {
	return &TokenVerificationError{Code: code, Err: cause}
}

// statusForCode maps an error code to the HTTP status the middleware answers
// with. Only oauth.missing_scope is an authorization failure; everything else
// reaching this point is an authentication failure.
func statusForCode(code string) int {
	if code == ErrorCodeMissingScope {
		return http.StatusForbidden
	}
	return http.StatusUnauthorized
}

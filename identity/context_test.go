// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package identity

import (
	"context"
	"testing"
)

func TestTokenFromContext(t *testing.T) {
	ctx := ContextWithToken(context.Background(), "abc123")

	token, ok := TokenFromContext(ctx)
	if !ok || token != "abc123" {
		t.Fatalf("token = %q, ok = %v; want abc123, true", token, ok)
	}
}

// TestTokenFromContextNested: a derived context shadows the outer token and
// the outer one is untouched, which is what makes concurrent requests safe.
func TestTokenFromContextNested(t *testing.T) {
	outer := ContextWithToken(context.Background(), "first")
	inner := ContextWithToken(outer, "second")

	if token, _ := TokenFromContext(inner); token != "second" {
		t.Errorf("inner token = %q, want second", token)
	}
	if token, _ := TokenFromContext(outer); token != "first" {
		t.Errorf("outer token = %q, want first", token)
	}
}

func TestTokenFromContextUnset(t *testing.T) {
	if token, ok := TokenFromContext(context.Background()); ok || token != "" {
		t.Fatalf("token = %q, ok = %v; want \"\", false", token, ok)
	}
}

// TestTokenFromContextEmptyIsUnset: an empty token is no token, so a caller
// never propagates an empty header believing it is authenticated.
func TestTokenFromContextEmptyIsUnset(t *testing.T) {
	ctx := ContextWithToken(context.Background(), "")
	if _, ok := TokenFromContext(ctx); ok {
		t.Fatal("an empty token must read as unset")
	}
}

func TestOutboundIdentityHeadersWithToken(t *testing.T) {
	headers := outboundIdentityHeaders(ContextWithToken(context.Background(), "tok"))

	if len(headers) != 1 {
		t.Fatalf("headers = %v, want exactly one", headers)
	}
	if got, want := headers.Get(UserTokenHeader), BearerPrefix+"tok"; got != want {
		t.Errorf("%s = %q, want %q", UserTokenHeader, got, want)
	}
}

func TestOutboundIdentityHeadersWithoutToken(t *testing.T) {
	headers := outboundIdentityHeaders(context.Background())

	if headers == nil {
		t.Fatal("want an empty header set, got nil")
	}
	if len(headers) != 0 {
		t.Errorf("headers = %v, want empty", headers)
	}
}

func TestUserFromContext(t *testing.T) {
	user := &VerifiedUser{Subject: testSubject, Scopes: []string{"agent.invoke"}}
	ctx := ContextWithUser(context.Background(), user)

	got, ok := UserFromContext(ctx)
	if !ok {
		t.Fatal("want a verified user on the context")
	}
	if got.Subject != testSubject {
		t.Errorf("subject = %q, want %q", got.Subject, testSubject)
	}
	if !got.HasScope("agent.invoke") {
		t.Error("want HasScope(agent.invoke) to be true")
	}
	if got.HasScope("admin.write") {
		t.Error("want HasScope(admin.write) to be false")
	}
}

func TestUserFromContextUnset(t *testing.T) {
	if user, ok := UserFromContext(context.Background()); ok || user != nil {
		t.Fatalf("user = %v, ok = %v; want nil, false", user, ok)
	}
}

// TestUserFromContextNilIsUnset guards against a nil user reading as present,
// which would turn a missing identity into a nil-pointer dereference in a
// handler that trusted the ok result.
func TestUserFromContextNilIsUnset(t *testing.T) {
	ctx := ContextWithUser(context.Background(), nil)
	if _, ok := UserFromContext(ctx); ok {
		t.Fatal("a nil user must read as unset")
	}
}

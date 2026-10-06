// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package goai

import "testing"

// The root module does not depend on any adapter's framework library (see
// AGENTS.md: adapters are separate modules with their own go.mod), so these
// cases cannot exercise the "found" branch of frameworkVersion here — that
// needs an adapter's own test suite, which does depend on its framework. What
// they do cover: an unmapped label is dropped, a mapped label whose module is
// absent from build info is dropped too, and the label lookup is
// case-insensitive.
func TestFrameworkVersion(t *testing.T) {
	if got := frameworkVersion("test"); got != "" {
		t.Errorf("frameworkVersion(%q) = %q, want empty for an unmapped framework", "test", got)
	}

	for _, label := range []string{"langchaingo", "LangChainGo", "LANGCHAINGO", "eino", "Eino"} {
		if got := frameworkVersion(label); got != "" {
			t.Errorf("frameworkVersion(%q) = %q, want empty: this module never imports the adapter library", label, got)
		}
	}
}

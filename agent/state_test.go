// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package agent

import "testing"

func TestToStateAndInto(t *testing.T) {
	type s struct {
		Input  string `json:"input,omitempty"`
		Output string `json:"output,omitempty"`
	}

	// Struct -> State keeps only the populated (json-tagged) channels.
	st, err := ToState(s{Input: "hello"})
	if err != nil {
		t.Fatalf("ToState: %v", err)
	}
	if st["input"] != "hello" {
		t.Fatalf("input channel = %v, want hello", st["input"])
	}
	if _, present := st["output"]; present {
		t.Fatalf("empty output should be omitted, got %v", st["output"])
	}

	// A State passes through unchanged, a map converts without a copy round-trip.
	if got, _ := ToState(State{"input": "x"}); got["input"] != "x" {
		t.Fatalf("State passthrough failed: %v", got)
	}

	// State -> struct decodes by json tag.
	var out s
	if err := (State{"input": "a", "output": "b"}).Into(&out); err != nil {
		t.Fatalf("Into: %v", err)
	}
	if out.Input != "a" || out.Output != "b" {
		t.Fatalf("Into = %+v, want {a b}", out)
	}
}

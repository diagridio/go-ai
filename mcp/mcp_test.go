// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package mcp

import (
	"encoding/json"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestListToolsResultParsing checks that the JSON the sidecar's ListTools
// workflow returns decodes into the fields we surface as a Tool.
func TestListToolsResultParsing(t *testing.T) {
	raw := `{"tools":[{"name":"get_weather","description":"Get the weather",` +
		`"inputSchema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	var res mcpsdk.ListToolsResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(res.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(res.Tools))
	}
	if res.Tools[0].Name != "get_weather" || res.Tools[0].Description != "Get the weather" {
		t.Fatalf("unexpected tool: %+v", res.Tools[0])
	}
	if res.Tools[0].InputSchema == nil {
		t.Fatal("expected an input schema")
	}
}

// TestTextContent checks that CallTool output decodes and concatenates its text
// content, and that IsError round-trips.
func TestTextContent(t *testing.T) {
	raw := `{"content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}],"isError":false}`
	var res mcpsdk.CallToolResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := textContent(res.Content); got != "hello world" {
		t.Fatalf("textContent = %q, want %q", got, "hello world")
	}
	if res.IsError {
		t.Fatal("IsError should be false")
	}
}

func TestWorkflowNaming(t *testing.T) {
	if got := workflowPrefix + "weather.ListTools"; got != "dapr.internal.mcp.weather.ListTools" {
		t.Fatalf("ListTools name = %q", got)
	}
	if got := workflowPrefix + "weather.CallTool.get_weather"; got != "dapr.internal.mcp.weather.CallTool.get_weather" {
		t.Fatalf("CallTool name = %q", got)
	}
}

// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package langchaingo

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/diagridio/go-ai/agent"
	"github.com/diagridio/go-ai/mcp"

	"github.com/tmc/langchaingo/llms"
)

// maxToolTurns bounds the model-tool loop.
const maxToolTurns = 8

// ToolModelNode wraps a chat model that can call MCP tools. It runs the
// model-tool loop until the model replies without a tool call, then writes that
// reply to the output channel. The whole loop runs in one durable node.
func ToolModelNode(model llms.Model, tools []mcp.Tool, opts ...Option) agent.NodeFunc {
	c := resolve(opts...)

	llmTools := make([]llms.Tool, 0, len(tools))
	byName := make(map[string]mcp.Tool, len(tools))
	for _, t := range tools {
		llmTools = append(llmTools, llms.Tool{
			Type: "function",
			Function: &llms.FunctionDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Schema,
			},
		})
		byName[t.Name] = t
	}

	return func(ctx context.Context, s agent.State) (agent.State, error) {
		prompt, _ := s[c.inputKey].(string)
		msgs := make([]llms.MessageContent, 0, 4)
		if c.system != "" {
			msgs = append(msgs, llms.TextParts(llms.ChatMessageTypeSystem, c.system))
		}
		msgs = append(msgs, llms.TextParts(llms.ChatMessageTypeHuman, prompt))

		var callOpts []llms.CallOption
		if len(llmTools) > 0 {
			callOpts = append(callOpts, llms.WithTools(llmTools))
		}

		for turn := 0; turn < maxToolTurns; turn++ {
			resp, err := model.GenerateContent(ctx, msgs, callOpts...)
			if err != nil {
				return nil, fmt.Errorf("langchaingo: model call: %w", err)
			}
			if len(resp.Choices) == 0 {
				return nil, fmt.Errorf("langchaingo: model returned no choices")
			}
			choice := resp.Choices[0]
			if len(choice.ToolCalls) == 0 {
				return agent.State{c.outputKey: choice.Content}, nil
			}

			// Record the tool-call turn, then run each tool and feed results back.
			asst := llms.MessageContent{Role: llms.ChatMessageTypeAI}
			for _, tc := range choice.ToolCalls {
				asst.Parts = append(asst.Parts, tc)
			}
			msgs = append(msgs, asst)

			for _, tc := range choice.ToolCalls {
				tool, ok := byName[tc.FunctionCall.Name]
				if !ok {
					return nil, fmt.Errorf("langchaingo: model called unknown tool %q", tc.FunctionCall.Name)
				}
				var args map[string]any
				if a := tc.FunctionCall.Arguments; a != "" {
					if err := json.Unmarshal([]byte(a), &args); err != nil {
						return nil, fmt.Errorf("langchaingo: decode tool args: %w", err)
					}
				}
				result, err := tool.Call(ctx, args)
				if err != nil {
					return nil, err
				}
				msgs = append(msgs, llms.MessageContent{
					Role: llms.ChatMessageTypeTool,
					Parts: []llms.ContentPart{llms.ToolCallResponse{
						ToolCallID: tc.ID,
						Name:       tc.FunctionCall.Name,
						Content:    result,
					}},
				})
			}
		}
		return nil, fmt.Errorf("langchaingo: tool loop exceeded %d turns", maxToolTurns)
	}
}

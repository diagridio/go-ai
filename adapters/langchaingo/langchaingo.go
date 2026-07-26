// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

// Package langchaingo adapts LangChainGo (github.com/tmc/langchaingo) to the
// go-ai graph engine. Its node constructors wrap a chat model as an
// agent.NodeFunc. Durability comes from the Catalyst backend, not from langchaingo.
package langchaingo

import (
	"context"
	"fmt"

	"github.com/diagridio/go-ai/agent"

	"github.com/tmc/langchaingo/llms"
)

// Framework is the identifier recorded in the agent registry.
const Framework = "langchaingo"

type nodeConfig struct {
	system    string
	inputKey  string
	outputKey string
}

// Option configures a node constructor.
type Option func(*nodeConfig)

// WithSystemPrompt sets a system prompt prepended to the model call.
func WithSystemPrompt(s string) Option { return func(c *nodeConfig) { c.system = s } }

// WithInputKey sets the state channel read as the user prompt (default "input").
func WithInputKey(k string) Option { return func(c *nodeConfig) { c.inputKey = k } }

// WithOutputKey sets the state channel the reply is written to (default "output").
func WithOutputKey(k string) Option { return func(c *nodeConfig) { c.outputKey = k } }

func resolve(opts ...Option) nodeConfig {
	c := nodeConfig{inputKey: "input", outputKey: "output"}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// ModelNode wraps a chat model as a graph node. It reads the prompt from the
// input channel, calls the model, and writes the reply to the output channel.
func ModelNode(model llms.Model, opts ...Option) agent.NodeFunc {
	c := resolve(opts...)
	return func(ctx context.Context, s agent.State) (agent.State, error) {
		prompt, _ := s[c.inputKey].(string)
		msgs := make([]llms.MessageContent, 0, 2)
		if c.system != "" {
			msgs = append(msgs, llms.MessageContent{
				Role:  llms.ChatMessageTypeSystem,
				Parts: []llms.ContentPart{llms.TextContent{Text: c.system}},
			})
		}
		msgs = append(msgs, llms.MessageContent{
			Role:  llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{llms.TextContent{Text: prompt}},
		})

		resp, err := model.GenerateContent(ctx, msgs)
		if err != nil {
			return nil, fmt.Errorf("langchaingo: model call: %w", err)
		}
		if len(resp.Choices) == 0 {
			return nil, fmt.Errorf("langchaingo: model returned no choices")
		}
		return agent.State{c.outputKey: resp.Choices[0].Content}, nil
	}
}

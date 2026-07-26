// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

// Package eino adapts CloudWeGo Eino (github.com/cloudwego/eino) to the go-ai
// graph engine. Its node constructors wrap an Eino chat model as an
// agent.NodeFunc. Durability comes from the Catalyst backend, not from eino.
package eino

import (
	"context"
	"fmt"

	"github.com/diagridio/go-ai/agent"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Framework is the identifier recorded in the agent registry.
const Framework = "eino"

type nodeConfig struct {
	system    string
	inputKey  string
	outputKey string
}

// Option configures a node constructor.
type Option func(*nodeConfig)

// WithSystemPrompt sets a system message prepended to the model call.
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

// ChatModelNode wraps an Eino chat model as a graph node. It reads the prompt
// from the input channel, calls Generate, and writes the reply to the output
// channel.
func ChatModelNode(m model.BaseChatModel, opts ...Option) agent.NodeFunc {
	c := resolve(opts...)
	return func(ctx context.Context, s agent.State) (agent.State, error) {
		prompt, _ := s[c.inputKey].(string)
		msgs := make([]*schema.Message, 0, 2)
		if c.system != "" {
			msgs = append(msgs, schema.SystemMessage(c.system))
		}
		msgs = append(msgs, schema.UserMessage(prompt))

		reply, err := m.Generate(ctx, msgs)
		if err != nil {
			return nil, fmt.Errorf("eino: model generate: %w", err)
		}
		if reply == nil {
			return nil, fmt.Errorf("eino: model returned nil message")
		}
		return agent.State{c.outputKey: reply.Content}, nil
	}
}

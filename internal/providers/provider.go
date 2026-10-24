// Package providers defines the upstream model backends and a common interface
// the router calls. Each provider translates LLMGate's OpenAI-compatible
// request into its own wire format and back.
package providers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/harshakumar25/llmgate/internal/config"
)

// Message is one chat turn, OpenAI-compatible.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is the gateway's normalized chat request.
type Request struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

// Usage reports token accounting for a single call.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Response is the gateway's normalized chat response.
type Response struct {
	Content  string `json:"content"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Usage    Usage  `json:"usage"`
}

// Provider is implemented by every upstream backend.
type Provider interface {
	// Name identifies the provider in logs and metering.
	Name() string
	// Chat performs a single completion. It should return a non-nil error on
	// any non-2xx response so the router can fail over to the next provider.
	Chat(ctx context.Context, req Request) (*Response, error)
}

// Build constructs the ordered provider chain from config.
func Build(cfgs []config.Provider) ([]Provider, error) {
	httpClient := &http.Client{Timeout: 60 * time.Second}
	out := make([]Provider, 0, len(cfgs))
	for _, c := range cfgs {
		switch c.Name {
		case "openai":
			out = append(out, NewOpenAI(c, httpClient))
		case "ollama":
			out = append(out, NewOllama(c, httpClient))
		default:
			return nil, fmt.Errorf("providers: unknown provider %q", c.Name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("providers: empty chain")
	}
	return out, nil
}

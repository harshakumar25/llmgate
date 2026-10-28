// Package providers defines the upstream model backends and a common interface
// the router calls. Each provider translates LLMGate's OpenAI-compatible
// request into its own wire format and back.
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/harshakumar25/llmgate/internal/config"
)

// Message is one chat turn, OpenAI-compatible. An assistant turn may carry
// ToolCalls; a "tool" turn answers one of them by ToolCallID.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Request is the gateway's normalized chat request.
type Request struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	// ResponseFormat requests structured output, OpenAI-style
	// ({"type": "json_object"}). Ollama maps it to format: "json".
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	// Think toggles reasoning on models that support it (Ollama's "think").
	// Nil leaves the model's default; providers without the knob ignore it.
	Think *bool `json:"think,omitempty"`
	// Stream asks the gateway to send the reply as server-sent events. It is
	// a transport choice, not part of the prompt, so it never changes the
	// cache key (see cache.Key).
	Stream bool `json:"stream,omitempty"`
	// Tools the model may call, and how it must choose (OpenAI shapes; the
	// adapters translate). ToolChoice is "auto", "none", "required" or
	// {"type":"function","function":{"name":...}}, passed as raw JSON.
	Tools      []Tool          `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
}

// ResponseFormat is OpenAI's response_format field.
type ResponseFormat struct {
	Type string `json:"type"`
}

// Usage reports token accounting for a single call.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Response is the gateway's normalized chat response.
type Response struct {
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// FinishReason is OpenAI's vocabulary: "stop", "length" or "tool_calls".
	// Empty when the provider did not say.
	FinishReason string `json:"finish_reason,omitempty"`
	Model        string `json:"model"`
	Provider     string `json:"provider"`
	Usage        Usage  `json:"usage"`
}

// StatusError is a non-2xx reply from an upstream. The router uses the code to
// decide whether retrying the same provider can help.
type StatusError struct {
	Provider string
	Code     int
	Body     string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: status %d: %s", e.Provider, e.Code, e.Body)
}

// Retryable reports whether sending the same request to the same provider
// again might succeed: timeouts, rate limits, 5xx and transport errors can;
// other 4xx replies (bad request, bad key, unknown model) cannot, and a
// cancelled caller should not be retried at all.
func Retryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, ErrInvalidRequest) {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code == http.StatusRequestTimeout || se.Code == http.StatusTooManyRequests || se.Code >= 500
	}
	return true
}

// Provider is implemented by every upstream backend.
type Provider interface {
	// Name identifies the provider in logs and metering.
	Name() string
	// Chat performs a single completion. It should return a non-nil error on
	// any non-2xx response so the router can fail over to the next provider.
	Chat(ctx context.Context, req Request) (*Response, error)
}

// Build constructs the ordered provider chain from config. timeout bounds each
// upstream call; zero means 60s.
func Build(cfgs []config.Provider, timeout time.Duration) ([]Provider, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	httpClient := &http.Client{Timeout: timeout}
	out := make([]Provider, 0, len(cfgs))
	for _, c := range cfgs {
		switch c.Name {
		case "openai":
			out = append(out, NewOpenAI(c, httpClient))
		case "ollama":
			out = append(out, NewOllama(c, httpClient))
		case "anthropic":
			out = append(out, NewAnthropic(c, httpClient))
		case "gemini":
			out = append(out, NewGemini(c, httpClient))
		default:
			return nil, fmt.Errorf("providers: unknown provider %q", c.Name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("providers: empty chain")
	}
	return out, nil
}

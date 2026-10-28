package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/harshakumar25/llmgate/internal/config"
)

// OpenAI talks to any OpenAI-compatible /chat/completions endpoint.
type OpenAI struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewOpenAI builds an OpenAI provider. BaseURL defaults to the public API.
func NewOpenAI(c config.Provider, client *http.Client) *OpenAI {
	base := c.BaseURL
	if base == "" {
		base = "https://api.openai.com"
	}
	return &OpenAI{baseURL: base, apiKey: c.APIKey, model: c.Model, client: client}
}

func (o *OpenAI) Name() string { return "openai" }

// payload builds the /chat/completions body shared by Chat and ChatStream.
func (o *OpenAI) payload(req Request) (string, map[string]any) {
	model := req.Model
	if model == "" {
		model = o.model
	}
	p := map[string]any{
		"model":       model,
		"messages":    req.Messages,
		"temperature": req.Temperature,
	}
	// OpenAI rejects max_tokens: 0, so only send it when set.
	if req.MaxTokens > 0 {
		p["max_tokens"] = req.MaxTokens
	}
	if req.ResponseFormat != nil {
		p["response_format"] = req.ResponseFormat
	}
	return model, p
}

func (o *OpenAI) post(ctx context.Context, payload map[string]any) (*http.Response, error) {
	return postJSON(ctx, o.client, o.Name(), o.baseURL+"/v1/chat/completions", o.apiKey, payload)
}

func (o *OpenAI) Chat(ctx context.Context, req Request) (*Response, error) {
	model, payload := o.payload(req)
	resp, err := o.post(ctx, payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var parsed struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("openai: decode: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("openai: no choices returned")
	}

	return &Response{
		Content:  parsed.Choices[0].Message.Content,
		Model:    model,
		Provider: o.Name(),
		Usage:    usage(parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens),
	}, nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

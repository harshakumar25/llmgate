package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/harshakumar25/llmgate/internal/config"
)

// Ollama talks to a local Ollama server (https://ollama.com) via /api/chat.
// It is the zero-cost default backend for local development.
type Ollama struct {
	baseURL string
	model   string
	client  *http.Client
}

// NewOllama builds an Ollama provider. BaseURL defaults to the local daemon.
func NewOllama(c config.Provider, client *http.Client) *Ollama {
	base := c.BaseURL
	if base == "" {
		base = "http://localhost:11434"
	}
	return &Ollama{baseURL: base, model: c.Model, client: client}
}

func (o *Ollama) Name() string { return "ollama" }

func (o *Ollama) Chat(ctx context.Context, req Request) (*Response, error) {
	model := req.Model
	if model == "" {
		model = o.model
	}
	payload := map[string]any{
		"model":    model,
		"messages": req.Messages,
		"stream":   false,
	}
	// Ollama takes sampling settings under "options".
	opts := map[string]any{}
	if req.Temperature != 0 {
		opts["temperature"] = req.Temperature
	}
	if req.MaxTokens > 0 {
		opts["num_predict"] = req.MaxTokens
	}
	if len(opts) > 0 {
		payload["options"] = opts
	}
	if req.ResponseFormat != nil && req.ResponseFormat.Type == "json_object" {
		payload["format"] = "json"
	}
	if req.Think != nil {
		payload["think"] = *req.Think
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama: request: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &StatusError{Provider: "ollama", Code: resp.StatusCode, Body: truncate(data, 300)}
	}

	var parsed struct {
		Message         Message `json:"message"`
		PromptEvalCount int     `json:"prompt_eval_count"`
		EvalCount       int     `json:"eval_count"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("ollama: decode: %w", err)
	}

	return &Response{
		Content:  parsed.Message.Content,
		Model:    model,
		Provider: o.Name(),
		Usage: Usage{
			PromptTokens:     parsed.PromptEvalCount,
			CompletionTokens: parsed.EvalCount,
			TotalTokens:      parsed.PromptEvalCount + parsed.EvalCount,
		},
	}, nil
}

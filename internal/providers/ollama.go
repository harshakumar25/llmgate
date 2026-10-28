package providers

import (
	"context"
	"encoding/json"
	"fmt"
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

// payload builds the /api/chat body shared by Chat and ChatStream.
func (o *Ollama) payload(req Request) (string, map[string]any, error) {
	model := req.Model
	if model == "" {
		model = o.model
	}
	msgs, err := ollamaMessages(req.Messages)
	if err != nil {
		return "", nil, err
	}
	p := map[string]any{
		"model":    model,
		"messages": msgs,
		"stream":   false,
	}
	// Ollama takes OpenAI-shaped tools but has no tool_choice: "none" is
	// honoured by not offering the tools; "required" cannot be enforced.
	if mode, _ := toolChoiceMode(req.ToolChoice); len(req.Tools) > 0 && mode != "none" {
		p["tools"] = req.Tools
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
		p["options"] = opts
	}
	if req.ResponseFormat != nil && req.ResponseFormat.Type == "json_object" {
		p["format"] = "json"
	}
	if req.Think != nil {
		p["think"] = *req.Think
	}
	return model, p, nil
}

func (o *Ollama) post(ctx context.Context, payload map[string]any) (*http.Response, error) {
	return postJSON(ctx, o.client, o.Name(), o.baseURL+"/api/chat", "", payload)
}

func (o *Ollama) Chat(ctx context.Context, req Request) (*Response, error) {
	model, payload, err := o.payload(req)
	if err != nil {
		return nil, err
	}
	resp, err := o.post(ctx, payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var parsed struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		DoneReason      string `json:"done_reason"`
		PromptEvalCount int    `json:"prompt_eval_count"`
		EvalCount       int    `json:"eval_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("ollama: decode: %w", err)
	}

	var calls []ToolCall
	for _, c := range parsed.Message.ToolCalls {
		id := c.ID
		if id == "" {
			id = newCallID()
		}
		calls = append(calls, ToolCall{ID: id, Type: "function",
			Function: ToolCallFunction{Name: c.Function.Name, Arguments: argsString(c.Function.Arguments)}})
	}
	return &Response{
		Content:      parsed.Message.Content,
		ToolCalls:    calls,
		FinishReason: finishFrom(parsed.DoneReason, len(calls)),
		Model:        model,
		Provider:     o.Name(),
		Usage:        usage(parsed.PromptEvalCount, parsed.EvalCount),
	}, nil
}

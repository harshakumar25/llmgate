package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/harshakumar25/llmgate/internal/config"
)

// anthropicVersion is the Messages API version header LLMGate speaks.
const anthropicVersion = "2023-06-01"

// anthropicDefaultMaxTokens is used when the client sets no max_tokens, since
// the Messages API requires one.
const anthropicDefaultMaxTokens = 1024

// Anthropic talks to the Anthropic Messages API (POST /v1/messages).
type Anthropic struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewAnthropic builds an Anthropic provider. BaseURL defaults to the public API.
func NewAnthropic(c config.Provider, client *http.Client) *Anthropic {
	base := c.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	return &Anthropic{baseURL: base, apiKey: c.APIKey, model: c.Model, client: client}
}

func (a *Anthropic) Name() string { return "anthropic" }

// payload translates the gateway request into the Messages API shape: system
// turns move to the top-level "system" field, and max_tokens is mandatory.
func (a *Anthropic) payload(req Request, stream bool) (string, []byte, error) {
	model := req.Model
	if model == "" {
		model = a.model
	}
	var system []string
	msgs := make([]Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == "system" {
			system = append(system, m.Content)
			continue
		}
		msgs = append(msgs, m)
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}
	p := map[string]any{
		"model":      model,
		"messages":   msgs,
		"max_tokens": maxTokens,
	}
	if len(system) > 0 {
		p["system"] = strings.Join(system, "\n\n")
	}
	if req.Temperature != 0 {
		p["temperature"] = req.Temperature
	}
	if stream {
		p["stream"] = true
	}
	body, err := json.Marshal(p)
	return model, body, err
}

func (a *Anthropic) do(ctx context.Context, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	if a.apiKey != "" {
		httpReq.Header.Set("x-api-key", a.apiKey)
	}
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &StatusError{Provider: "anthropic", Code: resp.StatusCode, Body: truncate(data, 300)}
	}
	return resp, nil
}

func (a *Anthropic) Chat(ctx context.Context, req Request) (*Response, error) {
	model, body, err := a.payload(req, false)
	if err != nil {
		return nil, err
	}
	resp, err := a.do(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("anthropic: decode: %w", err)
	}
	var text strings.Builder
	for _, c := range parsed.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	return &Response{
		Content:  text.String(),
		Model:    model,
		Provider: a.Name(),
		Usage:    usage(parsed.Usage.InputTokens, parsed.Usage.OutputTokens),
	}, nil
}

func usage(prompt, completion int) Usage {
	return Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion}
}

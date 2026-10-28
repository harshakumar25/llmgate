package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/harshakumar25/llmgate/internal/config"
)

// Gemini talks to Google's Gemini API (generateContent and
// streamGenerateContent). Its shape differs from OpenAI's in most places:
// turns are "contents" made of "parts", the assistant role is "model", the
// system prompt is a separate systemInstruction, tool calls are
// functionCall parts with object arguments and no ids, and tool results are
// functionResponse parts.
type Gemini struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewGemini builds a Gemini provider. BaseURL defaults to the public API.
func NewGemini(c config.Provider, client *http.Client) *Gemini {
	base := c.BaseURL
	if base == "" {
		base = "https://generativelanguage.googleapis.com"
	}
	return &Gemini{baseURL: strings.TrimRight(base, "/"), apiKey: c.APIKey, model: c.Model, client: client}
}

func (g *Gemini) Name() string { return "gemini" }

type geminiPart struct {
	Text             string          `json:"text,omitempty"`
	FunctionCall     *geminiCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFuncResp `json:"functionResponse,omitempty"`
}

type geminiCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiFuncResp struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

func (g *Gemini) payload(req Request) (string, map[string]any, error) {
	model := req.Model
	if model == "" {
		model = g.model
	}
	names := toolNames(req.Messages)
	var system []geminiPart
	var contents []geminiContent
	for _, m := range req.Messages {
		switch {
		case m.Role == "system":
			system = append(system, geminiPart{Text: m.Content})
		case m.Role == "tool":
			contents = append(contents, geminiContent{Role: "user", Parts: []geminiPart{{
				FunctionResponse: &geminiFuncResp{Name: names[m.ToolCallID], Response: toolResult(m.Content)},
			}}})
		default:
			role := "user"
			if m.Role == "assistant" {
				role = "model"
			}
			var parts []geminiPart
			if m.Content != "" {
				parts = append(parts, geminiPart{Text: m.Content})
			}
			for _, c := range m.ToolCalls {
				args, err := argsObject(c.Function.Arguments)
				if err != nil {
					return "", nil, err
				}
				parts = append(parts, geminiPart{FunctionCall: &geminiCall{Name: c.Function.Name, Args: args}})
			}
			if len(parts) == 0 {
				parts = []geminiPart{{Text: ""}}
			}
			contents = append(contents, geminiContent{Role: role, Parts: parts})
		}
	}
	p := map[string]any{"contents": contents}
	if len(system) > 0 {
		p["systemInstruction"] = geminiContent{Parts: system}
	}
	gen := map[string]any{}
	if req.Temperature != 0 {
		gen["temperature"] = req.Temperature
	}
	if req.MaxTokens > 0 {
		gen["maxOutputTokens"] = req.MaxTokens
	}
	if req.ResponseFormat != nil && req.ResponseFormat.Type == "json_object" {
		gen["responseMimeType"] = "application/json"
	}
	if len(gen) > 0 {
		p["generationConfig"] = gen
	}
	if len(req.Tools) > 0 {
		decls := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			d := map[string]any{"name": t.Function.Name, "description": t.Function.Description}
			if len(t.Function.Parameters) > 0 {
				d["parameters"] = t.Function.Parameters
			}
			decls = append(decls, d)
		}
		p["tools"] = []map[string]any{{"functionDeclarations": decls}}
		mode, name := toolChoiceMode(req.ToolChoice)
		cfg := map[string]any{}
		switch mode {
		case "none":
			cfg["mode"] = "NONE"
		case "required":
			cfg["mode"] = "ANY"
		case "function":
			cfg["mode"] = "ANY"
			cfg["allowedFunctionNames"] = []string{name}
		}
		if len(cfg) > 0 {
			p["toolConfig"] = map[string]any{"functionCallingConfig": cfg}
		}
	}
	return model, p, nil
}

// toolResult wraps a tool's output for functionResponse, which must be a JSON
// object: an object is passed as is, anything else goes under "content".
func toolResult(content string) json.RawMessage {
	var obj map[string]any
	if json.Unmarshal([]byte(content), &obj) == nil {
		return json.RawMessage(content)
	}
	b, _ := json.Marshal(map[string]any{"content": content})
	return b
}

func (g *Gemini) post(ctx context.Context, model, method string, payload map[string]any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/v1beta/models/%s:%s", g.baseURL, url.PathEscape(model), method)
	if method == "streamGenerateContent" {
		u += "?alt=sse"
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if g.apiKey != "" {
		httpReq.Header.Set("x-goog-api-key", g.apiKey) // a header, so the key never lands in URL logs
	}
	resp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini: request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &StatusError{Provider: "gemini", Code: resp.StatusCode, Body: truncate(data, 300)}
	}
	return resp, nil
}

// geminiReply is one GenerateContentResponse (the whole reply, or one chunk).
type geminiReply struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

// fold adds one reply's text, calls, finish reason and usage into out.
func (r *geminiReply) fold(out *Response, text *strings.Builder, onText func(string) error) error {
	if r.PromptFeedback != nil && r.PromptFeedback.BlockReason != "" {
		return &StatusError{Provider: "gemini", Code: http.StatusBadRequest, Body: "prompt blocked: " + r.PromptFeedback.BlockReason}
	}
	if u := r.UsageMetadata; u != nil {
		out.Usage = usage(u.PromptTokenCount, u.CandidatesTokenCount)
	}
	if len(r.Candidates) == 0 {
		return nil
	}
	c := r.Candidates[0]
	for _, p := range c.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: newCallID(), Type: "function",
				Function: ToolCallFunction{Name: p.FunctionCall.Name, Arguments: argsString(p.FunctionCall.Args)}})
		case p.Text != "":
			text.WriteString(p.Text)
			if onText != nil {
				if err := onText(p.Text); err != nil {
					return err
				}
			}
		}
	}
	if c.FinishReason != "" {
		out.FinishReason = geminiFinish(c.FinishReason)
	}
	return nil
}

func geminiFinish(reason string) string {
	switch reason {
	case "MAX_TOKENS":
		return "length"
	case "STOP":
		return "stop"
	}
	return "content_filter" // SAFETY, RECITATION, BLOCKLIST, ...
}

func (g *Gemini) Chat(ctx context.Context, req Request) (*Response, error) {
	model, payload, err := g.payload(req)
	if err != nil {
		return nil, err
	}
	resp, err := g.post(ctx, model, "generateContent", payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r geminiReply
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("gemini: decode: %w", err)
	}
	out := &Response{Model: model, Provider: g.Name()}
	var text strings.Builder
	if err := r.fold(out, &text, nil); err != nil {
		return nil, err
	}
	out.Content = text.String()
	if len(out.ToolCalls) > 0 {
		out.FinishReason = "tool_calls"
	}
	return out, nil
}

// ChatStream streams via streamGenerateContent?alt=sse: each event is a
// partial GenerateContentResponse, the last one carrying usage.
func (g *Gemini) ChatStream(ctx context.Context, req Request, onDelta func(string) error) (*Response, error) {
	model, payload, err := g.payload(req)
	if err != nil {
		return nil, err
	}
	resp, err := g.post(ctx, model, "streamGenerateContent", payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := &Response{Model: model, Provider: g.Name()}
	var text strings.Builder
	err = scanSSE(resp.Body, func(data []byte) error {
		var r geminiReply
		if err := json.Unmarshal(data, &r); err != nil {
			return fmt.Errorf("gemini: decode chunk: %w", err)
		}
		return r.fold(out, &text, onDelta)
	})
	if err != nil {
		return nil, err
	}
	out.Content = text.String()
	return out, nil
}

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/harshakumar25/llmgate/internal/providers"
)

// The OpenAI-compatible surface: POST /v1/chat/completions and GET /v1/models
// in OpenAI's exact request and response shapes, so the official SDKs,
// LangChain and anything else that takes a base_url work unchanged:
//
//	client = OpenAI(base_url="http://localhost:8080/v1", api_key="<gateway key>")
//
// It runs the same pipeline as /v1/chat; only the dialect differs.

// oaiRequest is the subset of the Chat Completions request LLMGate honours.
// Unknown fields are ignored, as OpenAI-compatible servers conventionally do.
type oaiRequest struct {
	Model               string       `json:"model"`
	Messages            []oaiMessage `json:"messages"`
	Temperature         *float64     `json:"temperature"`
	MaxTokens           int          `json:"max_tokens"`
	MaxCompletionTokens int          `json:"max_completion_tokens"`
	Stream              bool         `json:"stream"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	ResponseFormat *providers.ResponseFormat `json:"response_format"`
	N              *int                      `json:"n"`
	Tools          json.RawMessage           `json:"tools"`
	// Think is LLMGate's reasoning toggle (Ollama), accepted here too since
	// OpenAI's request has no equivalent. SDKs send it via extra_body.
	Think *bool `json:"think"`
}

// oaiMessage.Content is either a string or an array of content parts.
type oaiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// text flattens a message's content to plain text. Text parts are joined;
// image and audio parts are rejected rather than silently dropped.
func (m oaiMessage) text() (string, error) {
	c := bytes.TrimSpace(m.Content)
	if len(c) == 0 || string(c) == "null" {
		return "", nil
	}
	if c[0] == '"' {
		var s string
		err := json.Unmarshal(c, &s)
		return s, err
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(c, &parts); err != nil {
		return "", fmt.Errorf("content must be a string or an array of parts")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return "", fmt.Errorf("content part type %q is not supported; only text is", p.Type)
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var in oaiRequest
	if !decodeBody(w, r, &in) {
		return
	}
	if in.N != nil && *in.N != 1 {
		writeErrorParam(w, http.StatusBadRequest, "n", "only n=1 is supported")
		return
	}
	if len(bytes.TrimSpace(in.Tools)) > 0 && string(bytes.TrimSpace(in.Tools)) != "null" {
		writeErrorParam(w, http.StatusBadRequest, "tools", "tool calling is not supported by this gateway yet")
		return
	}
	req := providers.Request{
		Model:          in.Model,
		MaxTokens:      in.MaxTokens,
		ResponseFormat: in.ResponseFormat,
		Stream:         in.Stream,
		Think:          in.Think,
	}
	if in.MaxCompletionTokens > 0 {
		req.MaxTokens = in.MaxCompletionTokens
	}
	if in.Temperature != nil {
		req.Temperature = *in.Temperature
	}
	for i, m := range in.Messages {
		text, err := m.text()
		if err != nil {
			writeErrorParam(w, http.StatusBadRequest, fmt.Sprintf("messages[%d].content", i), err.Error())
			return
		}
		req.Messages = append(req.Messages, providers.Message{Role: m.Role, Content: text})
	}
	f := openaiFormat{
		id:           "chatcmpl-" + requestID(r),
		created:      time.Now().Unix(),
		includeUsage: in.StreamOptions != nil && in.StreamOptions.IncludeUsage,
	}
	s.serveChat(w, r, req, f)
}

type openaiFormat struct {
	id           string
	created      int64
	includeUsage bool
}

type oaiChoice struct {
	Index        int             `json:"index"`
	Message      *oaiOutMessage  `json:"message,omitempty"`
	Delta        *oaiOutMessage  `json:"delta,omitempty"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs"`
}

type oaiOutMessage struct {
	Role    string  `json:"role,omitempty"`
	Content *string `json:"content,omitempty"`
}

type oaiCompletion struct {
	ID                string           `json:"id"`
	Object            string           `json:"object"`
	Created           int64            `json:"created"`
	Model             string           `json:"model"`
	Choices           []oaiChoice      `json:"choices"`
	Usage             *providers.Usage `json:"usage,omitempty"`
	SystemFingerprint *string          `json:"system_fingerprint"`
}

var jsonNull = json.RawMessage("null")

// finishReason reports "length" when the reply used the whole token
// allowance, which is how a cut-off answer shows up across providers.
func finishReason(req providers.Request, resp *providers.Response) *string {
	r := "stop"
	if req.MaxTokens > 0 && resp.Usage.CompletionTokens >= req.MaxTokens {
		r = "length"
	}
	return &r
}

func (f openaiFormat) reply(w http.ResponseWriter, req providers.Request, resp *providers.Response) {
	content := resp.Content
	w.Header().Set("X-LLMGate-Provider", resp.Provider)
	usage := resp.Usage
	writeJSON(w, http.StatusOK, oaiCompletion{
		ID:      f.id,
		Object:  "chat.completion",
		Created: f.created,
		Model:   resp.Model,
		Choices: []oaiChoice{{
			Message:      &oaiOutMessage{Role: "assistant", Content: &content},
			FinishReason: finishReason(req, resp),
			Logprobs:     jsonNull,
		}},
		Usage: &usage,
	})
}

func (f openaiFormat) stream(w http.ResponseWriter, req providers.Request) streamWriter {
	return &openaiStream{f: f, req: req, sse: newSSE(w)}
}

// openaiStream writes chat.completion.chunk events: a first chunk carrying
// the assistant role, one chunk per text fragment, a final chunk with the
// finish reason, an optional usage chunk (stream_options.include_usage), then
// [DONE]. Chunks before the last carry the requested model name; the final
// ones carry the model that actually answered.
type openaiStream struct {
	f   openaiFormat
	req providers.Request
	sse *sse
}

func (o *openaiStream) chunk(model string, choices []oaiChoice, usage *providers.Usage) error {
	return o.sse.event(oaiCompletion{
		ID: o.f.id, Object: "chat.completion.chunk", Created: o.f.created,
		Model: model, Choices: choices, Usage: usage,
	})
}

func (o *openaiStream) delta(text string) error {
	if !o.sse.started {
		empty := ""
		role := []oaiChoice{{Delta: &oaiOutMessage{Role: "assistant", Content: &empty}, Logprobs: jsonNull}}
		if err := o.chunk(o.req.Model, role, nil); err != nil {
			return err
		}
	}
	return o.chunk(o.req.Model, []oaiChoice{{Delta: &oaiOutMessage{Content: &text}, Logprobs: jsonNull}}, nil)
}

func (o *openaiStream) done(resp *providers.Response) {
	if !o.sse.started {
		empty := ""
		_ = o.chunk(resp.Model, []oaiChoice{{Delta: &oaiOutMessage{Role: "assistant", Content: &empty}, Logprobs: jsonNull}}, nil)
	}
	_ = o.chunk(resp.Model, []oaiChoice{{Delta: &oaiOutMessage{}, FinishReason: finishReason(o.req, resp), Logprobs: jsonNull}}, nil)
	if o.f.includeUsage {
		usage := resp.Usage
		_ = o.chunk(resp.Model, []oaiChoice{}, &usage)
	}
	o.sse.finish()
}

func (o *openaiStream) fail(msg string) {
	_ = o.sse.event(map[string]any{"error": apiError(http.StatusBadGateway, "upstream_error", "", msg)})
}

func (o *openaiStream) started() bool { return o.sse.started }

// handleModels lists the model names clients can request, OpenAI-style.
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	seen := map[string]bool{}
	data := []model{}
	for _, p := range s.deps.Config.Providers {
		for _, name := range p.Serves() {
			if name == "" || name == "*" || seen[name] {
				continue
			}
			seen[name] = true
			data = append(data, model{ID: name, Object: "model", OwnedBy: p.Name})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

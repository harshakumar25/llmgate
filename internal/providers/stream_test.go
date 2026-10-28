package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/harshakumar25/llmgate/internal/config"
)

// fakeUpstream serves a fixed body and records the decoded request.
func fakeUpstream(t *testing.T, status int, body string, got *map[string]any, headers *http.Header) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			_ = json.NewDecoder(r.Body).Decode(got)
		}
		if headers != nil {
			*headers = r.Header.Clone()
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func collect(t *testing.T, s Streamer, req Request) (*Response, []string) {
	t.Helper()
	var deltas []string
	resp, err := s.ChatStream(context.Background(), req, func(d string) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp, deltas
}

var hi = Request{Messages: []Message{{Role: "user", Content: "hi"}}}

func TestOpenAIStream(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
		`data: [DONE]`,
	}, "\n\n")
	var got map[string]any
	srv := fakeUpstream(t, 200, body, &got, nil)
	o := NewOpenAI(config.Provider{BaseURL: srv.URL, Model: "gpt"}, srv.Client())

	resp, deltas := collect(t, o, hi)
	if strings.Join(deltas, "|") != "Hel|lo" || resp.Content != "Hello" {
		t.Fatalf("deltas=%q content=%q", deltas, resp.Content)
	}
	if resp.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if got["stream"] != true || got["stream_options"] == nil {
		t.Fatalf("request = %v, want stream + include_usage", got)
	}
}

func TestOllamaStream(t *testing.T) {
	body := `{"message":{"content":"Hel"},"done":false}
{"message":{"content":"lo"},"done":false}
{"message":{"content":""},"done":true,"prompt_eval_count":4,"eval_count":2}
`
	var got map[string]any
	srv := fakeUpstream(t, 200, body, &got, nil)
	o := NewOllama(config.Provider{BaseURL: srv.URL, Model: "m"}, srv.Client())

	resp, deltas := collect(t, o, hi)
	if strings.Join(deltas, "|") != "Hel|lo" || resp.Usage.TotalTokens != 6 {
		t.Fatalf("deltas=%q usage=%+v", deltas, resp.Usage)
	}
	if got["stream"] != true {
		t.Fatalf("stream flag not sent: %v", got)
	}
}

func TestOllamaStreamError(t *testing.T) {
	srv := fakeUpstream(t, 200, `{"error":"model not found"}`+"\n", nil, nil)
	o := NewOllama(config.Provider{BaseURL: srv.URL, Model: "m"}, srv.Client())
	_, err := o.ChatStream(context.Background(), hi, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "model not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestAnthropicChatTranslatesRequest(t *testing.T) {
	var got map[string]any
	var hdr http.Header
	srv := fakeUpstream(t, 200,
		`{"content":[{"type":"text","text":"Paris"}],"usage":{"input_tokens":9,"output_tokens":1}}`, &got, &hdr)
	a := NewAnthropic(config.Provider{BaseURL: srv.URL, APIKey: "k", Model: "claude"}, srv.Client())

	resp, err := a.Chat(context.Background(), Request{Messages: []Message{
		{Role: "system", Content: "Be brief."},
		{Role: "user", Content: "Capital of France?"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Paris" || resp.Usage.TotalTokens != 10 {
		t.Fatalf("resp = %+v", resp)
	}
	if got["system"] != "Be brief." || got["max_tokens"] != float64(anthropicDefaultMaxTokens) {
		t.Fatalf("payload = %v", got)
	}
	if msgs := got["messages"].([]any); len(msgs) != 1 {
		t.Fatalf("system turn should be lifted out of messages: %v", msgs)
	}
	if hdr.Get("x-api-key") != "k" || hdr.Get("anthropic-version") != anthropicVersion {
		t.Fatalf("headers = %v", hdr)
	}
}

func TestAnthropicStream(t *testing.T) {
	body := strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":8}}}",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\"}",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}",
		"event: ping\ndata: {\"type\":\"ping\"}",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}",
	}, "\n\n")
	var got map[string]any
	srv := fakeUpstream(t, 200, body, &got, nil)
	a := NewAnthropic(config.Provider{BaseURL: srv.URL, Model: "claude"}, srv.Client())

	resp, deltas := collect(t, a, hi)
	if strings.Join(deltas, "|") != "Hel|lo" || resp.Usage.TotalTokens != 11 {
		t.Fatalf("deltas=%q usage=%+v", deltas, resp.Usage)
	}
	if got["stream"] != true {
		t.Fatalf("stream flag not sent: %v", got)
	}
}

func TestStatusErrorsAreTyped(t *testing.T) {
	srv := fakeUpstream(t, 429, `{"error":"slow down"}`, nil, nil)
	cases := map[string]Provider{
		"openai":    NewOpenAI(config.Provider{BaseURL: srv.URL}, srv.Client()),
		"ollama":    NewOllama(config.Provider{BaseURL: srv.URL}, srv.Client()),
		"anthropic": NewAnthropic(config.Provider{BaseURL: srv.URL}, srv.Client()),
	}
	for name, p := range cases {
		_, err := p.Chat(context.Background(), hi)
		var se *StatusError
		if !errors.As(err, &se) || se.Code != 429 || !Retryable(err) {
			t.Errorf("%s: err = %v, want retryable StatusError 429", name, err)
		}
	}
}

func TestDeltaErrorAbortsStream(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n"
	srv := fakeUpstream(t, 200, body, nil, nil)
	o := NewOpenAI(config.Provider{BaseURL: srv.URL}, srv.Client())
	stop := errors.New("client left")
	n := 0
	_, err := o.ChatStream(context.Background(), hi, func(string) error { n++; return stop })
	if !errors.Is(err, stop) || n != 1 {
		t.Fatalf("err=%v deltas=%d", err, n)
	}
}

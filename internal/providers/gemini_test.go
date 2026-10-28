package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/harshakumar25/llmgate/internal/config"
)

func TestGeminiChatTranslates(t *testing.T) {
	var got map[string]any
	var hdr http.Header
	var path string
	srv := fakeUpstream(t, 200, `{"candidates":[{"content":{"role":"model","parts":[{"text":"Paris"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":1}}`, &got, &hdr)
	srv.Config.Handler = recordPath(&path, srv.Config.Handler)
	g := NewGemini(config.Provider{BaseURL: srv.URL, APIKey: "gk", Model: "gemini-2.5-flash"}, srv.Client())

	resp, err := g.Chat(context.Background(), Request{Messages: []Message{
		{Role: "system", Content: "Be brief."},
		{Role: "user", Content: "Capital of France?"},
		{Role: "assistant", Content: "Let me think."},
		{Role: "user", Content: "Well?"},
	}, MaxTokens: 50, ResponseFormat: &ResponseFormat{Type: "json_object"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Paris" || resp.Usage.TotalTokens != 8 || resp.FinishReason != "stop" {
		t.Fatalf("resp = %+v", resp)
	}
	if path != "/v1beta/models/gemini-2.5-flash:generateContent" || hdr.Get("x-goog-api-key") != "gk" {
		t.Fatalf("path=%q key header=%q", path, hdr.Get("x-goog-api-key"))
	}
	contents := got["contents"].([]any)
	if len(contents) != 3 || contents[1].(map[string]any)["role"] != "model" {
		t.Fatalf("contents = %v (system lifted out, assistant -> model)", contents)
	}
	sys := got["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)
	gen := got["generationConfig"].(map[string]any)
	if sys["text"] != "Be brief." || gen["maxOutputTokens"] != float64(50) || gen["responseMimeType"] != "application/json" {
		t.Fatalf("system=%v gen=%v", sys, gen)
	}
}

func TestGeminiTools(t *testing.T) {
	var got map[string]any
	srv := fakeUpstream(t, 200, `{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"get_weather","args":{"city":"Rome"}}}]},"finishReason":"STOP"}]}`, &got, nil)
	g := NewGemini(config.Provider{BaseURL: srv.URL, Model: "m"}, srv.Client())

	resp, err := g.Chat(context.Background(), Request{Messages: toolConversation, Tools: []Tool{weather}, ToolChoice: json.RawMessage(`"required"`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Arguments != `{"city":"Rome"}` || resp.FinishReason != "tool_calls" || !strings.HasPrefix(resp.ToolCalls[0].ID, "call_") {
		t.Fatalf("resp = %+v", resp)
	}
	decl := got["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)
	if decl["name"] != "get_weather" || decl["parameters"] == nil {
		t.Fatalf("declarations = %v", decl)
	}
	if mode := got["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)["mode"]; mode != "ANY" {
		t.Fatalf("mode = %v", mode)
	}
	contents := got["contents"].([]any)
	call := contents[1].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
	result := contents[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if call["args"].(map[string]any)["city"] != "Paris" || result["name"] != "get_weather" || result["response"].(map[string]any)["temp_c"] != float64(18) {
		t.Fatalf("call=%v result=%v", call, result)
	}
}

func TestGeminiStream(t *testing.T) {
	body := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}]}`,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}`,
	}, "\r\n\r\n")
	var path string
	srv := fakeUpstream(t, 200, body, nil, nil)
	srv.Config.Handler = recordPath(&path, srv.Config.Handler)
	g := NewGemini(config.Provider{BaseURL: srv.URL, Model: "m"}, srv.Client())

	resp, deltas := collect(t, g, hi)
	if strings.Join(deltas, "|") != "Hel|lo" || resp.Usage.TotalTokens != 5 || resp.FinishReason != "stop" {
		t.Fatalf("deltas=%q resp=%+v", deltas, resp)
	}
	if path != "/v1beta/models/m:streamGenerateContent" {
		t.Fatalf("path = %q", path)
	}
}

func TestGeminiBlockedPromptIsAClientError(t *testing.T) {
	srv := fakeUpstream(t, 200, `{"promptFeedback":{"blockReason":"SAFETY"}}`, nil, nil)
	g := NewGemini(config.Provider{BaseURL: srv.URL, Model: "m"}, srv.Client())
	_, err := g.Chat(context.Background(), hi)
	if err == nil || Retryable(err) || !strings.Contains(err.Error(), "SAFETY") {
		t.Fatalf("err = %v; a safety block must not be retried", err)
	}
}

func recordPath(path *string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*path = r.URL.Path
		next.ServeHTTP(w, r)
	})
}

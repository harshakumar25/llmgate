package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/harshakumar25/llmgate/internal/config"
)

// TestOllamaForwardsRequestSettings checks that sampling settings, JSON mode
// and the reasoning toggle reach Ollama in its own wire format.
func TestOllamaForwardsRequestSettings(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"{}"},"prompt_eval_count":3,"eval_count":2}`))
	}))
	defer srv.Close()

	think := false
	o := NewOllama(config.Provider{BaseURL: srv.URL, Model: "qwen3:4b"}, srv.Client())
	resp, err := o.Chat(context.Background(), Request{
		Messages:       []Message{{Role: "user", Content: "hi"}},
		Temperature:    0.7,
		MaxTokens:      200,
		ResponseFormat: &ResponseFormat{Type: "json_object"},
		Think:          &think,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.TotalTokens != 5 {
		t.Fatalf("total tokens = %d, want 5", resp.Usage.TotalTokens)
	}

	opts, _ := got["options"].(map[string]any)
	if opts["temperature"] != 0.7 || opts["num_predict"] != float64(200) {
		t.Fatalf("options = %v, want temperature 0.7 and num_predict 200", opts)
	}
	if got["format"] != "json" {
		t.Fatalf("format = %v, want json", got["format"])
	}
	if got["think"] != false {
		t.Fatalf("think = %v, want false", got["think"])
	}
}

// TestOllamaOmitsUnsetSettings checks that a bare request leaves Ollama's
// defaults alone.
func TestOllamaOmitsUnsetSettings(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"ok"}}`))
	}))
	defer srv.Close()

	o := NewOllama(config.Provider{BaseURL: srv.URL, Model: "llama3.2"}, srv.Client())
	if _, err := o.Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"options", "format", "think"} {
		if _, ok := got[k]; ok {
			t.Fatalf("unexpected %q in body: %v", k, got)
		}
	}
}

package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/harshakumar25/llmgate/internal/config"
)

func post(t *testing.T, url, body string, header ...string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestChatCompletionsShape(t *testing.T) {
	ts := newTestServer(t, &slowProvider{}, nil)
	resp, b := post(t, ts.URL+"/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi "},{"type":"text","text":"there"}]}],"max_tokens":50}`,
		"X-Request-ID", "trace-123")

	var got struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if resp.StatusCode != 200 || got.Object != "chat.completion" || got.ID != "chatcmpl-trace-123" || got.Created == 0 {
		t.Fatalf("envelope = %s", b)
	}
	c := got.Choices[0]
	if c.Message.Role != "assistant" || c.Message.Content != "echo: hi there" || c.FinishReason != "stop" || got.Usage.TotalTokens != 10 {
		t.Fatalf("choice = %+v usage=%+v (content parts must be joined)", c, got.Usage)
	}
	if resp.Header.Get("X-Request-ID") != "trace-123" || resp.Header.Get("X-LLMGate-Provider") != "slow" {
		t.Fatalf("headers = %v", resp.Header)
	}
}

func TestChatCompletionsRejectsWhatItCannotDo(t *testing.T) {
	ts := newTestServer(t, &slowProvider{}, nil)
	cases := map[string]string{
		"messages[0].content": `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`,
		"n":                   `{"n":2,"messages":[{"role":"user","content":"hi"}]}`,
		"tools":               `{"tools":[{"type":"function"}],"messages":[{"role":"user","content":"hi"}]}`,
	}
	for param, body := range cases {
		resp, b := post(t, ts.URL+"/v1/chat/completions", body)
		var e struct {
			Error struct {
				Type  string `json:"type"`
				Param string `json:"param"`
			} `json:"error"`
		}
		json.Unmarshal(b, &e)
		if resp.StatusCode != 400 || e.Error.Type != "invalid_request_error" || e.Error.Param != param {
			t.Errorf("%s: %d %s", param, resp.StatusCode, b)
		}
	}
}

func TestChatCompletionsStream(t *testing.T) {
	ts := newTestServer(t, &streamProvider{parts: []string{"Hel", "lo"}}, nil)
	_, b := post(t, ts.URL+"/v1/chat/completions",
		`{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)

	var events []string
	for _, line := range strings.Split(string(b), "\n") {
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			events = append(events, d)
		}
	}
	// role, Hel, lo, finish, usage, [DONE]
	if len(events) != 6 || events[5] != "[DONE]" {
		t.Fatalf("events = %q", events)
	}
	type chunk struct {
		Object  string `json:"object"`
		Choices []struct {
			Delta struct {
				Role    string  `json:"role"`
				Content *string `json:"content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	parse := func(i int) chunk {
		var c chunk
		if err := json.Unmarshal([]byte(events[i]), &c); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if c.Object != "chat.completion.chunk" {
			t.Fatalf("event %d object = %q", i, c.Object)
		}
		return c
	}
	if c := parse(0); c.Choices[0].Delta.Role != "assistant" {
		t.Fatalf("first chunk should carry the role: %s", events[0])
	}
	if c := parse(1); *c.Choices[0].Delta.Content != "Hel" || c.Choices[0].FinishReason != nil {
		t.Fatalf("content chunk = %s", events[1])
	}
	if c := parse(3); c.Choices[0].FinishReason == nil || *c.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish chunk = %s", events[3])
	}
	if c := parse(4); len(c.Choices) != 0 || c.Usage == nil || c.Usage.TotalTokens != 7 {
		t.Fatalf("usage chunk = %s", events[4])
	}
}

func TestErrorsUseOpenAIShape(t *testing.T) {
	ts := newTestServer(t, &slowProvider{}, func(c *config.Config) { c.APIKeys = []string{"k"} })
	resp, b := post(t, ts.URL+"/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`)
	var e struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	json.Unmarshal(b, &e)
	if resp.StatusCode != 401 || e.Error.Type != "authentication_error" || e.Error.Message == "" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestModelsList(t *testing.T) {
	ts := newTestServer(t, &slowProvider{}, func(c *config.Config) {
		c.Providers = []config.Provider{
			{Name: "openai", Model: "gpt-4o-mini", Models: []string{"gpt-4o"}},
			{Name: "ollama", Model: "llama3.2", Models: []string{"*"}},
		}
	})
	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&got)
	ids := []string{}
	for _, m := range got.Data {
		ids = append(ids, m.ID+"@"+m.OwnedBy)
	}
	if got.Object != "list" || strings.Join(ids, ",") != "gpt-4o-mini@openai,gpt-4o@openai,llama3.2@ollama" {
		t.Fatalf("models = %v", ids)
	}
}

func TestDailyTokenBudget(t *testing.T) {
	p := &slowProvider{} // 10 tokens per call
	ts := newTestServer(t, p, func(c *config.Config) {
		c.APIKeys = []string{"alice", "bob"}
		c.Budget.TokensDaily = 15
	})
	auth := []string{"Authorization", "Bearer alice"}
	ask := func(prompt string, h ...string) int {
		resp, _ := post(t, ts.URL+"/v1/chat", `{"messages":[{"role":"user","content":"`+prompt+`"}]}`, h...)
		return resp.StatusCode
	}
	if ask("a", auth...) != 200 || ask("b", auth...) != 200 {
		t.Fatal("first two calls fit the budget")
	}
	resp, b := post(t, ts.URL+"/v1/chat", `{"messages":[{"role":"user","content":"c"}]}`, auth...)
	if resp.StatusCode != 429 || !strings.Contains(string(b), "insufficient_quota") {
		t.Fatalf("over budget: %d %s", resp.StatusCode, b)
	}
	if ask("a", auth...) != 200 {
		t.Fatal("a cached answer costs nothing and should still be served")
	}
	if ask("c", "Authorization", "Bearer bob") != 200 {
		t.Fatal("bob has his own budget")
	}
	if c := p.calls.Load(); c != 3 {
		t.Fatalf("upstream calls = %d, want 3", c)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer alice")
	uresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer uresp.Body.Close()
	var u struct {
		Usage struct {
			Key    string `json:"key"`
			Calls  int    `json:"calls"`
			Tokens int    `json:"tokens"`
		} `json:"usage"`
		Budget struct {
			TokensRemaining int `json:"tokens_remaining"`
		} `json:"budget"`
	}
	json.NewDecoder(uresp.Body).Decode(&u)
	if u.Usage.Calls != 2 || u.Usage.Tokens != 20 || u.Budget.TokensRemaining != 0 || u.Usage.Key == "alice" {
		t.Fatalf("usage = %+v", u)
	}
}

func TestRequestIDGeneratedAndSanitised(t *testing.T) {
	ts := newTestServer(t, &slowProvider{}, nil)
	resp, _ := post(t, ts.URL+"/v1/chat", `{"messages":[{"role":"user","content":"x"}]}`, "X-Request-ID", "bad id; with spaces")
	id := resp.Header.Get("X-Request-ID")
	if len(id) != 24 || strings.ContainsAny(id, " ;") {
		t.Fatalf("id = %q, want a fresh 24-char hex id", id)
	}
	resp, _ = post(t, ts.URL+"/v1/chat", `{"messages":[{"role":"user","content":"x"}]}`, "X-Request-ID", strings.Repeat("a", 65))
	if len(resp.Header.Get("X-Request-ID")) != 24 {
		t.Fatal("an over-long id must be replaced")
	}
}

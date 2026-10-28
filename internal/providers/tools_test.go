package providers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/harshakumar25/llmgate/internal/config"
)

var weather = Tool{Type: "function", Function: ToolFunction{
	Name:        "get_weather",
	Description: "Current weather for a city",
	Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
}}

// A full tool round trip: the user asks, the model called the tool, the
// tool answered, and the model is asked again.
var toolConversation = []Message{
	{Role: "user", Content: "Weather in Paris?"},
	{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Type: "function",
		Function: ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Paris"}`}}}},
	{Role: "tool", ToolCallID: "call_1", Content: `{"temp_c":18}`},
}

func TestOpenAIToolsPassThrough(t *testing.T) {
	var got map[string]any
	srv := fakeUpstream(t, 200, `{"choices":[{"message":{"role":"assistant","content":null,
		"tool_calls":[{"id":"call_9","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Rome\"}"}}]},
		"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`, &got, nil)
	o := NewOpenAI(config.Provider{BaseURL: srv.URL, Model: "gpt"}, srv.Client())

	resp, err := o.Chat(context.Background(), Request{Messages: toolConversation, Tools: []Tool{weather}, ToolChoice: json.RawMessage(`"auto"`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_9" || resp.ToolCalls[0].Function.Arguments != `{"city":"Rome"}` || resp.FinishReason != "tool_calls" {
		t.Fatalf("resp = %+v", resp)
	}
	if got["tool_choice"] != "auto" || len(got["tools"].([]any)) != 1 {
		t.Fatalf("payload = %v", got)
	}
	msgs := got["messages"].([]any)
	if m := msgs[2].(map[string]any); m["tool_call_id"] != "call_1" || m["role"] != "tool" {
		t.Fatalf("tool turn = %v", m)
	}
}

func TestOllamaToolsTranslate(t *testing.T) {
	var got map[string]any
	srv := fakeUpstream(t, 200, `{"message":{"role":"assistant","content":"",
		"tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Rome"}}}]},
		"done_reason":"stop","prompt_eval_count":4,"eval_count":2}`, &got, nil)
	o := NewOllama(config.Provider{BaseURL: srv.URL, Model: "qwen"}, srv.Client())

	resp, err := o.Chat(context.Background(), Request{Messages: toolConversation, Tools: []Tool{weather}})
	if err != nil {
		t.Fatal(err)
	}
	c := resp.ToolCalls
	if len(c) != 1 || !strings.HasPrefix(c[0].ID, "call_") || c[0].Function.Arguments != `{"city":"Rome"}` || resp.FinishReason != "tool_calls" {
		t.Fatalf("resp = %+v (Ollama sends no id and object arguments)", resp)
	}
	msgs := got["messages"].([]any)
	call := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if args, ok := call["arguments"].(map[string]any); !ok || args["city"] != "Paris" {
		t.Fatalf("arguments must be sent as an object: %v", call)
	}
	if tool := msgs[2].(map[string]any); tool["tool_name"] != "get_weather" {
		t.Fatalf("tool turn should name the function: %v", tool)
	}
}

func TestOllamaToolChoiceNoneWithholdsTools(t *testing.T) {
	var got map[string]any
	srv := fakeUpstream(t, 200, `{"message":{"content":"ok"}}`, &got, nil)
	o := NewOllama(config.Provider{BaseURL: srv.URL, Model: "qwen"}, srv.Client())
	o.Chat(context.Background(), Request{Messages: hi.Messages, Tools: []Tool{weather}, ToolChoice: json.RawMessage(`"none"`)})
	if _, ok := got["tools"]; ok {
		t.Fatal(`tool_choice "none" must not offer tools`)
	}
}

func TestAnthropicToolsTranslate(t *testing.T) {
	var got map[string]any
	srv := fakeUpstream(t, 200, `{"content":[{"type":"text","text":"Checking."},
		{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Rome"}}],
		"stop_reason":"tool_use","usage":{"input_tokens":9,"output_tokens":4}}`, &got, nil)
	a := NewAnthropic(config.Provider{BaseURL: srv.URL, Model: "claude"}, srv.Client())

	conv := append([]Message{{Role: "system", Content: "Be brief."}}, toolConversation...)
	conv = append(conv[:3], append([]Message{conv[3], {Role: "tool", ToolCallID: "call_2", Content: "second"}}, conv[4:]...)...)
	resp, err := a.Chat(context.Background(), Request{Messages: conv, Tools: []Tool{weather},
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"get_weather"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Checking." || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "toolu_1" ||
		resp.ToolCalls[0].Function.Arguments != `{"city":"Rome"}` || resp.FinishReason != "tool_calls" {
		t.Fatalf("resp = %+v", resp)
	}

	tools := got["tools"].([]any)
	if tool := tools[0].(map[string]any); tool["name"] != "get_weather" || tool["input_schema"] == nil {
		t.Fatalf("tools = %v", tools)
	}
	if c := got["tool_choice"].(map[string]any); c["type"] != "tool" || c["name"] != "get_weather" {
		t.Fatalf("tool_choice = %v", c)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v; the two tool results should share one user turn", msgs)
	}
	use := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if use["type"] != "tool_use" || use["id"] != "call_1" || use["input"].(map[string]any)["city"] != "Paris" {
		t.Fatalf("assistant tool call = %v", use)
	}
	results := msgs[2].(map[string]any)
	blocks := results["content"].([]any)
	if results["role"] != "user" || len(blocks) != 2 || blocks[0].(map[string]any)["tool_use_id"] != "call_1" {
		t.Fatalf("tool results = %v", results)
	}
}

func TestBadToolArgumentsAreRejected(t *testing.T) {
	srv := fakeUpstream(t, 200, `{}`, nil, nil)
	o := NewOllama(config.Provider{BaseURL: srv.URL, Model: "qwen"}, srv.Client())
	bad := []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "x", Function: ToolCallFunction{Name: "f", Arguments: "not json"}}}}}
	if _, err := o.Chat(context.Background(), Request{Messages: bad}); err == nil {
		t.Fatal("non-JSON arguments should fail before reaching the provider")
	}
}

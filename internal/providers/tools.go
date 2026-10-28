package providers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// Tool is a function the model may call (OpenAI's shape).
type Tool struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a callable function. Parameters is a JSON Schema.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolCall is one call the model asked for. Arguments is a JSON object
// encoded as a string, exactly as OpenAI sends it; Ollama and Anthropic use
// a real object, and their adapters convert both ways.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction names the function and carries its arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ErrInvalidRequest marks a request that no provider could accept as sent
// (for example malformed tool-call arguments). It is never retried.
var ErrInvalidRequest = errors.New("invalid request")

// ValidateMessages checks the tool-calling parts of a conversation before it
// goes anywhere: arguments must be JSON objects, and every "tool" turn must
// name the call it answers.
func ValidateMessages(msgs []Message) error {
	for i, m := range msgs {
		for _, c := range m.ToolCalls {
			if c.Function.Name == "" {
				return fmt.Errorf("%w: messages[%d]: tool call without a function name", ErrInvalidRequest, i)
			}
			if _, err := argsObject(c.Function.Arguments); err != nil {
				return fmt.Errorf("messages[%d]: %w", i, err)
			}
		}
		if m.Role == "tool" && m.ToolCallID == "" {
			return fmt.Errorf("%w: messages[%d]: tool turn without tool_call_id", ErrInvalidRequest, i)
		}
	}
	return nil
}

// argsObject returns a call's arguments as a JSON object, for providers that
// want an object rather than a string. Empty arguments become {}.
func argsObject(args string) (json.RawMessage, error) {
	if args == "" {
		return json.RawMessage("{}"), nil
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(args), &probe); err != nil {
		return nil, fmt.Errorf("%w: tool call arguments are not a JSON object: %v", ErrInvalidRequest, err)
	}
	return json.RawMessage(args), nil
}

// argsString encodes an object-valued arguments field as OpenAI's string.
func argsString(obj json.RawMessage) string {
	if len(obj) == 0 || string(obj) == "null" {
		return "{}"
	}
	return string(obj)
}

// toolChoiceMode reads ToolChoice: the mode ("auto", "none", "required",
// "function") and, for "function", the forced function's name.
func toolChoiceMode(raw json.RawMessage) (mode, name string) {
	if len(raw) == 0 {
		return "", ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, ""
	}
	var obj struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Function.Name != "" {
		return "function", obj.Function.Name
	}
	return "", ""
}

// newCallID makes an OpenAI-style call id for providers that return none
// (Ollama). Clients echo it back in the "tool" turn, so it must be unique
// within a conversation; random is simplest.
func newCallID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}

// toolNames maps call ids to function names across a conversation, for
// providers whose tool-result turn names the function rather than the call.
func toolNames(msgs []Message) map[string]string {
	names := map[string]string{}
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			names[c.ID] = c.Function.Name
		}
	}
	return names
}

// ollamaMessages converts turns to Ollama's shape: tool-call arguments as
// objects, and tool results labelled with the function name.
func ollamaMessages(msgs []Message) ([]map[string]any, error) {
	names := toolNames(msgs)
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		o := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(m.ToolCalls))
			for _, c := range m.ToolCalls {
				args, err := argsObject(c.Function.Arguments)
				if err != nil {
					return nil, err
				}
				calls = append(calls, map[string]any{"function": map[string]any{"name": c.Function.Name, "arguments": args}})
			}
			o["tool_calls"] = calls
		}
		if m.Role == "tool" {
			if name := names[m.ToolCallID]; name != "" {
				o["tool_name"] = name
			}
		}
		out = append(out, o)
	}
	return out, nil
}

// anthropicMessages converts turns to the Messages API: assistant tool calls
// become tool_use blocks, and "tool" turns become tool_result blocks inside a
// user turn (consecutive results share one turn, as the API expects).
func anthropicMessages(msgs []Message) ([]map[string]any, error) {
	var out []map[string]any
	for _, m := range msgs {
		switch {
		case m.Role == "tool":
			block := map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content}
			if n := len(out); n > 0 && out[n-1]["role"] == "user" {
				if blocks, ok := out[n-1]["content"].([]map[string]any); ok && len(blocks) > 0 && blocks[0]["type"] == "tool_result" {
					out[n-1]["content"] = append(blocks, block)
					continue
				}
			}
			out = append(out, map[string]any{"role": "user", "content": []map[string]any{block}})
		case len(m.ToolCalls) > 0:
			var blocks []map[string]any
			if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, c := range m.ToolCalls {
				input, err := argsObject(c.Function.Arguments)
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Function.Name, "input": input})
			}
			out = append(out, map[string]any{"role": m.Role, "content": blocks})
		default:
			out = append(out, map[string]any{"role": m.Role, "content": m.Content})
		}
	}
	return out, nil
}

// anthropicTools converts tools and tool_choice. A "none" choice drops the
// tools entirely, which is the one behaviour every API version agrees on.
func anthropicTools(req Request) (tools []map[string]any, choice map[string]any) {
	mode, name := toolChoiceMode(req.ToolChoice)
	if len(req.Tools) == 0 || mode == "none" {
		return nil, nil
	}
	for _, t := range req.Tools {
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		tools = append(tools, map[string]any{"name": t.Function.Name, "description": t.Function.Description, "input_schema": schema})
	}
	switch mode {
	case "required":
		choice = map[string]any{"type": "any"}
	case "function":
		choice = map[string]any{"type": "tool", "name": name}
	}
	return tools, choice
}

// finishFrom maps provider stop reasons onto OpenAI's vocabulary.
func finishFrom(reason string, calls int) string {
	if calls > 0 {
		return "tool_calls"
	}
	switch reason {
	case "length", "max_tokens":
		return "length"
	case "":
		return ""
	}
	return "stop"
}

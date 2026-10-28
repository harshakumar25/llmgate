package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Streamer is implemented by providers that can stream a completion. onDelta
// receives each text fragment as it arrives; returning an error from it aborts
// the stream. The returned Response holds the full text and final usage.
type Streamer interface {
	ChatStream(ctx context.Context, req Request, onDelta func(string) error) (*Response, error)
}

// errStreamDone stops a line scan early without reporting an error.
var errStreamDone = errors.New("stream done")

// scanLines calls fn for every non-empty line of r. Lines can be long (a
// single SSE event may carry a large JSON payload), so the buffer grows to 1 MiB.
func scanLines(r io.Reader, fn func(line []byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if err := fn(line); err != nil {
			if errors.Is(err, errStreamDone) {
				return nil
			}
			return err
		}
	}
	return sc.Err()
}

// scanSSE calls fn with the payload of each "data:" line of a server-sent
// event stream, stopping at the OpenAI-style "[DONE]" sentinel.
func scanSSE(r io.Reader, fn func(data []byte) error) error {
	return scanLines(r, func(line []byte) error {
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			return nil // event:, id:, retry: and comments carry nothing we need
		}
		data = bytes.TrimSpace(data)
		if string(data) == "[DONE]" {
			return errStreamDone
		}
		return fn(data)
	})
}

// ChatStream streams from an OpenAI-compatible endpoint. It asks for usage in
// the final chunk (stream_options.include_usage) so metering stays exact.
func (o *OpenAI) ChatStream(ctx context.Context, req Request, onDelta func(string) error) (*Response, error) {
	model, payload := o.payload(req)
	payload["stream"] = true
	payload["stream_options"] = map[string]bool{"include_usage": true}
	resp, err := o.post(ctx, payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := &Response{Model: model, Provider: o.Name()}
	var text strings.Builder
	err = scanSSE(resp.Body, func(data []byte) error {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return fmt.Errorf("openai: decode chunk: %w", err)
		}
		if chunk.Usage != nil {
			out.Usage = usage(chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens)
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content == "" {
				continue
			}
			text.WriteString(c.Delta.Content)
			if err := onDelta(c.Delta.Content); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Content = text.String()
	return out, nil
}

// ChatStream streams from Ollama, which sends newline-delimited JSON objects
// and reports token counts on the final one (done: true).
func (o *Ollama) ChatStream(ctx context.Context, req Request, onDelta func(string) error) (*Response, error) {
	model, payload, err := o.payload(req)
	if err != nil {
		return nil, err
	}
	payload["stream"] = true
	resp, err := o.post(ctx, payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := &Response{Model: model, Provider: o.Name()}
	var text strings.Builder
	err = scanLines(resp.Body, func(line []byte) error {
		var chunk struct {
			Message         Message `json:"message"`
			Done            bool    `json:"done"`
			Error           string  `json:"error"`
			PromptEvalCount int     `json:"prompt_eval_count"`
			EvalCount       int     `json:"eval_count"`
		}
		if err := json.Unmarshal(line, &chunk); err != nil {
			return fmt.Errorf("ollama: decode chunk: %w", err)
		}
		if chunk.Error != "" {
			return fmt.Errorf("ollama: stream error: %s", chunk.Error)
		}
		if c := chunk.Message.Content; c != "" {
			text.WriteString(c)
			if err := onDelta(c); err != nil {
				return err
			}
		}
		if chunk.Done {
			out.Usage = usage(chunk.PromptEvalCount, chunk.EvalCount)
			return errStreamDone
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Content = text.String()
	return out, nil
}

// ChatStream streams from the Anthropic Messages API. Input tokens arrive in
// message_start, text in content_block_delta, output tokens in message_delta.
func (a *Anthropic) ChatStream(ctx context.Context, req Request, onDelta func(string) error) (*Response, error) {
	model, body, err := a.payload(req, true)
	if err != nil {
		return nil, err
	}
	resp, err := a.do(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := &Response{Model: model, Provider: a.Name()}
	var text strings.Builder
	var in, outTok int
	err = scanSSE(resp.Body, func(data []byte) error {
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("anthropic: decode event: %w", err)
		}
		switch ev.Type {
		case "message_start":
			in = ev.Message.Usage.InputTokens
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				text.WriteString(ev.Delta.Text)
				return onDelta(ev.Delta.Text)
			}
		case "message_delta":
			outTok = ev.Usage.OutputTokens
		case "message_stop":
			return errStreamDone
		case "error":
			return fmt.Errorf("anthropic: stream error: %s", ev.Error.Message)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Content = text.String()
	out.Usage = usage(in, outTok)
	return out, nil
}

// postJSON sends a JSON body and returns the open response for 2xx replies,
// or a *StatusError carrying the upstream's message.
func postJSON(ctx context.Context, client *http.Client, provider, url, apiKey string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s: request: %w", provider, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &StatusError{Provider: provider, Code: resp.StatusCode, Body: truncate(data, 300)}
	}
	return resp, nil
}

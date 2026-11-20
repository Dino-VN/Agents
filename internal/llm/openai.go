package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAI talks to any OpenAI-compatible /chat/completions endpoint
// (OpenRouter, DeepSeek, Ollama, vLLM, OpenAI itself...).
type OpenAI struct {
	key, baseURL string
	http         *http.Client
}

func NewOpenAI(apiKey, baseURL string) *OpenAI {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAI{key: apiKey, baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 10 * time.Minute}}
}

type oaMsg struct {
	Role       string       `json:"role"`
	Content    any          `json:"content,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openaiSession struct {
	p     *OpenAI
	spec  Spec
	tools []map[string]any
	msgs  []oaMsg
}

func (p *OpenAI) NewSession(spec Spec, seed []Message) Session {
	s := &openaiSession{p: p, spec: spec}
	for _, t := range spec.Tools {
		params := map[string]any{}
		for k, v := range t.Extra {
			params[k] = v
		}
		params["type"], params["properties"] = "object", t.Properties
		if len(t.Required) > 0 {
			params["required"] = t.Required
		}
		s.tools = append(s.tools, map[string]any{
			"type":     "function",
			"function": map[string]any{"name": t.Name, "description": t.Description, "parameters": params},
		})
	}
	s.msgs = append(s.msgs, oaMsg{Role: "system", Content: spec.System})
	for _, m := range normalizeSeed(seed) {
		s.msgs = append(s.msgs, oaMsg{Role: m.Role, Content: m.Text})
	}
	return s
}

func (s *openaiSession) Len() int { return len(s.msgs) - 1 }

func (s *openaiSession) Send(ctx context.Context, blocks []Block) (*Turn, error) {
	n := len(s.msgs)
	var parts []map[string]any
	for _, b := range blocks {
		switch b.Type {
		case ToolResult:
			c := b.Text
			if b.IsError {
				c = "ERROR: " + c
			}
			s.msgs = append(s.msgs, oaMsg{Role: "tool", ToolCallID: b.ToolID, Content: c})
			// Tool messages can't hold images here; show them in the following user turn.
			for _, p := range b.Parts {
				parts = append(parts, map[string]any{"type": "image_url",
					"image_url": map[string]any{"url": "data:" + p.MediaType + ";base64," + p.Data}})
			}
		case Text:
			parts = append(parts, map[string]any{"type": "text", "text": b.Text})
		case Document:
			parts = append(parts, map[string]any{"type": "text", "text": "[PDF đính kèm: model này không đọc PDF trực tiếp được.]"})
		case Image:
			parts = append(parts, map[string]any{"type": "image_url",
				"image_url": map[string]any{"url": "data:" + b.MediaType + ";base64," + b.Data}})
		}
	}
	if len(parts) > 0 {
		s.msgs = append(s.msgs, oaMsg{Role: "user", Content: parts})
	}

	body := map[string]any{"model": s.spec.Model.ID, "messages": s.msgs, "max_tokens": s.spec.MaxTokens}
	if len(s.tools) > 0 {
		body["tools"] = s.tools
	}
	if e := s.spec.Effort; e != "" {
		if e == "xhigh" || e == "max" { // OpenAI-style APIs top out at "high"
			e = "high"
		}
		body["reasoning_effort"] = e
	}
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", s.p.baseURL+"/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if s.p.key != "" {
		req.Header.Set("Authorization", "Bearer "+s.p.key)
	}
	resp, err := s.p.http.Do(req)
	if err != nil {
		s.msgs = s.msgs[:n]
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		s.msgs = s.msgs[:n]
		return nil, &HTTPError{Status: resp.StatusCode, Body: truncate(string(data), 500)}
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content   string       `json:"content"`
				ToolCalls []oaToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &out); err != nil || len(out.Choices) == 0 {
		s.msgs = s.msgs[:n]
		return nil, fmt.Errorf("openai-compat: bad response: %s", truncate(string(data), 500))
	}
	c := out.Choices[0]
	s.msgs = append(s.msgs, oaMsg{Role: "assistant", Content: c.Message.Content, ToolCalls: c.Message.ToolCalls})

	stop := map[string]string{"stop": "end_turn", "tool_calls": "tool_use", "length": "max_tokens"}[c.FinishReason]
	if stop == "" {
		stop = c.FinishReason
	}
	t := &Turn{Text: strings.TrimSpace(c.Message.Content), Stop: stop, Usage: Usage{In: out.Usage.Prompt, Out: out.Usage.Completion}}
	for _, tc := range c.Message.ToolCalls {
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		t.Calls = append(t.Calls, Block{Type: ToolUse, ToolID: tc.ID, ToolName: tc.Function.Name, Input: json.RawMessage(args)})
	}
	return t, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

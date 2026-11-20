// Package llm is a provider-neutral layer over chat models. Each Session owns its
// native history and only ever appends to it, so provider-specific blocks (e.g.
// Claude thinking blocks) are passed back unchanged.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"agentd/internal/config"
)

type BlockType string

const (
	Text       BlockType = "text"
	Image      BlockType = "image"
	Document   BlockType = "document" // PDF, base64
	ToolUse    BlockType = "tool_use"
	ToolResult BlockType = "tool_result"
)

type Block struct {
	Type BlockType
	Text string

	// tool_use / tool_result
	ToolID   string
	ToolName string
	Input    json.RawMessage
	IsError  bool
	Parts    []Block // tool_result: extra image blocks (screenshots)

	// image / document (base64)
	MediaType string
	Data      string
}

func TextBlock(s string) Block { return Block{Type: Text, Text: s} }

func ResultBlock(id, content string, isErr bool) Block {
	return Block{Type: ToolResult, ToolID: id, Text: content, IsError: isErr}
}

type ToolDef struct {
	Name        string
	Description string
	Properties  map[string]any
	Required    []string
	Extra       map[string]any // other JSON-schema keys ($defs, additionalProperties...)
}

type Usage struct{ In, Out int64 }

type Turn struct {
	Text  string
	Calls []Block // tool_use blocks
	Stop  string  // end_turn | tool_use | max_tokens | refusal | ...
	Usage Usage
}

// Message is a text-only history entry used to seed a fresh session.
type Message struct {
	Role string // user | assistant
	Text string
}

type Spec struct {
	Model     config.Model
	System    string
	Tools     []ToolDef
	MaxTokens int64
	Effort    string // low | medium | high | xhigh | max; "" = model default
}

type Session interface {
	// Send appends a user message made of blocks, calls the model and appends its reply.
	Send(ctx context.Context, blocks []Block) (*Turn, error)
	// Len is the number of messages in the native history.
	Len() int
}

type Provider interface {
	NewSession(spec Spec, seed []Message) Session
}

type Router struct {
	cfg       *config.Config
	providers map[string]Provider
}

func NewRouter(cfg *config.Config) (*Router, error) {
	r := &Router{cfg: cfg, providers: map[string]Provider{}}
	for name, p := range cfg.Providers {
		key := ""
		if p.APIKeyEnv != "" {
			key = os.Getenv(p.APIKeyEnv)
		}
		switch p.Type {
		case "anthropic":
			r.providers[name] = NewAnthropic(key, p.BaseURL, p.Auth)
		case "openai":
			r.providers[name] = NewOpenAI(key, p.BaseURL)
		default:
			return nil, fmt.Errorf("provider %q: unknown type %q", name, p.Type)
		}
	}
	return r, nil
}

// SetProvider overrides a provider (used by tests).
func (r *Router) SetProvider(name string, p Provider) { r.providers[name] = p }

// NewSession starts a conversation with a configured model. effort overrides the
// model's default thinking effort ("" keeps it).
func (r *Router) NewSession(model, effort, system string, tools []ToolDef, seed []Message) (Session, error) {
	m, ok := r.cfg.Models[model]
	if !ok {
		return nil, fmt.Errorf("unknown model %q", model)
	}
	p, ok := r.providers[m.Provider]
	if !ok {
		return nil, fmt.Errorf("model %q: provider %q not configured", model, m.Provider)
	}
	if effort == "" {
		effort = m.Effort
	}
	return p.NewSession(Spec{Model: m, System: system, Tools: tools, MaxTokens: m.MaxTokens, Effort: effort}, seed), nil
}

// Complete is a one-shot helper: a fresh session with no tools and a single prompt.
func (r *Router) Complete(ctx context.Context, model, system, prompt string) (string, error) {
	s, err := r.NewSession(model, "", system, nil, nil)
	if err != nil {
		return "", err
	}
	t, err := s.Send(ctx, []Block{TextBlock(prompt)})
	if err != nil {
		return "", err
	}
	return t.Text, nil
}

var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// EffortRank orders effort levels; unknown or empty is -1.
func EffortRank(e string) int {
	for i, x := range Efforts {
		if x == e {
			return i
		}
	}
	return -1
}

func ValidEffort(e string) bool {
	for _, x := range Efforts {
		if x == e {
			return true
		}
	}
	return false
}

// normalizeSeed merges consecutive same-role messages and makes the history start
// with a user turn, as both providers require alternation.
func normalizeSeed(seed []Message) []Message {
	var out []Message
	for _, m := range seed {
		if m.Text == "" {
			continue
		}
		if len(out) == 0 && m.Role != "user" {
			continue
		}
		if len(out) > 0 && out[len(out)-1].Role == m.Role {
			out[len(out)-1].Text += "\n\n" + m.Text
			continue
		}
		out = append(out, m)
	}
	// The next Send adds a user turn, so the seed must end on assistant.
	if len(out) > 0 && out[len(out)-1].Role == "user" {
		out = append(out, Message{Role: "assistant", Text: "(ok)"})
	}
	return out
}

// HTTPError is returned by adapters that talk HTTP directly.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

// Retryable reports whether a failed model call is worth retrying: network errors,
// rate limits, overload and 5xx, plus proxies that return errors inside a 200 stream.
func Retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	status := 0
	var ae *anthropic.Error
	var he *HTTPError
	switch {
	case errors.As(err, &ae):
		status = ae.StatusCode
	case errors.As(err, &he):
		status = he.Status
	}
	switch {
	case status == 0, status == 200, status == 408, status == 409, status == 429, status >= 500:
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "overloaded") || strings.Contains(msg, "try again")
}

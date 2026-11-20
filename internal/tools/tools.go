// Package tools holds the tool registry and the built-in, stateless tools
// (web, files, shell, calc, time). Tools that need the agent runtime
// (delegate, memory, skills) are registered by the agent package.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"

	"agentd/internal/llm"
)

// Call carries who is invoking a tool.
type Call struct {
	AgentID   string
	Role      string
	Conv      string
	Workspace string
}

type Func func(ctx context.Context, c *Call, in json.RawMessage) (string, error)

// RichFunc is a tool that can also return images (screenshots).
type RichFunc func(ctx context.Context, c *Call, in json.RawMessage) (string, []llm.Block, error)

type Tool struct {
	Def     llm.ToolDef
	Run     Func
	RunRich RichFunc // used instead of Run when set
}

type Registry struct{ tools map[string]Tool }

func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

func (r *Registry) Add(t Tool) { r.tools[t.Def.Name] = t }

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Match returns tools whose names match any of the glob patterns, sorted by name
// so the tool list (and the prompt cache) is stable.
func (r *Registry) Match(patterns []string) []Tool {
	var out []Tool
	for name, t := range r.tools {
		if Matches(patterns, name) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Def.Name < out[j].Def.Name })
	return out
}

func Matches(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// Validate checks the input is a JSON object holding every required field.
// Needed because eager input streaming skips server-side validation.
func Validate(def llm.ToolDef, in json.RawMessage) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(in, &m); err != nil {
		return fmt.Errorf("input is not valid JSON: %v", err)
	}
	for _, f := range def.Required {
		if _, ok := m[f]; !ok {
			return fmt.Errorf("missing required field %q", f)
		}
	}
	return nil
}

// Schema helpers.
func Str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func Int(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

func Def(name, desc string, props map[string]any, required ...string) llm.ToolDef {
	if props == nil {
		props = map[string]any{}
	}
	return llm.ToolDef{Name: name, Description: desc, Properties: props, Required: required}
}

// Args unmarshals tool input into a struct.
func Args[T any](in json.RawMessage) (T, error) {
	var v T
	err := json.Unmarshal(in, &v)
	return v, err
}

func Clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n…[cắt bớt %d ký tự]", len(s)-n)
}

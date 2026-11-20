package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"agentd/internal/llm"
	"agentd/internal/mcp"
	"agentd/internal/tools"
)

var badToolChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// startMCP launches configured MCP servers and registers their tools as
// "<server>_<tool>". A server that fails to start is logged and skipped.
func (rt *Runtime) startMCP() {
	for name, sc := range rt.C().MCP {
		srv := &mcp.Server{Name: name, Command: rt.C().Path(sc.Command), Args: sc.Args, Env: sc.Env}
		if !strings.Contains(sc.Command, "/") {
			srv.Command = sc.Command // bare command: resolve via PATH
		}
		ctx, cancel := context.WithTimeout(rt.ctx, 30*time.Second)
		err := srv.Start(ctx)
		var list []mcp.Tool
		if err == nil {
			list, err = srv.ListTools(ctx)
		}
		cancel()
		if err != nil {
			log.Printf("mcp %s: %v (skipped)", name, err)
			continue
		}
		rt.mcp = append(rt.mcp, srv)
		n := 0
		for _, t := range list {
			if len(sc.Tools) > 0 && !tools.Matches(sc.Tools, t.Name) {
				continue
			}
			rt.Tools.Add(mcpTool(srv, name, t))
			n++
		}
		log.Printf("mcp %s: %d tools registered", name, n)
	}
}

func mcpTool(srv *mcp.Server, prefix string, t mcp.Tool) tools.Tool {
	name := badToolChars.ReplaceAllString(prefix+"_"+t.Name, "_")
	if len(name) > 64 {
		name = name[:64]
	}
	def := llm.ToolDef{Name: name, Description: t.Description, Properties: map[string]any{}, Extra: map[string]any{}}
	for k, v := range t.InputSchema {
		switch k {
		case "properties":
			if m, ok := v.(map[string]any); ok {
				def.Properties = m
			}
		case "required":
			if arr, ok := v.([]any); ok {
				for _, r := range arr {
					if s, ok := r.(string); ok {
						def.Required = append(def.Required, s)
					}
				}
			}
		case "type", "$schema":
		default:
			def.Extra[k] = v
		}
	}
	remote := t.Name
	return tools.Tool{
		Def: def,
		RunRich: func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, []llm.Block, error) {
			res, err := srv.CallTool(ctx, remote, in)
			if err != nil {
				return "", nil, err
			}
			var text []string
			var images []llm.Block
			for _, item := range res.Content {
				switch item.Type {
				case "text":
					text = append(text, item.Text)
				case "image":
					if _, err := base64.StdEncoding.DecodeString(item.Data); err == nil {
						images = append(images, llm.Block{Type: llm.Image, MediaType: item.MimeType, Data: item.Data})
					}
				default:
					raw, _ := json.Marshal(item)
					text = append(text, string(raw))
				}
			}
			out := tools.Clip(strings.Join(text, "\n"), 40000)
			if res.IsError {
				return "", nil, fmt.Errorf("%s", out)
			}
			if out == "" && len(images) == 0 {
				out = "(ok)"
			}
			return out, images, nil
		},
	}
}

// Close stops MCP servers.
func (rt *Runtime) Close() {
	for _, s := range rt.mcp {
		s.Close()
	}
}

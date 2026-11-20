package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestMain doubles as a fake MCP server when MCP_FAKE=1.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_FAKE") == "1" {
		fakeServer()
		return
	}
	os.Exit(m.Run())
}

func fakeServer() {
	sc := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var req struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		json.Unmarshal(sc.Bytes(), &req)
		if req.ID == nil {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			// Also send a server→client request and a notification, which the client must survive.
			out.Encode(map[string]any{"jsonrpc": "2.0", "id": 999, "method": "roots/list"})
			out.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{}})
			result = map[string]any{"serverInfo": map[string]any{"name": "fake", "version": "1"}, "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "screenshot", "description": "shot",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"pid": map[string]any{"type": "integer"}}, "required": []string{"pid"}}}}}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p)
			if p.Name == "crash" {
				os.Exit(1)
			}
			result = map[string]any{"content": []any{
				map[string]any{"type": "text", "text": fmt.Sprintf("pid=%v", p.Arguments["pid"])},
				map[string]any{"type": "image", "data": "aGVsbG8=", "mimeType": "image/png"},
			}}
		}
		out.Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
	}
}

func TestClient(t *testing.T) {
	exe, _ := os.Executable()
	s := &Server{Name: "fake", Command: exe, Env: map[string]string{"MCP_FAKE": "1"}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tools, err := s.ListTools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "screenshot" {
		t.Fatalf("tools: %+v %v", tools, err)
	}
	res, err := s.CallTool(ctx, "screenshot", json.RawMessage(`{"pid":42}`))
	if err != nil || len(res.Content) != 2 || res.Content[0].Text != "pid=42" || res.Content[1].MimeType != "image/png" {
		t.Fatalf("call: %+v %v", res, err)
	}
	// A crashed server is restarted on the next call.
	if _, err := s.CallTool(ctx, "crash", nil); err == nil {
		t.Fatal("expected error from crashed server")
	}
	time.Sleep(100 * time.Millisecond)
	if res, err := s.CallTool(ctx, "screenshot", json.RawMessage(`{"pid":7}`)); err != nil || res.Content[0].Text != "pid=7" {
		t.Fatalf("after restart: %+v %v", res, err)
	}
}

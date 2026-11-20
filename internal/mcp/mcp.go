// Package mcp is a minimal Model Context Protocol client over stdio: it starts a
// server process, lists its tools and calls them. Used to plug tools such as
// cua-driver (desktop control) into agentd.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Content is one item of a tool result.
type Content struct {
	Type     string `json:"type"` // text | image | resource ...
	Text     string `json:"text"`
	Data     string `json:"data"` // base64 for images
	MimeType string `json:"mimeType"`
}

type Result struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError"`
}

type Server struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	nextID  int64
	pending map[int64]chan rpcResponse
	dead    chan struct{}
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Start launches the server and performs the initialize handshake.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	cmd := exec.Command(s.Command, s.Args...)
	cmd.Env = os.Environ()
	for k, v := range s.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	cmd.Stderr = &prefixLog{name: s.Name}
	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("mcp %s: start: %w", s.Name, err)
	}
	s.cmd, s.stdin = cmd, stdin
	s.pending = map[int64]chan rpcResponse{}
	s.dead = make(chan struct{})
	dead := s.dead
	s.mu.Unlock()

	go s.readLoop(stdout, dead)
	go func() {
		cmd.Wait()
		close(dead)
	}()

	var init struct {
		ServerInfo struct{ Name, Version string } `json:"serverInfo"`
	}
	err = s.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "agentd", "version": "1"},
	}, &init)
	if err != nil {
		s.Close()
		return fmt.Errorf("mcp %s: initialize: %w", s.Name, err)
	}
	s.notify("notifications/initialized", map[string]any{})
	log.Printf("mcp %s: connected to %s %s", s.Name, init.ServerInfo.Name, init.ServerInfo.Version)
	return nil
}

func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil && s.cmd.Process != nil {
		s.stdin.Close()
		s.cmd.Process.Kill()
	}
}

func (s *Server) alive() bool {
	s.mu.Lock()
	dead := s.dead
	s.mu.Unlock()
	if dead == nil {
		return false
	}
	select {
	case <-dead:
		return false
	default:
		return true
	}
}

func (s *Server) readLoop(r io.Reader, dead chan struct{}) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20) // screenshots can be large
	for sc.Scan() {
		var msg struct {
			ID     *json.RawMessage `json:"id"`
			Method string           `json:"method"`
			rpcResponse
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.ID == nil {
			continue // notifications and noise
		}
		if msg.Method != "" { // server → client request: we support none
			s.send(map[string]any{"jsonrpc": "2.0", "id": msg.ID,
				"error": map[string]any{"code": -32601, "message": "not supported by agentd"}})
			continue
		}
		var id int64
		if json.Unmarshal(*msg.ID, &id) != nil {
			continue
		}
		s.mu.Lock()
		ch := s.pending[id]
		delete(s.pending, id)
		s.mu.Unlock()
		if ch != nil {
			ch <- msg.rpcResponse
		}
	}
}

func (s *Server) send(v any) error {
	raw, _ := json.Marshal(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stdin == nil {
		return fmt.Errorf("not started")
	}
	_, err := s.stdin.Write(append(raw, '\n'))
	return err
}

func (s *Server) notify(method string, params any) {
	s.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (s *Server) call(ctx context.Context, method string, params, out any) error {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	ch := make(chan rpcResponse, 1)
	s.pending[id] = ch
	dead := s.dead
	s.mu.Unlock()
	if err := s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return fmt.Errorf("%s (code %d)", r.Error.Message, r.Error.Code)
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-dead:
		return fmt.Errorf("mcp %s: server exited", s.Name)
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return ctx.Err()
	}
}

func (s *Server) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := s.call(ctx, "tools/list", params, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" {
			return all, nil
		}
		cursor = page.NextCursor
	}
}

// CallTool runs a tool, restarting the server once if it has died.
func (s *Server) CallTool(ctx context.Context, name string, args json.RawMessage) (*Result, error) {
	if !s.alive() {
		log.Printf("mcp %s: server not running, restarting", s.Name)
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.Start(sctx)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	var res Result
	if err := s.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

type prefixLog struct{ name string }

func (p *prefixLog) Write(b []byte) (int, error) {
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if l != "" {
			log.Printf("mcp %s: %s", p.name, l)
		}
	}
	return len(b), nil
}

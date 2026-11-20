package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentd/internal/config"
	"agentd/internal/llm"
	"agentd/internal/skills"
	"agentd/internal/store"
	"agentd/internal/tools"
)

// fakeProvider scripts replies per model id: script(callIndex, blocks) → Turn.
type fakeProvider struct {
	scripts map[string]func(n int, in []llm.Block) *llm.Turn
}

type fakeSession struct {
	p    *fakeProvider
	spec llm.Spec
	n    int
}

func (p *fakeProvider) NewSession(spec llm.Spec, seed []llm.Message) llm.Session {
	return &fakeSession{p: p, spec: spec}
}

func (s *fakeSession) Len() int { return s.n * 2 }

func (s *fakeSession) Send(ctx context.Context, in []llm.Block) (*llm.Turn, error) {
	s.n++
	script, ok := s.p.scripts[s.spec.Model.ID]
	if !ok {
		script = func(int, []llm.Block) *llm.Turn { return text("ok") }
	}
	t := script(s.n, in)
	t.Usage = llm.Usage{In: 100, Out: 10}
	return t, nil
}

func call(name string, input any) *llm.Turn {
	raw, _ := json.Marshal(input)
	return &llm.Turn{Stop: "tool_use", Calls: []llm.Block{{Type: llm.ToolUse, ToolID: name + "-id", ToolName: name, Input: raw}}}
}

func text(s string) *llm.Turn { return &llm.Turn{Stop: "end_turn", Text: s} }

func joined(in []llm.Block) string {
	var b strings.Builder
	for _, x := range in {
		b.WriteString(x.Text + "\n")
	}
	return b.String()
}

type recGW struct {
	mu   sync.Mutex
	msgs []string
}

func (g *recGW) Send(conv, text string) {
	g.mu.Lock()
	g.msgs = append(g.msgs, text)
	g.mu.Unlock()
}

func (g *recGW) SendFile(conv, path, caption string) error {
	g.mu.Lock()
	g.msgs = append(g.msgs, "FILE "+path+" "+caption)
	g.mu.Unlock()
	return nil
}

func (g *recGW) waitFor(t *testing.T, sub string) string {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		for _, m := range g.msgs {
			if strings.Contains(m, sub) {
				g.mu.Unlock()
				return m
			}
		}
		g.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q; got %q", sub, g.msgs)
	return ""
}

func setup(t *testing.T, scripts map[string]func(int, []llm.Block) *llm.Turn) (*Runtime, *recGW, string) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "ws"), 0o755)
	os.MkdirAll(filepath.Join(dir, "skills", "demo"), 0o755)
	os.WriteFile(filepath.Join(dir, "skills", "demo", "SKILL.md"), []byte("---\nname: demo\ndescription: demo skill\n---\nDo the demo."), 0o644)
	cfg := &config.Config{
		Dir: dir, Workspace: "ws", SkillsDir: "skills",
		Providers: map[string]config.Provider{"fake": {Type: "anthropic"}},
		Models: map[string]config.Model{
			"chat":   {Provider: "fake", ID: "chat"},
			"coder":  {Provider: "fake", ID: "coder"},
			"helper": {Provider: "fake", ID: "helper"},
		},
		Roles: map[string]config.Role{
			"chat":   {Model: "chat", Tools: []string{"delegate", "recall", "remember", "use_skill", "calc"}},
			"coder":  {Model: "coder", Tools: []string{"spawn_agent", "write_file", "report", "send_message"}, Description: "codes"},
			"helper": {Model: "helper", Tools: []string{"report"}, Description: "helps"},
		},
		Limits: config.Limits{MaxDepth: 3, MaxChildren: 3, MaxTurns: 10, WorkerTimeout: time.Minute, ChatMaxMessages: 1000, ApprovalTimeout: time.Second},
	}
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	router, _ := llm.NewRouter(cfg)
	router.SetProvider("fake", &fakeProvider{scripts: scripts})
	sk, err := skills.Load(cfg.Path("skills"))
	if err != nil {
		t.Fatal(err)
	}
	gw := &recGW{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return New(ctx, cfg, router, st, sk, gw), gw, dir
}

// chat → delegate coder → coder spawns helper → helper reports → coder writes a
// file and reports → chat tells the user.
func TestDelegationTree(t *testing.T) {
	scripts := map[string]func(int, []llm.Block) *llm.Turn{
		"chat": func(n int, in []llm.Block) *llm.Turn {
			switch {
			case n == 1:
				return call("delegate", map[string]string{"role": "coder", "brief": "tạo hello.txt"})
			case strings.Contains(joined(in), "trạng thái:"):
				return text("Xong rồi: " + joined(in))
			default:
				return text("Đã giao cho coder.")
			}
		},
		"coder": func(n int, in []llm.Block) *llm.Turn {
			switch {
			case n == 1:
				return call("spawn_agent", map[string]string{"role": "helper", "brief": "nghĩ nội dung"})
			case n == 2:
				return text("") // wait for helper
			case strings.Contains(joined(in), "[báo cáo từ helper-"):
				return call("write_file", map[string]string{"path": "hello.txt", "content": "xin chào"})
			default:
				return call("report", map[string]string{"status": "done", "result": "đã tạo hello.txt"})
			}
		},
		"helper": func(n int, in []llm.Block) *llm.Turn { return text("nội dung: xin chào") },
	}
	rt, gw, dir := setup(t, scripts)
	rt.HandleUser("c1", "tạo file hello giúp mình", nil)

	gw.waitFor(t, "Đã giao cho coder")
	final := gw.waitFor(t, "Xong rồi")
	if !strings.Contains(final, "đã tạo hello.txt") {
		t.Fatalf("final message missing coder result: %q", final)
	}
	b, err := os.ReadFile(filepath.Join(dir, "ws", "hello.txt"))
	if err != nil || string(b) != "xin chào" {
		t.Fatalf("hello.txt = %q, %v", b, err)
	}
	// The tree should drain once everyone reported.
	deadline := time.Now().Add(2 * time.Second)
	for rt.lookup("chat-c1").activeChildren() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := rt.lookup("chat-c1").activeChildren(); n != 0 {
		t.Fatalf("chat still has %d children", n)
	}
}

func TestDepthLimitAndPermissions(t *testing.T) {
	scripts := map[string]func(int, []llm.Block) *llm.Turn{
		"chat": func(n int, in []llm.Block) *llm.Turn {
			if n == 1 {
				// shell is not in chat's tool list → must be refused.
				return call("shell", map[string]string{"command": "echo hi"})
			}
			return text("result: " + joined(in))
		},
	}
	rt, gw, _ := setup(t, scripts)
	rt.HandleUser("c2", "chạy lệnh", nil)
	msg := gw.waitFor(t, "result:")
	if !strings.Contains(msg, "không được phép") {
		t.Fatalf("expected permission error, got %q", msg)
	}

	rt.C().Limits.MaxDepth = 1
	chat := rt.lookup("chat-c2")
	w, err := rt.Spawn(chat, "helper", "", "", "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Spawn(w, "helper", "", "", "y"); err == nil || !strings.Contains(err.Error(), "độ sâu") {
		t.Fatalf("expected depth error, got %v", err)
	}
}

func TestMemoryAndSkills(t *testing.T) {
	scripts := map[string]func(int, []llm.Block) *llm.Turn{
		"chat": func(n int, in []llm.Block) *llm.Turn {
			switch n {
			case 1:
				return call("remember", map[string]string{"kind": "fact", "content": "User thích cà phê sữa đá"})
			case 2:
				return call("recall", map[string]string{"query": "cà phê"})
			case 3:
				if !strings.Contains(joined(in), "cà phê sữa đá") {
					return text("FAIL recall: " + joined(in))
				}
				return call("use_skill", map[string]string{"name": "demo"})
			default:
				return text("skill: " + joined(in))
			}
		},
	}
	rt, gw, _ := setup(t, scripts)
	rt.HandleUser("c3", "nhớ nhé", nil)
	msg := gw.waitFor(t, "skill:")
	if !strings.Contains(msg, "Do the demo.") {
		t.Fatalf("skill body not loaded: %q", msg)
	}
	if !strings.Contains(rt.Mem.Profile(), "cà phê sữa đá") {
		t.Fatalf("profile missing fact: %q", rt.Mem.Profile())
	}
}

func TestSelfConfigAndReload(t *testing.T) {
	rt, _, dir := setup(t, nil)
	cfgYAML := `
providers: {fake: {type: anthropic}}
models:
  chat: {provider: fake, id: chat}
  helper: {provider: fake, id: helper, effort: low}
roles:
  chat: {model: chat, tools: [self_read]}
  helper: {model: helper, tools: [report], description: helps}
`
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfgYAML), 0o644)
	rt.cfgPath = filepath.Join(dir, "config.yaml")

	if _, err := rt.selfWrite("secrets.env", "X=1"); err == nil {
		t.Fatal("writing secrets must be refused")
	}
	if _, err := rt.selfWrite("data/agentd.db", "x"); err == nil {
		t.Fatal("writing data/ must be refused")
	}
	if _, err := rt.selfWrite("config.yaml", "roles: {chat: {model: nope}}"); err == nil {
		t.Fatal("invalid config must be refused")
	}
	if _, err := rt.selfWrite("skills/new-skill/SKILL.md", "---\nname: new-skill\ndescription: d\n---\nbody"); err != nil {
		t.Fatal(err)
	}
	msg, err := rt.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.C().Roles["coder"]; ok || rt.C().Models["helper"].Effort != "low" {
		t.Fatalf("reload did not apply new config: %s", msg)
	}
	if _, err := rt.S().Use("new-skill", "chat"); err != nil {
		t.Fatalf("new skill not loaded: %v", err)
	}
	// Effort validation on spawn.
	chat, _ := rt.chatFor("c9")
	if _, err := rt.Spawn(chat, "helper", "", "turbo", "x"); err == nil {
		t.Fatal("invalid effort accepted")
	}
}

func TestEffortFloorAndInspect(t *testing.T) {
	slow := func(int, []llm.Block) *llm.Turn { time.Sleep(2 * time.Second); return text("xong") }
	rt, _, _ := setup(t, map[string]func(int, []llm.Block) *llm.Turn{"coder": slow})
	rt.C().Roles["coder"] = config.Role{Model: "coder", Effort: "high", Tools: []string{"report"}, Description: "codes"}
	chat, _ := rt.chatFor("c7")
	low, err := rt.Spawn(chat, "coder", "", "low", "x")
	if err != nil {
		t.Fatal(err)
	}
	if low.Effort != "high" {
		t.Fatalf("effort below role default: %q", low.Effort)
	}
	max, _ := rt.Spawn(chat, "coder", "", "max", "y")
	if max.Effort != "max" {
		t.Fatalf("raising effort should work: %q", max.Effort)
	}
	low.logActivity("browser_navigate → Đã mở: Shop")
	tool, _ := rt.Tools.Get("agent_inspect")
	out, err := tool.Run(context.Background(), &tools.Call{Conv: "c7"}, json.RawMessage(`{"id":"`+low.ID+`"}`))
	if err != nil || !strings.Contains(out, "browser_navigate") || !strings.Contains(out, "effort high") {
		t.Fatalf("inspect: %q %v", out, err)
	}
	if _, err := tool.Run(context.Background(), &tools.Call{Conv: "other"}, json.RawMessage(`{"id":"`+low.ID+`"}`)); err == nil {
		t.Fatal("inspect must not cross conversations")
	}
}

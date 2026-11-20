// Package agent runs the agent tree: one chat agent per conversation, worker
// agents it delegates to, and subagents workers spawn. Every agent is a goroutine
// with an inbox; messages between agents travel through those inboxes.
package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentd/internal/browser"
	"agentd/internal/config"
	"agentd/internal/llm"
	"agentd/internal/mcp"
	"agentd/internal/memory"
	"agentd/internal/skills"
	"agentd/internal/store"
	"agentd/internal/tools"
)

// Gateway delivers text and files to the user of a conversation (CLI, Telegram...).
type Gateway interface {
	Send(conv, text string)
	SendFile(conv, path, caption string) error
}

type Runtime struct {
	Store *store.Store
	Mem   *memory.Memory
	Tools *tools.Registry
	GW    Gateway

	state atomic.Pointer[state] // swapped as a whole by Reload

	ctx       context.Context
	cfgPath   string
	mcp       []*mcp.Server
	browser   *browser.Browser
	mu        sync.Mutex
	agents    map[string]*Agent
	chats     map[string]*Agent
	approvals map[int]*approval
	seq       atomic.Int64
	apSeq     int
}

type approval struct {
	conv  string
	agent *Agent
	ch    chan bool
}

// state is the reloadable part of the runtime: config, model router, skills.
type state struct {
	cfg    *config.Config
	router *llm.Router
	skills *skills.Set
}

func (rt *Runtime) C() *config.Config { return rt.state.Load().cfg }
func (rt *Runtime) R() *llm.Router    { return rt.state.Load().router }
func (rt *Runtime) S() *skills.Set    { return rt.state.Load().skills }

func New(ctx context.Context, cfg *config.Config, router *llm.Router, st *store.Store, sk *skills.Set, gw Gateway) *Runtime {
	rt := &Runtime{
		Store: st, GW: gw,
		Mem:       memory.New(st, router, cfg.Distiller),
		Tools:     tools.NewRegistry(),
		ctx:       ctx,
		agents:    map[string]*Agent{},
		chats:     map[string]*Agent{},
		approvals: map[int]*approval{},
	}
	rt.state.Store(&state{cfg: cfg, router: router, skills: sk})
	rt.cfgPath = cfg.File
	tools.RegisterLocal(rt.Tools)
	tools.RegisterCode(rt.Tools)
	tools.RegisterWeb(rt.Tools, cfg.Web.Search)
	rt.registerAgentTools()
	rt.registerWorkTools()
	rt.registerBrowserTools()
	rt.registerSendFile()
	rt.registerInspect()
	rt.startMCP()
	return rt
}

// HandleUser is the single entry point for user input from any gateway.
func (rt *Runtime) HandleUser(conv, text string, attachments []llm.Block) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "/") && rt.command(conv, text) {
		return
	}
	chat, err := rt.chatFor(conv)
	if err != nil {
		rt.GW.Send(conv, "⚠️ Không khởi tạo được chat agent: "+err.Error())
		return
	}
	rt.Store.AddMessage(conv, "user", chat.ID, "user", text)
	chat.deliver(Event{Kind: "user", From: "user", Text: text, Attachments: attachments})
}

func (rt *Runtime) command(conv, text string) bool {
	f := strings.Fields(text)
	switch f[0] {
	case "/approve", "/deny", "/yes", "/no":
		id := 0
		if len(f) > 1 && f[1] != "all" {
			fmt.Sscan(f[1], &id)
		}
		ok := f[0] == "/approve" || f[0] == "/yes"
		rt.mu.Lock()
		ap := rt.approvals[id]
		if id == 0 { // no id: answer the only pending request of this conversation
			for k, a := range rt.approvals {
				if a.conv == conv {
					id, ap = k, a
				}
			}
		}
		delete(rt.approvals, id)
		rt.mu.Unlock()
		if ap == nil {
			rt.GW.Send(conv, "Không có yêu cầu duyệt nào đang chờ.")
			return true
		}
		if ok && f[len(f)-1] == "all" {
			ap.agent.mu.Lock()
			ap.agent.trusted = true
			ap.agent.mu.Unlock()
			rt.GW.Send(conv, fmt.Sprintf("Đã tin tưởng %s: các lệnh tiếp theo của nó không cần duyệt nữa.", ap.agent.ID))
		}
		ap.ch <- ok
		return true
	case "/think", "/effort":
		chat, err := rt.chatFor(conv)
		if err != nil {
			rt.GW.Send(conv, err.Error())
			return true
		}
		if len(f) < 2 {
			rt.GW.Send(conv, fmt.Sprintf("Mức suy nghĩ của chat: %s (model %s).\nĐổi: /think low|medium|high|xhigh|max · /think default\nWorker: chat tự chọn effort khi giao việc, hoặc bạn dặn \"giao coder, suy nghĩ kỹ (high)\".",
				chat.effortLabel(), chat.Model))
			return true
		}
		e := strings.ToLower(f[1])
		if e == "default" || e == "auto" {
			e = ""
		} else if !llm.ValidEffort(e) {
			rt.GW.Send(conv, "Mức không hợp lệ. Dùng: "+strings.Join(llm.Efforts, ", ")+" hoặc default")
			return true
		}
		rt.Store.SetSetting("think:"+conv, e)
		chat.deliverControl(func() {
			chat.Effort = e
			if err := chat.resetSession(rt.chatSeed(conv)); err != nil {
				log.Printf("[%s] think: %v", chat.ID, err)
			}
		})
		rt.GW.Send(conv, "Đã đặt mức suy nghĩ của chat: "+chat.labelFor(e))
		return true
	case "/reload":
		if msg, err := rt.Reload(); err != nil {
			rt.GW.Send(conv, "❌ "+err.Error())
		} else {
			rt.GW.Send(conv, "✅ "+msg)
		}
		return true
	case "/screen", "/view":
		go func() {
			ctx, cancel := context.WithTimeout(rt.ctx, 30*time.Second)
			defer cancel()
			ws := rt.C().Path(rt.C().Workspace)
			rel, _, err := rt.saveBrowserShot(ctx, ws)
			if err != nil {
				rt.GW.Send(conv, "⚠️ "+err.Error())
				return
			}
			if err := rt.GW.SendFile(conv, filepath.Join(ws, rel), "Trình duyệt lúc này"); err != nil {
				rt.GW.Send(conv, "⚠️ "+err.Error())
			}
		}()
		return true
	case "/status", "/agents":
		rt.GW.Send(conv, rt.Tree(conv))
		return true
	case "/stop":
		n := rt.CancelAll(conv)
		rt.GW.Send(conv, fmt.Sprintf("Đã dừng %d agent.", n))
		return true
	case "/memory":
		rt.GW.Send(conv, rt.Mem.Profile()+"\n(/forget <id> để xoá một ghi nhớ)")
		return true
	case "/forget":
		var id int64
		if len(f) > 1 {
			fmt.Sscan(f[1], &id)
		}
		if rt.Mem.Forget(id) {
			rt.GW.Send(conv, fmt.Sprintf("Đã quên #%d.", id))
		} else {
			rt.GW.Send(conv, "Không tìm thấy ghi nhớ đó.")
		}
		return true
	case "/skills":
		rt.GW.Send(conv, "Skills của chat:\n"+rt.S().Index("chat"))
		return true
	case "/help":
		rt.GW.Send(conv, "/status · /screen (ảnh trình duyệt) · /stop · /think [mức] · /memory · /forget <id> · /skills · /reload · /approve [id] [all] · /deny [id] · /<skill> <yêu cầu>")
		return true
	}
	// "/<skill-name> ..." → ask the chat agent to apply that skill.
	name := strings.TrimPrefix(f[0], "/")
	if _, err := rt.S().Use(name, "chat"); err == nil {
		chat, err := rt.chatFor(conv)
		if err == nil {
			rest := strings.TrimSpace(strings.TrimPrefix(text, f[0]))
			rt.Store.AddMessage(conv, "user", chat.ID, "user", text)
			chat.deliver(Event{Kind: "user", From: "user", Text: fmt.Sprintf("Dùng skill %q (gọi use_skill trước). Yêu cầu: %s", name, rest)})
		}
		return true
	}
	return false
}

func (rt *Runtime) newID(role string) string {
	return fmt.Sprintf("%s-%d", role, rt.seq.Add(1))
}

func (rt *Runtime) chatFor(conv string) (*Agent, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if a, ok := rt.chats[conv]; ok {
		return a, nil
	}
	a := &Agent{rt: rt, ID: "chat-" + conv, Role: "chat", Conv: conv, Model: rt.C().Roles["chat"].Model,
		Effort: rt.Store.Setting("think:" + conv),
		inbox:  make(chan Event, 256), children: map[string]*Agent{}, budget: &Budget{}}
	a.ctx, a.cancel = context.WithCancel(rt.ctx)
	if err := a.resetSession(rt.chatSeed(conv)); err != nil {
		return nil, err
	}
	rt.chats[conv] = a
	rt.agents[a.ID] = a
	go a.run()
	return a, nil
}

func (rt *Runtime) chatSeed(conv string) []llm.Message {
	var seed []llm.Message
	for _, m := range rt.Store.Dialog(conv, 8) {
		role := "assistant"
		if m.Kind == "user" {
			role = "user"
		}
		seed = append(seed, llm.Message{Role: role, Text: m.Content})
	}
	// A trailing user message is the one about to be sent; don't send it twice.
	for len(seed) > 0 && seed[len(seed)-1].Role == "user" {
		seed = seed[:len(seed)-1]
	}
	return seed
}

// Spawn creates a child agent of parent and hands it the task brief.
func (rt *Runtime) Spawn(parent *Agent, role, model, effort, brief string) (*Agent, error) {
	lim := rt.C().Limits
	rc, ok := rt.C().Roles[role]
	if !ok || role == "chat" {
		return nil, fmt.Errorf("không có role %q; các role: %s", role, strings.Join(rt.workerRoles(), ", "))
	}
	if model == "" {
		model = rc.Model
	}
	if _, ok := rt.C().Models[model]; !ok {
		return nil, fmt.Errorf("không có model %q", model)
	}
	if effort != "" && !llm.ValidEffort(effort) {
		return nil, fmt.Errorf("effort %q không hợp lệ; dùng: %s", effort, strings.Join(llm.Efforts, ", "))
	}
	// The role (or model) default is a floor: a delegator can ask for more thinking, not less.
	floor := rc.Effort
	if floor == "" {
		floor = rt.C().Models[model].Effort
	}
	if effort == "" || llm.EffortRank(effort) < llm.EffortRank(floor) {
		effort = floor
	}
	depth := parent.Depth + 1
	if depth > lim.MaxDepth {
		return nil, fmt.Errorf("đã đạt độ sâu tối đa (%d): tự làm việc này thay vì tạo agent con", lim.MaxDepth)
	}
	if parent.activeChildren() >= lim.MaxChildren {
		return nil, fmt.Errorf("đã có %d agent con đang chạy (tối đa %d)", parent.activeChildren(), lim.MaxChildren)
	}
	budget := parent.budget
	if parent.Role == "chat" { // each delegated task gets its own budget
		budget = &Budget{Limit: lim.TaskTokenBudget}
	}
	if budget.Exceeded() {
		return nil, fmt.Errorf("ngân sách token của task đã hết")
	}
	a := &Agent{rt: rt, ID: rt.newID(role), Role: role, Model: model, Effort: effort, Conv: parent.Conv, Parent: parent, Depth: depth,
		Task: brief, inbox: make(chan Event, 64), children: map[string]*Agent{}, budget: budget}
	if parent.Role == "chat" {
		a.ctx, a.cancel = context.WithTimeout(parent.ctx, lim.WorkerTimeout)
	} else {
		a.ctx, a.cancel = context.WithCancel(parent.ctx)
	}
	if err := a.resetSession(nil); err != nil {
		a.cancel()
		return nil, err
	}
	rt.mu.Lock()
	rt.agents[a.ID] = a
	rt.mu.Unlock()
	parent.addChild(a)
	rt.Store.UpsertAgent(a.ID, a.Conv, parent.ID, role, model, brief, "running")
	rt.Store.AddMessage(a.Conv, parent.ID, a.ID, "task", brief)
	a.deliver(Event{Kind: "task", From: parent.ID, Text: brief})
	go a.run()
	log.Printf("[%s] spawned %s (model %s, effort %s, depth %d)", parent.ID, a.ID, model, a.effortLabel(), depth)
	return a, nil
}

func (rt *Runtime) workerRoles() []string {
	var out []string
	for name := range rt.C().Roles {
		if name != "chat" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func (rt *Runtime) lookup(id string) *Agent {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.agents[id]
}

func (rt *Runtime) forget(a *Agent) {
	rt.mu.Lock()
	delete(rt.agents, a.ID)
	rt.mu.Unlock()
}

// Tree renders the live agent tree of a conversation.
func (rt *Runtime) Tree(conv string) string {
	rt.mu.Lock()
	chat := rt.chats[conv]
	rt.mu.Unlock()
	if chat == nil {
		return "Chưa có agent nào."
	}
	var b strings.Builder
	var walk func(a *Agent, indent string)
	walk = func(a *Agent, indent string) {
		fmt.Fprintf(&b, "%s• %s [%s/%s, %s] %s", indent, a.ID, a.Model, a.effortLabel(), a.Status(), clip(oneLine(a.Task), 80))
		if a.Role != "chat" {
			fmt.Fprintf(&b, " · %dk tok", a.budget.Used()/1000)
		}
		b.WriteString("\n")
		if todos := a.Todos(); todos != "" {
			for _, l := range strings.Split(strings.TrimSpace(todos), "\n") {
				b.WriteString(indent + "   " + l + "\n")
			}
		}
		for _, c := range a.childList() {
			walk(c, indent+"   ")
		}
	}
	walk(chat, "")
	return b.String()
}

func (rt *Runtime) CancelAll(conv string) int {
	rt.mu.Lock()
	chat := rt.chats[conv]
	rt.mu.Unlock()
	if chat == nil {
		return 0
	}
	n := 0
	for _, c := range chat.childList() {
		n += c.countTree()
		c.Cancel("user stopped")
	}
	return n
}

// RequestApproval asks the user whether agent a may run a gated tool. Blocks.
func (rt *Runtime) RequestApproval(ctx context.Context, a *Agent, tool, input string) bool {
	rt.mu.Lock()
	rt.apSeq++
	id := rt.apSeq
	ap := &approval{conv: a.Conv, agent: a, ch: make(chan bool, 1)}
	rt.approvals[id] = ap
	rt.mu.Unlock()
	rt.GW.Send(a.Conv, fmt.Sprintf("🔐 %s muốn chạy `%s`:\n%s\n→ /approve %d · /approve %d all (tin tưởng agent này tới khi xong) · /deny %d",
		a.ID, tool, clip(input, 2500), id, id, id))
	select {
	case ok := <-ap.ch:
		return ok
	case <-ctx.Done():
	case <-time.After(rt.C().Limits.ApprovalTimeout):
	}
	rt.mu.Lock()
	delete(rt.approvals, id)
	rt.mu.Unlock()
	return false
}

// ConfigPath is the config file the runtime was started with (for reload/self tools).
func (rt *Runtime) ConfigPath() string { return filepath.Join(rt.C().Dir, "config.yaml") }

// Reload re-reads config.yaml, prompts and skills. Running workers keep their
// current session; chat agents pick up the change from the next message.
func (rt *Runtime) Reload() (string, error) {
	path := rt.cfgPath
	cfg, err := config.Load(path)
	if err != nil {
		return "", fmt.Errorf("config lỗi, giữ cấu hình cũ: %w", err)
	}
	router, err := llm.NewRouter(cfg)
	if err != nil {
		return "", fmt.Errorf("router lỗi, giữ cấu hình cũ: %w", err)
	}
	sk, err := skills.Load(cfg.Path(cfg.SkillsDir))
	if err != nil {
		return "", fmt.Errorf("skills lỗi, giữ cấu hình cũ: %w", err)
	}
	rt.state.Store(&state{cfg: cfg, router: router, skills: sk})
	rt.Mem.Reconfigure(router, cfg.Distiller)
	rt.mu.Lock()
	for _, c := range rt.chats {
		c.mu.Lock()
		c.stale = true
		c.mu.Unlock()
	}
	rt.mu.Unlock()
	return fmt.Sprintf("Đã nạp lại cấu hình: %d model, %d role, %d skill. Chat áp dụng từ tin nhắn tiếp theo; worker đang chạy giữ cấu hình cũ.",
		len(cfg.Models), len(cfg.Roles), len(sk.ForRole("chat"))+len(sk.ForRole("coder"))), nil
}

// MaybeCompact distills and restarts a chat session that grew too long.
func (rt *Runtime) maybeCompact(a *Agent) {
	if a.session.Len() < rt.C().Limits.ChatMaxMessages {
		return
	}
	dialog := rt.Store.DialogSince(a.Conv, a.sessionStart)
	if err := rt.Mem.Distill(a.ctx, dialog); err != nil {
		log.Printf("[%s] distill: %v", a.ID, err)
	}
	if err := a.resetSession(rt.chatSeed(a.Conv)); err != nil {
		log.Printf("[%s] reset session: %v", a.ID, err)
	}
}

// DistillAll summarizes every open chat (on shutdown).
func (rt *Runtime) DistillAll(ctx context.Context) {
	rt.mu.Lock()
	var chats []*Agent
	for _, c := range rt.chats {
		chats = append(chats, c)
	}
	rt.mu.Unlock()
	for _, c := range chats {
		if d := rt.Store.DialogSince(c.Conv, c.sessionStart); len(d) >= 4 {
			if err := rt.Mem.Distill(ctx, d); err != nil {
				log.Printf("[%s] distill: %v", c.ID, err)
			}
		}
	}
}

func (rt *Runtime) readPrompt(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(rt.C().Path(path))
	if err != nil {
		log.Printf("prompt %s: %v", path, err)
		return ""
	}
	return string(b)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

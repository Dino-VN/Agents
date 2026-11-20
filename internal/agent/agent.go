package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentd/internal/llm"
	"agentd/internal/tools"
)

// Event is anything that lands in an agent's inbox.
type Event struct {
	Kind        string // user | task | message | report
	From        string
	Text        string
	Attachments []llm.Block // images / PDFs sent by the user
	fn          func()      // Kind "control": runs on the agent goroutine, no model call
}

// Budget is a token counter shared by one delegated task tree.
type Budget struct {
	Limit int64
	used  atomic.Int64
}

func (b *Budget) Add(u llm.Usage) { b.used.Add(u.In + u.Out) }
func (b *Budget) Used() int64     { return b.used.Load() }
func (b *Budget) Exceeded() bool  { return b.Limit > 0 && b.used.Load() >= b.Limit }

type Agent struct {
	rt     *Runtime
	ID     string
	Role   string
	Model  string
	Effort string // thinking effort override ("" = model default)
	Conv   string
	Task   string
	Parent *Agent
	Depth  int

	ctx    context.Context
	cancel context.CancelFunc
	inbox  chan Event
	budget *Budget

	session      llm.Session
	tools        []tools.Tool
	sessionStart int64 // last message id when the chat session was (re)built

	mu       sync.Mutex
	children map[string]*Agent
	status   string // running | waiting | done | failed | canceled
	reported bool
	stopNote string
	lastText string
	todos    string   // rendered todo list (todo_write)
	activity []string // recent actions, for agent_inspect
	trusted  bool     // user approved all gated tools for this agent
	stale    bool     // config reloaded; rebuild the session before the next turn
	controls []func()
}

func (a *Agent) effortLabel() string { return a.labelFor(a.Effort) }

func (a *Agent) labelFor(e string) string {
	if e != "" {
		return e
	}
	if d := a.rt.C().Models[a.Model].Effort; d != "" {
		return d + " (mặc định)"
	}
	return "mặc định của model"
}

// deliverControl runs fn on the agent's goroutine between turns, so it never
// races with a turn in progress.
func (a *Agent) deliverControl(fn func()) { a.deliver(Event{Kind: "control", fn: fn}) }

func (a *Agent) setTodos(s string) {
	a.mu.Lock()
	a.todos = s
	a.mu.Unlock()
}

func (a *Agent) Todos() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.todos
}

func (a *Agent) Status() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status == "" {
		return "running"
	}
	return a.status
}

func (a *Agent) setStatus(s string) {
	a.mu.Lock()
	a.status = s
	a.mu.Unlock()
}

func (a *Agent) addChild(c *Agent) {
	a.mu.Lock()
	a.children[c.ID] = c
	a.mu.Unlock()
}

func (a *Agent) removeChild(id string) {
	a.mu.Lock()
	delete(a.children, id)
	a.mu.Unlock()
}

func (a *Agent) childList() []*Agent {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*Agent, 0, len(a.children))
	for _, c := range a.children {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (a *Agent) activeChildren() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.children)
}

func (a *Agent) countTree() int {
	n := 1
	for _, c := range a.childList() {
		n += c.countTree()
	}
	return n
}

// deliver puts an event in the inbox without ever blocking the sender forever.
func (a *Agent) deliver(ev Event) {
	select {
	case a.inbox <- ev:
	default:
		go func() {
			select {
			case a.inbox <- ev:
			case <-a.ctx.Done():
			}
		}()
	}
}

func (a *Agent) Cancel(reason string) {
	a.mu.Lock()
	a.stopNote = reason
	a.mu.Unlock()
	a.cancel()
}

func (a *Agent) resetSession(seed []llm.Message) error {
	rc := a.rt.C().Roles[a.Role]
	a.tools = a.rt.Tools.Match(rc.Tools)
	defs := make([]llm.ToolDef, len(a.tools))
	for i, t := range a.tools {
		defs[i] = t.Def
	}
	s, err := a.rt.R().NewSession(a.Model, a.Effort, a.systemPrompt(), defs, seed)
	if err != nil {
		return err
	}
	a.session = s
	a.sessionStart = a.rt.Store.LastMessageID()
	return nil
}

func (a *Agent) run() {
	defer a.finish()
	for {
		select {
		case <-a.ctx.Done():
			return
		case ev := <-a.inbox:
			if ev.Kind == "control" {
				ev.fn()
				continue
			}
			if a.Role == "chat" {
				a.refreshIfStale()
				a.rt.maybeCompact(a)
			}
			a.turn([]Event{ev})
			for _, fn := range a.controls {
				fn()
			}
			a.controls = nil
			if a.Role != "chat" && a.doneAfterTurn() {
				return
			}
		}
	}
}

// refreshIfStale applies a config reload to the chat agent: new model, prompt
// and tools take effect from the next message.
func (a *Agent) refreshIfStale() {
	a.mu.Lock()
	stale := a.stale
	a.stale = false
	a.mu.Unlock()
	if !stale {
		return
	}
	a.Model = a.rt.C().Roles["chat"].Model
	if err := a.resetSession(a.rt.chatSeed(a.Conv)); err != nil {
		log.Printf("[%s] reload session: %v", a.ID, err)
	}
}

// doneAfterTurn decides whether a worker is finished: it reported, or it has no
// children left to wait for (in which case its last text is reported for it).
func (a *Agent) doneAfterTurn() bool {
	a.mu.Lock()
	reported, kids, last := a.reported, len(a.children), a.lastText
	a.mu.Unlock()
	if reported {
		return true
	}
	if kids > 0 || len(a.inbox) > 0 {
		a.setStatus("waiting")
		return false
	}
	a.report("done", last)
	return true
}

func (a *Agent) turn(events []Event) {
	a.setStatus("running")
	blocks := a.render(events)
	rc := a.rt.C().Roles[a.Role]
	lim := a.rt.C().Limits
	calls, nudged := 0, false
	lastText := ""

	for i := 0; ; i++ {
		t, err := a.send(blocks)
		if err != nil {
			if a.ctx.Err() != nil {
				return
			}
			log.Printf("[%s] model error: %v", a.ID, err)
			if a.Role == "chat" {
				a.rt.GW.Send(a.Conv, "⚠️ Lỗi gọi model: "+err.Error())
			} else {
				a.report("failed", "Lỗi gọi model: "+err.Error())
			}
			return
		}
		a.budget.Add(t.Usage)
		if t.Text != "" {
			lastText = t.Text
			a.logActivity("nói: " + oneLine(t.Text))
			if a.Role == "chat" {
				a.rt.Store.AddMessage(a.Conv, a.ID, "user", "reply", t.Text)
				a.rt.GW.Send(a.Conv, t.Text)
			}
		}
		if t.Stop == "refusal" {
			if a.Role == "chat" {
				a.rt.GW.Send(a.Conv, "(Model từ chối yêu cầu này.)")
			} else {
				a.report("failed", "Model từ chối thực hiện task.")
			}
			return
		}
		if len(t.Calls) == 0 {
			a.mu.Lock()
			a.lastText = lastText
			a.mu.Unlock()
			if a.Role != "chat" && !a.isReported() && a.activeChildren() == 0 && len(a.inbox) == 0 {
				a.report("done", lastText)
			}
			return
		}

		blocks = a.runTools(t)
		calls += len(t.Calls)
		if a.isReported() || a.ctx.Err() != nil {
			return
		}
		var notes []string
		if a.Role == "chat" && rc.MaxToolCalls > 0 && calls > rc.MaxToolCalls && !nudged {
			nudged = true
			notes = append(notes, "[hệ thống] Bạn đã gọi nhiều tool trong lượt này. Nếu việc còn dài, hãy delegate cho worker thay vì tự làm.")
		}
		if a.Role != "chat" {
			if i == lim.MaxTurns {
				notes = append(notes, "[hệ thống] Đã hết số lượt cho phép. Gọi report ngay với kết quả hiện có.")
			} else if i > lim.MaxTurns+2 {
				a.report("failed", "Vượt quá số lượt cho phép. Kết quả dở dang: "+lastText)
				return
			}
			if a.budget.Exceeded() {
				notes = append(notes, "[hệ thống] Ngân sách token của task đã hết. Gọi report ngay.")
			}
		}
		// Messages that arrived mid-turn are shown alongside the tool results.
		for _, ev := range a.drain() {
			notes = append(notes, a.renderEvent(ev))
		}
		for _, n := range notes {
			blocks = append(blocks, llm.TextBlock(n))
		}
	}
}

// send calls the model, retrying transient failures (overload, 5xx, network)
// with backoff. Sessions roll back a failed call, so a retry resends cleanly.
func (a *Agent) send(blocks []llm.Block) (*llm.Turn, error) {
	backoff := []time.Duration{3 * time.Second, 10 * time.Second, 30 * time.Second}
	for attempt := 0; ; attempt++ {
		t, err := a.session.Send(a.ctx, blocks)
		if err == nil || attempt == len(backoff) || !llm.Retryable(err) {
			return t, err
		}
		log.Printf("[%s] model error (retry %d in %s): %v", a.ID, attempt+1, backoff[attempt], err)
		select {
		case <-time.After(backoff[attempt]):
		case <-a.ctx.Done():
			return nil, a.ctx.Err()
		}
	}
}

func (a *Agent) drain() []Event {
	var out []Event
	for {
		select {
		case ev := <-a.inbox:
			if ev.Kind == "control" {
				a.controls = append(a.controls, ev.fn) // applied once the turn ends
				continue
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

func (a *Agent) isReported() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reported
}

// runTools executes all calls of a turn concurrently and returns their results
// in one batch (keeps parallel tool use working).
func (a *Agent) runTools(t *llm.Turn) []llm.Block {
	results := make([]llm.Block, len(t.Calls))
	var wg sync.WaitGroup
	for i, c := range t.Calls {
		wg.Add(1)
		go func(i int, c llm.Block) {
			defer wg.Done()
			if t.Stop == "max_tokens" {
				results[i] = llm.ResultBlock(c.ToolID, "Output bị cắt do max_tokens; input của tool có thể không đầy đủ. Hãy thử lại với input ngắn hơn (chia nhỏ).", true)
				return
			}
			out, parts, err := a.callTool(c)
			a.logActivity(fmt.Sprintf("%s %s → %s", c.ToolName, clip(oneLine(string(c.Input)), 160), resultSummary(out, parts, err)))
			if err != nil {
				results[i] = llm.ResultBlock(c.ToolID, err.Error(), true)
			} else {
				results[i] = llm.ResultBlock(c.ToolID, out, false)
				results[i].Parts = parts
			}
		}(i, c)
	}
	wg.Wait()
	return results
}

func (a *Agent) callTool(c llm.Block) (string, []llm.Block, error) {
	var tool *tools.Tool
	for i := range a.tools {
		if a.tools[i].Def.Name == c.ToolName {
			tool = &a.tools[i]
		}
	}
	if tool == nil {
		return "", nil, fmt.Errorf("tool %q không có hoặc không được phép cho role %s", c.ToolName, a.Role)
	}
	if err := tools.Validate(tool.Def, c.Input); err != nil {
		return "", nil, err
	}
	if a.needsApproval(c) {
		if !a.rt.RequestApproval(a.ctx, a, c.ToolName, a.rt.describeCall(c)) {
			return "", nil, fmt.Errorf("user không duyệt lệnh này (hoặc hết thời gian chờ). Đừng thử lại cùng lệnh; tìm cách khác hoặc report")
		}
	}
	log.Printf("[%s] tool %s %s", a.ID, c.ToolName, clip(string(c.Input), 200))
	call := &tools.Call{AgentID: a.ID, Role: a.Role, Conv: a.Conv, Workspace: a.rt.C().Path(a.rt.C().Workspace)}
	if tool.RunRich != nil {
		return tool.RunRich(a.ctx, call, json.RawMessage(c.Input))
	}
	out, err := tool.Run(a.ctx, call, json.RawMessage(c.Input))
	return out, nil, err
}

// needsApproval: tool is in the role's ask list, unless the user trusted this
// agent or it is a shell command matching the role's shell_allow patterns.
func (a *Agent) needsApproval(c llm.Block) bool {
	rc := a.rt.C().Roles[a.Role]
	if !tools.Matches(rc.Ask, c.ToolName) {
		return false
	}
	a.mu.Lock()
	trusted := a.trusted
	a.mu.Unlock()
	if trusted {
		return false
	}
	if c.ToolName == "shell" {
		var in struct {
			Command string `json:"command"`
		}
		json.Unmarshal(c.Input, &in)
		if tools.ShellAllowed(in.Command, rc.ShellAllow) {
			return false
		}
	}
	return true
}

func (a *Agent) render(events []Event) []llm.Block {
	var blocks []llm.Block
	for _, ev := range events {
		blocks = append(blocks, ev.Attachments...)
		blocks = append(blocks, llm.TextBlock(a.renderEvent(ev)))
	}
	if a.Role == "chat" && len(events) == 1 && events[0].Kind == "user" {
		if hints := a.rt.Mem.Hints(events[0].Text, 3); len(hints) > 0 {
			var b strings.Builder
			b.WriteString("[gợi ý từ trí nhớ — chỉ dùng nếu liên quan]\n")
			for _, h := range hints {
				fmt.Fprintf(&b, "- %s (%s, %s)\n", h.Content, h.Kind, h.TS.Format("2006-01-02"))
			}
			blocks = append(blocks, llm.TextBlock(b.String()))
		}
	}
	return blocks
}

func (a *Agent) renderEvent(ev Event) string {
	switch ev.Kind {
	case "user":
		return ev.Text
	case "task":
		return fmt.Sprintf("[task từ %s]\n%s", ev.From, ev.Text)
	case "report":
		return fmt.Sprintf("[báo cáo từ %s]\n%s", ev.From, ev.Text)
	default:
		return fmt.Sprintf("[tin nhắn từ %s]\n%s", ev.From, ev.Text)
	}
}

// report sends the final result to the parent; the worker stops afterwards.
func (a *Agent) report(status, result string) {
	a.mu.Lock()
	if a.reported {
		a.mu.Unlock()
		return
	}
	a.reported = true
	a.status = status
	a.mu.Unlock()
	if strings.TrimSpace(result) == "" {
		result = "(không có nội dung)"
	}
	a.rt.Store.FinishAgent(a.ID, status, result, a.budget.Used())
	if a.Parent != nil {
		text := fmt.Sprintf("trạng thái: %s\n%s", status, result)
		a.rt.Store.AddMessage(a.Conv, a.ID, a.Parent.ID, "report", text)
		a.Parent.deliver(Event{Kind: "report", From: a.ID, Text: text})
	}
}

func (a *Agent) finish() {
	if a.Role == "chat" {
		return
	}
	if !a.isReported() {
		a.mu.Lock()
		note := a.stopNote
		a.mu.Unlock()
		if note == "" {
			note = "hết thời gian hoặc bị huỷ"
		}
		a.report("canceled", "Agent dừng giữa chừng: "+note)
	}
	for _, c := range a.childList() {
		c.Cancel("agent cha đã kết thúc")
	}
	a.cancel()
	if a.Parent != nil {
		a.Parent.removeChild(a.ID)
	}
	a.rt.forget(a)
	log.Printf("[%s] finished (%s, %d tokens)", a.ID, a.Status(), a.budget.Used())
}

// logActivity keeps the last few actions of an agent for agent_inspect.
func (a *Agent) logActivity(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.activity = append(a.activity, time.Now().Format("15:04:05")+" "+s)
	if len(a.activity) > 40 {
		a.activity = a.activity[len(a.activity)-40:]
	}
}

func (a *Agent) Activity(n int) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n <= 0 || n > len(a.activity) {
		n = len(a.activity)
	}
	return append([]string(nil), a.activity[len(a.activity)-n:]...)
}

func resultSummary(out string, parts []llm.Block, err error) string {
	if err != nil {
		return "LỖI: " + clip(oneLine(err.Error()), 160)
	}
	s := clip(oneLine(out), 160)
	if len(parts) > 0 {
		s += fmt.Sprintf(" [+%d ảnh]", len(parts))
	}
	return s
}

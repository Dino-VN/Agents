// Package memory is the recall/remember layer on top of the store, plus the
// distiller that turns finished dialog into summaries and facts.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"agentd/internal/llm"
	"agentd/internal/store"
)

type Memory struct {
	st        *store.Store
	mu        sync.Mutex
	router    *llm.Router
	distiller string // model name; empty disables distillation
}

// Reconfigure swaps the router and distiller model after a config reload.
func (m *Memory) Reconfigure(router *llm.Router, distiller string) {
	m.mu.Lock()
	m.router, m.distiller = router, distiller
	m.mu.Unlock()
}

func New(st *store.Store, router *llm.Router, distiller string) *Memory {
	return &Memory{st: st, router: router, distiller: distiller}
}

func (m *Memory) Remember(kind, subject, content string) int64 {
	return m.st.AddNote(kind, subject, content)
}

func (m *Memory) Forget(id int64) bool { return m.st.ForgetNote(id) }

// Recall searches notes first, then the raw message log.
func (m *Memory) Recall(query string, limit int) string {
	var b strings.Builder
	notes := m.st.SearchNotes(query, limit)
	if len(notes) > 0 {
		b.WriteString("## Ghi nhớ\n")
		for _, n := range notes {
			fmt.Fprintf(&b, "- [#%d %s/%s %s] %s\n", n.ID, n.Kind, n.Subject, n.TS.Format("2006-01-02"), n.Content)
		}
	}
	msgs := m.st.SearchMessages(query, limit)
	if len(msgs) > 0 {
		b.WriteString("## Hội thoại cũ\n")
		for _, x := range msgs {
			fmt.Fprintf(&b, "- [%s %s→%s] %s\n", x.TS.Format("2006-01-02 15:04"), x.Sender, x.Recipient, clip(x.Content, 400))
		}
	}
	if b.Len() == 0 {
		return "Không tìm thấy gì liên quan."
	}
	return b.String()
}

// Hints returns a few short notes relevant to text, for injecting into a turn.
func (m *Memory) Hints(text string, limit int) []store.Note {
	return m.st.SearchNotes(text, limit)
}

// Profile renders what we know about the user plus recent session summaries.
func (m *Memory) Profile() string {
	var b strings.Builder
	facts := m.st.Notes("fact", "user", 40)
	if len(facts) > 0 {
		b.WriteString("Về user:\n")
		for i := len(facts) - 1; i >= 0; i-- {
			fmt.Fprintf(&b, "- %s (#%d)\n", facts[i].Content, facts[i].ID)
		}
	}
	sums := m.st.Notes("summary", "", 3)
	if len(sums) > 0 {
		b.WriteString("\nTóm tắt các phiên gần đây:\n")
		for i := len(sums) - 1; i >= 0; i-- {
			fmt.Fprintf(&b, "- %s: %s\n", sums[i].TS.Format("2006-01-02"), sums[i].Content)
		}
	}
	return b.String()
}

const distillPrompt = `Bạn là bộ chưng cất trí nhớ. Đọc đoạn hội thoại giữa user và trợ lý rồi trả về DUY NHẤT một JSON:
{"summary": "tóm tắt 1-3 câu: đã bàn gì, quyết định gì",
 "facts": ["sự thật bền vững về user: sở thích, dự án, người quen, thói quen, cách làm việc"]}
Chỉ ghi facts mới, chắc chắn và có ích lâu dài; không ghi chi tiết vụn vặt. Các fact đã biết:
%s

Hội thoại:
%s`

// Distill summarizes dialog and stores the summary and new user facts.
func (m *Memory) Distill(ctx context.Context, dialog []store.Msg) error {
	m.mu.Lock()
	router, distiller := m.router, m.distiller
	m.mu.Unlock()
	if distiller == "" || len(dialog) == 0 {
		return nil
	}
	var known, conv strings.Builder
	for _, f := range m.st.Notes("fact", "user", 60) {
		known.WriteString("- " + f.Content + "\n")
	}
	for _, x := range dialog {
		who := "Trợ lý"
		if x.Kind == "user" {
			who = "User"
		}
		fmt.Fprintf(&conv, "%s: %s\n", who, clip(x.Content, 2000))
	}
	out, err := router.Complete(ctx, distiller, "Bạn chỉ trả về JSON hợp lệ.",
		fmt.Sprintf(distillPrompt, known.String(), conv.String()))
	if err != nil {
		return err
	}
	var res struct {
		Summary string   `json:"summary"`
		Facts   []string `json:"facts"`
	}
	start, end := strings.Index(out, "{"), strings.LastIndex(out, "}")
	if start < 0 || end <= start {
		return fmt.Errorf("distiller returned no JSON")
	}
	if err := json.Unmarshal([]byte(out[start:end+1]), &res); err != nil {
		return err
	}
	if res.Summary != "" {
		m.st.AddNote("summary", "session", res.Summary)
	}
	for _, f := range res.Facts {
		if f = strings.TrimSpace(f); f != "" {
			m.st.AddNote("fact", "user", f)
		}
	}
	return nil
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

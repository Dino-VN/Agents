package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"agentd/internal/llm"
	"agentd/internal/tools"
)

// registerAgentTools adds tools that need the runtime: coordination, memory, skills.
func (rt *Runtime) registerAgentTools() {
	self := func(c *tools.Call) (*Agent, error) {
		a := rt.lookup(c.AgentID)
		if a == nil {
			return nil, fmt.Errorf("agent %s không còn tồn tại", c.AgentID)
		}
		return a, nil
	}
	type spawnArgs struct {
		Role   string `json:"role"`
		Model  string `json:"model"`
		Effort string `json:"effort"`
		Brief  string `json:"brief"`
	}
	spawnRun := func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, error) {
		a, err := self(c)
		if err != nil {
			return "", err
		}
		x, err := tools.Args[spawnArgs](in)
		if err != nil {
			return "", err
		}
		child, err := rt.Spawn(a, x.Role, x.Model, x.Effort, x.Brief)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Đã tạo %s (role %s, model %s, effort %s). Kết quả sẽ tới dưới dạng [báo cáo từ %s]. "+
			"Kết thúc lượt nếu không còn việc gì khác để làm song song.", child.ID, child.Role, child.Model, child.effortLabel(), child.ID), nil
	}
	spawnProps := map[string]any{
		"role":  tools.Str("Role của worker (xem danh sách trong system prompt)"),
		"model": tools.Str("Tên model (tuỳ chọn; mặc định theo role). Chọn theo thẻ năng lực model."),
		"effort": map[string]any{"type": "string", "enum": llm.Efforts,
			"description": "Mức suy nghĩ (tuỳ chọn): low = việc đơn giản/nhanh, medium = bình thường, high = khó, xhigh/max = rất khó, cần đúng tuyệt đối. Bỏ trống = mặc định của model."},
		"brief": tools.Str("Brief tự đủ: mục tiêu, ngữ cảnh cần biết, ràng buộc, tiêu chí hoàn thành, định dạng kết quả mong muốn."),
	}
	rt.Tools.Add(tools.Tool{
		Def: tools.Def("delegate", "Giao một việc cho worker agent chạy nền. Worker không thấy cuộc chat, nên brief phải đầy đủ.",
			spawnProps, "role", "brief"),
		Run: spawnRun,
	})
	rt.Tools.Add(tools.Tool{
		Def: tools.Def("spawn_agent", "Tạo subagent để làm một phần việc song song hoặc chuyên biệt. Subagent báo cáo lại cho bạn.",
			spawnProps, "role", "brief"),
		Run: spawnRun,
	})

	rt.Tools.Add(tools.Tool{
		Def: tools.Def("report", "Gửi kết quả cuối cùng cho agent đã giao việc và kết thúc. Gọi đúng một lần khi xong.",
			map[string]any{
				"status": map[string]any{"type": "string", "enum": []string{"done", "partial", "failed"}},
				"result": tools.Str("Kết quả đầy đủ: đã làm gì, kết quả, file đã tạo/sửa, vấn đề còn lại."),
			}, "status", "result"),
		Run: func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, error) {
			a, err := self(c)
			if err != nil {
				return "", err
			}
			x, err := tools.Args[struct{ Status, Result string }](in)
			if err != nil {
				return "", err
			}
			if a.activeChildren() > 0 {
				for _, ch := range a.childList() {
					ch.Cancel("agent cha đã report")
				}
			}
			a.report(x.Status, x.Result)
			return "Đã gửi báo cáo.", nil
		},
	})

	rt.Tools.Add(tools.Tool{
		Def: tools.Def("send_message", "Nhắn tin cho agent cha, agent con hoặc agent anh em (cùng cha).",
			map[string]any{"to": tools.Str("ID agent nhận"), "text": tools.Str("Nội dung")}, "to", "text"),
		Run: func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, error) {
			a, err := self(c)
			if err != nil {
				return "", err
			}
			x, err := tools.Args[struct{ To, Text string }](in)
			if err != nil {
				return "", err
			}
			target := rt.lookup(x.To)
			if target == nil || target.Conv != a.Conv {
				return "", fmt.Errorf("không tìm thấy agent %s (đã kết thúc?)", x.To)
			}
			if !related(a, target) {
				return "", fmt.Errorf("chỉ được nhắn cho cha, con hoặc anh em; hãy nhắn qua agent cha")
			}
			rt.Store.AddMessage(a.Conv, a.ID, target.ID, "message", x.Text)
			target.deliver(Event{Kind: "message", From: a.ID, Text: x.Text})
			return "Đã gửi.", nil
		},
	})

	rt.Tools.Add(tools.Tool{
		Def: tools.Def("list_agents", "Xem các agent liên quan (cha, con, anh em) và trạng thái.", nil),
		Run: func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, error) {
			a, err := self(c)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			if a.Parent != nil {
				fmt.Fprintf(&b, "cha: %s (%s)\n", a.Parent.ID, a.Parent.Role)
				for _, s := range a.Parent.childList() {
					if s != a {
						fmt.Fprintf(&b, "anh em: %s (%s, %s) — %s\n", s.ID, s.Role, s.Status(), clip(oneLine(s.Task), 100))
					}
				}
			}
			for _, ch := range a.childList() {
				fmt.Fprintf(&b, "con: %s (%s, %s) — %s\n", ch.ID, ch.Role, ch.Status(), clip(oneLine(ch.Task), 100))
			}
			if b.Len() == 0 {
				return "Không có agent liên quan.", nil
			}
			return b.String(), nil
		},
	})

	rt.Tools.Add(tools.Tool{
		Def: tools.Def("agent_status", "Xem cây agent đang chạy của cuộc hội thoại này.", nil),
		Run: func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, error) {
			return rt.Tree(c.Conv), nil
		},
	})

	rt.Tools.Add(tools.Tool{
		Def: tools.Def("cancel_agent", "Huỷ một agent con (và toàn bộ cây con của nó).",
			map[string]any{"id": tools.Str("ID agent"), "reason": tools.Str("Lý do")}, "id"),
		Run: func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, error) {
			a, err := self(c)
			if err != nil {
				return "", err
			}
			x, _ := tools.Args[struct{ ID, Reason string }](in)
			t := rt.lookup(x.ID)
			if t == nil || !isAncestor(a, t) {
				return "", fmt.Errorf("không tìm thấy agent con %s", x.ID)
			}
			t.Cancel(x.Reason)
			return "Đã huỷ " + x.ID, nil
		},
	})

	rt.Tools.Add(tools.Tool{
		Def: tools.Def("recall", "Tìm trong trí nhớ dài hạn và các cuộc hội thoại cũ (tìm theo từ khoá).",
			map[string]any{"query": tools.Str("Từ khoá"), "limit": tools.Int("Số kết quả mỗi loại (mặc định 6)")}, "query"),
		Run: func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, error) {
			x, err := tools.Args[struct {
				Query string `json:"query"`
				Limit int    `json:"limit"`
			}](in)
			if err != nil {
				return "", err
			}
			if x.Limit <= 0 {
				x.Limit = 6
			}
			return rt.Mem.Recall(x.Query, x.Limit), nil
		},
	})
	rt.Tools.Add(tools.Tool{
		Def: tools.Def("remember", "Lưu một ghi nhớ dài hạn. kind=fact (sự thật về user), lesson (bài học làm việc), note (khác).",
			map[string]any{
				"kind":    map[string]any{"type": "string", "enum": []string{"fact", "lesson", "note"}},
				"subject": tools.Str("Về ai/cái gì: 'user', tên role, tên dự án..."),
				"content": tools.Str("Nội dung ngắn gọn, tự đủ nghĩa"),
			}, "kind", "content"),
		Run: func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, error) {
			x, err := tools.Args[struct{ Kind, Subject, Content string }](in)
			if err != nil {
				return "", err
			}
			if x.Subject == "" {
				x.Subject = "user"
				if x.Kind == "lesson" {
					x.Subject = c.Role
				}
			}
			id := rt.Mem.Remember(x.Kind, x.Subject, x.Content)
			return fmt.Sprintf("Đã ghi nhớ (#%d).", id), nil
		},
	})
	rt.Tools.Add(tools.Tool{
		Def: tools.Def("forget", "Xoá một ghi nhớ sai hoặc lỗi thời theo id.",
			map[string]any{"id": tools.Int("ID ghi nhớ")}, "id"),
		Run: func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, error) {
			x, _ := tools.Args[struct{ ID int64 }](in)
			if !rt.Mem.Forget(x.ID) {
				return "", fmt.Errorf("không có ghi nhớ #%d", x.ID)
			}
			return "Đã xoá.", nil
		},
	})

	rt.Tools.Add(tools.Tool{
		Def: tools.Def("use_skill", "Nạp hướng dẫn đầy đủ của một skill (xem danh sách skill trong system prompt).",
			map[string]any{"name": tools.Str("Tên skill")}, "name"),
		Run: func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, error) {
			x, _ := tools.Args[struct{ Name string }](in)
			return rt.S().Use(x.Name, c.Role)
		},
	})
	rt.Tools.Add(tools.Tool{
		Def: tools.Def("read_skill_file", "Đọc một file đi kèm skill (references, scripts...).",
			map[string]any{"name": tools.Str("Tên skill"), "path": tools.Str("Đường dẫn tương đối trong thư mục skill")}, "name", "path"),
		Run: func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, error) {
			x, _ := tools.Args[struct{ Name, Path string }](in)
			return rt.S().ReadFile(x.Name, x.Path, c.Role)
		},
	})
}

// related: parent, child or sibling.
func related(a, b *Agent) bool {
	return a.Parent == b || b.Parent == a || (a.Parent != nil && a.Parent == b.Parent)
}

func isAncestor(anc, a *Agent) bool {
	for p := a.Parent; p != nil; p = p.Parent {
		if p == anc {
			return true
		}
	}
	return false
}

// registerWorkTools adds todo tracking and self-configuration tools.
func (rt *Runtime) registerWorkTools() {
	rt.Tools.Add(tools.Tool{
		Def: tools.Def("todo_write", "Ghi/cập nhật danh sách việc của bạn cho task hiện tại (gửi lại TOÀN BỘ danh sách mỗi lần). "+
			"Dùng cho task nhiều bước: lập kế hoạch trước, đánh dấu in_progress khi bắt đầu một việc, completed ngay khi xong.",
			map[string]any{"todos": map[string]any{"type": "array", "items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": tools.Str("Việc cần làm"),
					"status":  map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
				},
				"required": []string{"content", "status"},
			}}}, "todos"),
		Run: func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, error) {
			a := rt.lookup(c.AgentID)
			if a == nil {
				return "", fmt.Errorf("agent không còn tồn tại")
			}
			x, err := tools.Args[struct {
				Todos []struct{ Content, Status string } `json:"todos"`
			}](in)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			done := 0
			for _, t := range x.Todos {
				mark := "☐"
				switch t.Status {
				case "completed":
					mark = "☑"
					done++
				case "in_progress":
					mark = "▶"
				}
				fmt.Fprintf(&b, "%s %s\n", mark, t.Content)
			}
			a.setTodos(b.String())
			return fmt.Sprintf("Đã cập nhật (%d/%d xong):\n%s", done, len(x.Todos), b.String()), nil
		},
	})

	rt.Tools.Add(tools.Tool{
		Def: tools.Def("self_read", "Đọc file cấu hình của chính bạn trong thư mục home của agentd: config.yaml, souls/SOUL.md, roles/*.md, skills/<tên>/SKILL.md... "+
			"path rỗng hoặc '.' để liệt kê.", map[string]any{"path": tools.Str("Đường dẫn tương đối trong home agentd")}),
		Run: func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, error) {
			x, _ := tools.Args[struct{ Path string }](in)
			return rt.selfRead(x.Path)
		},
	})
	rt.Tools.Add(tools.Tool{
		Def: tools.Def("self_write", "Ghi (tạo/ghi đè) một file cấu hình của chính bạn: config.yaml, souls/*, roles/*, skills/**. "+
			"config.yaml được kiểm tra hợp lệ trước khi ghi; bản cũ được sao lưu. Không ghi được secrets.env hay data/. Gọi self_reload sau khi ghi.",
			map[string]any{"path": tools.Str("Đường dẫn tương đối trong home agentd"), "content": tools.Str("Nội dung đầy đủ của file")}, "path", "content"),
		Run: func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, error) {
			x, err := tools.Args[struct{ Path, Content string }](in)
			if err != nil {
				return "", err
			}
			return rt.selfWrite(x.Path, x.Content)
		},
	})
	rt.Tools.Add(tools.Tool{
		Def: tools.Def("self_reload", "Nạp lại config.yaml, prompt và skills sau khi sửa (không cần khởi động lại).", nil),
		Run: func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, error) {
			return rt.Reload()
		},
	})
}

// registerSendFile lets the chat agent send a workspace file to the user.
func (rt *Runtime) registerSendFile() {
	rt.Tools.Add(tools.Tool{
		Def: tools.Def("send_file", "Gửi một file trong workspace cho user (ảnh hiển thị trực tiếp, file khác gửi dạng tài liệu). "+
			"Dùng cho file worker đã tạo (báo cáo, code, ảnh chụp màn hình...) hoặc file user đã gửi trước đó.",
			map[string]any{"path": tools.Str("Đường dẫn tương đối trong workspace"), "caption": tools.Str("Chú thích ngắn (tuỳ chọn)")}, "path"),
		Run: func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, error) {
			x, err := tools.Args[struct{ Path, Caption string }](in)
			if err != nil {
				return "", err
			}
			p, err := tools.InWorkspace(c.Workspace, x.Path)
			if err != nil {
				return "", err
			}
			if st, err := os.Stat(p); err != nil || st.IsDir() {
				return "", fmt.Errorf("không có file %s", x.Path)
			}
			if err := rt.GW.SendFile(c.Conv, p, x.Caption); err != nil {
				return "", err
			}
			rt.Store.AddMessage(c.Conv, c.AgentID, "user", "reply", fmt.Sprintf("[đã gửi file %s] %s", x.Path, x.Caption))
			return "Đã gửi " + x.Path, nil
		},
	})
}

// registerInspect lets an agent look at what another agent in the same
// conversation is doing: status, todo list and its latest actions.
func (rt *Runtime) registerInspect() {
	rt.Tools.Add(tools.Tool{
		Def: tools.Def("agent_inspect", "Xem chi tiết một agent đang làm gì: trạng thái, model/effort, task, todo, và các bước gần nhất (tool đã gọi + kết quả rút gọn).",
			map[string]any{"id": tools.Str("ID agent (xem agent_status)"), "n": tools.Int("Số bước gần nhất (mặc định 15)")}, "id"),
		Run: func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, error) {
			x, _ := tools.Args[struct {
				ID string `json:"id"`
				N  int    `json:"n"`
			}](in)
			t := rt.lookup(x.ID)
			if t == nil || t.Conv != c.Conv {
				return "", fmt.Errorf("không có agent %s đang chạy (đã xong thì kết quả nằm trong báo cáo của nó)", x.ID)
			}
			if x.N <= 0 {
				x.N = 15
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%s — role %s, model %s, effort %s, trạng thái %s, %dk token\nTask: %s\n",
				t.ID, t.Role, t.Model, t.effortLabel(), t.Status(), t.budget.Used()/1000, clip(t.Task, 600))
			if todos := t.Todos(); todos != "" {
				b.WriteString("\nTodo:\n" + todos)
			}
			if kids := t.childList(); len(kids) > 0 {
				b.WriteString("\nAgent con:")
				for _, k := range kids {
					fmt.Fprintf(&b, " %s(%s)", k.ID, k.Status())
				}
				b.WriteString("\n")
			}
			b.WriteString("\nCác bước gần nhất:\n")
			for _, l := range t.Activity(x.N) {
				b.WriteString("- " + l + "\n")
			}
			return b.String(), nil
		},
	})
}

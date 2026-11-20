package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// RegisterLocal adds workspace file tools, shell, calc and time.
func RegisterLocal(r *Registry) {
	r.Add(Tool{
		Def: Def("read_file", "Đọc file text trong workspace, trả về kèm số dòng (dạng 'N→nội dung'; số dòng KHÔNG thuộc nội dung file). "+
			"File dài: dùng offset/limit để đọc từng đoạn.",
			map[string]any{"path": Str("Đường dẫn tương đối trong workspace"),
				"offset": Int("Dòng bắt đầu (1 = đầu file)"), "limit": Int("Số dòng tối đa (mặc định 2000)")}, "path"),
		Run: func(ctx context.Context, c *Call, in json.RawMessage) (string, error) {
			a, err := Args[struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}](in)
			if err != nil {
				return "", err
			}
			p, err := inWorkspace(c.Workspace, a.Path)
			if err != nil {
				return "", err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return "", err
			}
			if !utf8.Valid(b) {
				return fmt.Sprintf("%s là file nhị phân (%d bytes, %s); không đọc dạng text được.", a.Path, len(b), http.DetectContentType(b)), nil
			}
			return numberLines(string(b), a.Offset, a.Limit), nil
		},
	})
	r.Add(Tool{
		Def: Def("write_file", "Ghi (tạo hoặc ghi đè) file trong workspace.",
			map[string]any{"path": Str("Đường dẫn tương đối trong workspace"), "content": Str("Nội dung file")}, "path", "content"),
		Run: func(ctx context.Context, c *Call, in json.RawMessage) (string, error) {
			a, err := Args[struct{ Path, Content string }](in)
			if err != nil {
				return "", err
			}
			p, err := inWorkspace(c.Workspace, a.Path)
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(p, []byte(a.Content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("Đã ghi %d bytes vào %s", len(a.Content), a.Path), nil
		},
	})
	r.Add(Tool{
		Def: Def("list_dir", "Liệt kê file trong một thư mục của workspace.",
			map[string]any{"path": Str("Thư mục tương đối, mặc định '.'")}),
		Run: func(ctx context.Context, c *Call, in json.RawMessage) (string, error) {
			a, _ := Args[struct{ Path string }](in)
			if a.Path == "" {
				a.Path = "."
			}
			p, err := inWorkspace(c.Workspace, a.Path)
			if err != nil {
				return "", err
			}
			entries, err := os.ReadDir(p)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() {
					name += "/"
				}
				b.WriteString(name + "\n")
			}
			if b.Len() == 0 {
				return "(trống)", nil
			}
			return b.String(), nil
		},
	})
	r.Add(Tool{
		Def: Def("shell", "Chạy lệnh shell (sh -c) trong workspace, timeout mặc định 120s. Dùng cho build, test, git, cài đặt... "+
			"Ưu tiên read_file/grep/glob/edit_file thay vì cat/grep/sed qua shell.",
			map[string]any{"command": Str("Lệnh cần chạy"), "cwd": Str("Thư mục chạy, tương đối trong workspace (mặc định gốc workspace)"),
				"timeout_sec": Int("Timeout giây (tối đa 600)")}, "command"),
		Run: func(ctx context.Context, c *Call, in json.RawMessage) (string, error) {
			a, err := Args[struct {
				Command string `json:"command"`
				Cwd     string `json:"cwd"`
				Timeout int    `json:"timeout_sec"`
			}](in)
			if err != nil {
				return "", err
			}
			if a.Timeout <= 0 || a.Timeout > 600 {
				a.Timeout = 120
			}
			ctx, cancel := context.WithTimeout(ctx, time.Duration(a.Timeout)*time.Second)
			defer cancel()
			dir, err := inWorkspace(c.Workspace, a.Cwd)
			if err != nil {
				return "", err
			}
			cmd := exec.CommandContext(ctx, "sh", "-c", a.Command)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			res := Clip(string(out), 30000)
			if err != nil {
				return fmt.Sprintf("%s\n[exit: %v]", res, err), nil
			}
			if res == "" {
				res = "(không có output)"
			}
			return res, nil
		},
	})
	r.Add(Tool{
		Def: Def("calc", "Tính biểu thức số học: + - * / % ^, ngoặc, sqrt/abs/round/floor/ceil/ln/log/sin/cos/tan, pi, e.",
			map[string]any{"expr": Str("Biểu thức, ví dụ (1.5e3 + 20) * 12% ...")}, "expr"),
		Run: func(ctx context.Context, c *Call, in json.RawMessage) (string, error) {
			a, err := Args[struct{ Expr string }](in)
			if err != nil {
				return "", err
			}
			v, err := Eval(a.Expr)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%s = %s", a.Expr, formatNum(v)), nil
		},
	})
	r.Add(Tool{
		Def: Def("get_time", "Lấy ngày giờ hiện tại (có thể theo múi giờ IANA, ví dụ Asia/Ho_Chi_Minh).",
			map[string]any{"timezone": Str("Múi giờ IANA, tuỳ chọn")}),
		Run: func(ctx context.Context, c *Call, in json.RawMessage) (string, error) {
			a, _ := Args[struct{ Timezone string }](in)
			now := time.Now()
			if a.Timezone != "" {
				loc, err := time.LoadLocation(a.Timezone)
				if err != nil {
					return "", err
				}
				now = now.In(loc)
			}
			return now.Format("Monday, 2006-01-02 15:04:05 MST"), nil
		},
	})
}

// InWorkspace is inWorkspace for other packages.
func InWorkspace(ws, rel string) (string, error) { return inWorkspace(ws, rel) }

// inWorkspace resolves rel inside ws and refuses paths that escape it.
func inWorkspace(ws, rel string) (string, error) {
	p := filepath.Join(ws, rel)
	r, err := filepath.Rel(ws, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("đường dẫn %q nằm ngoài workspace", rel)
	}
	return p, nil
}

// ShellAllowed reports whether cmd is a single simple command matching one of the
// glob patterns. Anything with chaining, redirection or substitution is refused,
// so "ls; rm -rf ~" can never ride on an "ls*" rule.
func ShellAllowed(cmd string, patterns []string) bool {
	// "a && b" is allowed when every part is.
	if parts := strings.Split(cmd, "&&"); len(parts) > 1 {
		for _, p := range parts {
			if !ShellAllowed(p, patterns) {
				return false
			}
		}
		return true
	}
	cmd = strings.TrimSpace(cmd)
	if cmd == "" || strings.ContainsAny(cmd, ";&|`$<>\n\\(){}") {
		return false
	}
	for _, p := range patterns {
		if ok, _ := path.Match(p, cmd); ok {
			return true
		}
		// "go test*" style patterns: match on prefix so arguments may contain '/'.
		if strings.HasSuffix(p, "*") && !strings.ContainsAny(p[:len(p)-1], "*?[") &&
			strings.HasPrefix(cmd, p[:len(p)-1]) {
			return true
		}
	}
	return false
}

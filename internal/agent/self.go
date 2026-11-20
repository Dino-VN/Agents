package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentd/internal/config"
	"agentd/internal/llm"
)

// selfPath resolves rel inside the agentd home and enforces what self tools may touch.
func (rt *Runtime) selfPath(rel string, write bool) (string, error) {
	home := rt.C().Dir
	p := filepath.Join(home, filepath.Clean("/"+rel))
	r, _ := filepath.Rel(home, p)
	first := strings.Split(r, string(filepath.Separator))[0]
	switch {
	case r == "secrets.env" || strings.HasSuffix(r, ".env"):
		return "", fmt.Errorf("không được đụng tới secrets; user tự sửa bằng \"agentd setup\"")
	case first == "data":
		return "", fmt.Errorf("data/ là dữ liệu runtime (DB, log), không sửa bằng tool này")
	case write && !(r == "config.yaml" || first == "souls" || first == "roles" || first == "skills"):
		return "", fmt.Errorf("chỉ được ghi config.yaml, souls/, roles/, skills/")
	}
	return p, nil
}

func (rt *Runtime) selfRead(rel string) (string, error) {
	if rel == "" {
		rel = "."
	}
	p, err := rt.selfPath(rel, false)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		b, err := os.ReadFile(p)
		return string(b), err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Home agentd: %s\n", rt.C().Dir)
	filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		r, _ := filepath.Rel(rt.C().Dir, path)
		if d.IsDir() && (r == "data" || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if !d.IsDir() && !strings.HasSuffix(r, ".env") && !strings.HasSuffix(r, ".bak") {
			b.WriteString(r + "\n")
		}
		return nil
	})
	return b.String(), nil
}

func (rt *Runtime) selfWrite(rel, content string) (string, error) {
	p, err := rt.selfPath(rel, true)
	if err != nil {
		return "", err
	}
	if filepath.Base(p) == "config.yaml" {
		tmp := filepath.Join(filepath.Dir(p), ".config.check.yaml")
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
			return "", err
		}
		_, err := config.Load(tmp)
		os.Remove(tmp)
		if err != nil {
			return "", fmt.Errorf("config mới không hợp lệ, chưa ghi: %w", err)
		}
	}
	backup := ""
	if old, err := os.ReadFile(p); err == nil {
		r, _ := filepath.Rel(rt.C().Dir, p)
		backup = filepath.Join(rt.C().Path(rt.C().DataDir), "backup", time.Now().Format("20060102-150405"), r)
		os.MkdirAll(filepath.Dir(backup), 0o755)
		os.WriteFile(backup, old, 0o644)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Đã ghi %s (%d bytes).", rel, len(content))
	if backup != "" {
		msg += " Bản cũ: " + backup
	}
	return msg + " Gọi self_reload để áp dụng.", nil
}

// describeCall renders a gated tool call for the approval prompt: the command for
// shell, a diff for self_write, raw input otherwise.
func (rt *Runtime) describeCall(c llm.Block) string {
	var in struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	json.Unmarshal(c.Input, &in)
	switch c.ToolName {
	case "shell":
		if in.Cwd != "" {
			return "(trong " + in.Cwd + ") " + in.Command
		}
		return in.Command
	case "self_write":
		old := ""
		if p, err := rt.selfPath(in.Path, true); err == nil {
			b, _ := os.ReadFile(p)
			old = string(b)
		}
		return in.Path + ":\n" + lineDiff(old, in.Content)
	}
	return string(c.Input)
}

// lineDiff shows the changed middle of two texts (common head/tail trimmed),
// which is what a typical single edit looks like.
func lineDiff(a, b string) string {
	if a == "" {
		return "(file mới)\n" + clip(b, 1500)
	}
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	i := 0
	for i < len(x) && i < len(y) && x[i] == y[i] {
		i++
	}
	j := 0
	for j < len(x)-i && j < len(y)-i && x[len(x)-1-j] == y[len(y)-1-j] {
		j++
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "@@ dòng %d @@\n", i+1)
	for _, l := range x[i : len(x)-j] {
		sb.WriteString("- " + l + "\n")
	}
	for _, l := range y[i : len(y)-j] {
		sb.WriteString("+ " + l + "\n")
	}
	if i == len(x) && i == len(y) {
		return "(không thay đổi)"
	}
	return clip(sb.String(), 2000)
}

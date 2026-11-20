package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, r *Registry, ws, name string, in any) (string, error) {
	t.Helper()
	tool, ok := r.Get(name)
	if !ok {
		t.Fatalf("no tool %s", name)
	}
	raw, _ := json.Marshal(in)
	return tool.Run(context.Background(), &Call{Workspace: ws}, raw)
}

func TestCodeTools(t *testing.T) {
	ws := t.TempDir()
	r := NewRegistry()
	RegisterLocal(r)
	RegisterCode(r)
	os.MkdirAll(filepath.Join(ws, "src", "pkg"), 0o755)
	os.MkdirAll(filepath.Join(ws, "node_modules", "x"), 0o755)
	os.WriteFile(filepath.Join(ws, "src", "main.go"), []byte("package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "src", "pkg", "util.go"), []byte("package pkg\n// TODO fix\nfunc A() {}\nfunc B() {}\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "node_modules", "x", "i.js"), []byte("TODO hidden"), 0o644)

	out, err := run(t, r, ws, "edit_file", map[string]any{"path": "src/main.go", "old_string": `println("hi")`, "new_string": `println("xin chào")`})
	if err != nil || !strings.Contains(out, "dòng 4") {
		t.Fatalf("edit: %q %v", out, err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "src", "main.go")); !strings.Contains(string(b), "xin chào") {
		t.Fatal("edit not applied")
	}
	if _, err := run(t, r, ws, "edit_file", map[string]any{"path": "src/pkg/util.go", "old_string": "func", "new_string": "fn"}); err == nil || !strings.Contains(err.Error(), "2 lần") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
	if out, err := run(t, r, ws, "edit_file", map[string]any{"path": "src/pkg/util.go", "old_string": "func", "new_string": "fn", "replace_all": true}); err != nil || !strings.Contains(out, "2 chỗ") {
		t.Fatalf("replace_all: %q %v", out, err)
	}
	if _, err := run(t, r, ws, "edit_file", map[string]any{"path": "new/file.txt", "old_string": "", "new_string": "hello"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	out, _ = run(t, r, ws, "grep", map[string]any{"pattern": "TODO"})
	if !strings.Contains(out, "src/pkg/util.go:2:") || strings.Contains(out, "node_modules") {
		t.Fatalf("grep: %q", out)
	}
	out, _ = run(t, r, ws, "glob", map[string]any{"pattern": "**/*.go"})
	if out != "src/main.go\nsrc/pkg/util.go" {
		t.Fatalf("glob: %q", out)
	}
	out, _ = run(t, r, ws, "read_file", map[string]any{"path": "src/main.go", "offset": 3, "limit": 1})
	if !strings.HasPrefix(out, "3→func main() {") {
		t.Fatalf("read_file numbering: %q", out)
	}
	if _, err := run(t, r, ws, "read_file", map[string]any{"path": "../etc/passwd"}); err == nil {
		t.Fatal("path escape not blocked")
	}
}

func TestShellAllowed(t *testing.T) {
	allow := []string{"ls*", "git status*", "go test*", "pwd"}
	for cmd, want := range map[string]bool{
		"ls -la":            true,
		"go test ./...":     true,
		"git status":        true,
		"pwd":               true,
		"ls; rm -rf ~":      false,
		"ls && curl x":      false,
		"go test $(evil)":   false,
		"git status > /x":   false,
		"rm -rf /":          false,
		"lsblk | nc host 1": false,
		"ls && pwd":         true,
		"ls && rm -rf x":    false,
		"ls & rm x":         false,
	} {
		if got := ShellAllowed(cmd, allow); got != want {
			t.Errorf("ShellAllowed(%q) = %v, want %v", cmd, got, want)
		}
	}
}

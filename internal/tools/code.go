package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Directories skipped by grep/glob.
var skipDirs = map[string]bool{".git": true, "node_modules": true, ".venv": true, "venv": true,
	"__pycache__": true, "vendor": true, "dist": true, "build": true, "target": true, ".next": true}

// RegisterCode adds the Claude Code-style editing and search tools.
func RegisterCode(r *Registry) {
	r.Add(Tool{
		Def: Def("edit_file", "Sửa file bằng cách thay đúng một đoạn text. old_string phải khớp CHÍNH XÁC (kể cả thụt lề) và là duy nhất trong file, "+
			"trừ khi replace_all=true. Đọc file bằng read_file trước khi sửa; không chép số dòng vào old_string.",
			map[string]any{
				"path":        Str("Đường dẫn tương đối trong workspace"),
				"old_string":  Str("Đoạn text cần thay (rỗng = tạo file mới với new_string)"),
				"new_string":  Str("Đoạn text mới"),
				"replace_all": map[string]any{"type": "boolean", "description": "Thay mọi chỗ khớp"},
			}, "path", "old_string", "new_string"),
		Run: func(ctx context.Context, c *Call, in json.RawMessage) (string, error) {
			a, err := Args[struct {
				Path       string `json:"path"`
				Old        string `json:"old_string"`
				New        string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			}](in)
			if err != nil {
				return "", err
			}
			p, err := inWorkspace(c.Workspace, a.Path)
			if err != nil {
				return "", err
			}
			b, err := os.ReadFile(p)
			if os.IsNotExist(err) && a.Old == "" {
				os.MkdirAll(filepath.Dir(p), 0o755)
				return fmt.Sprintf("Đã tạo %s.", a.Path), os.WriteFile(p, []byte(a.New), 0o644)
			}
			if err != nil {
				return "", err
			}
			s := string(b)
			if a.Old == "" {
				return "", fmt.Errorf("file đã tồn tại; old_string không được rỗng")
			}
			if a.Old == a.New {
				return "", fmt.Errorf("old_string và new_string giống nhau")
			}
			n := strings.Count(s, a.Old)
			switch {
			case n == 0:
				return "", fmt.Errorf("không tìm thấy old_string trong %s. Đọc lại file và chép chính xác (kể cả khoảng trắng)", a.Path)
			case n > 1 && !a.ReplaceAll:
				return "", fmt.Errorf("old_string xuất hiện %d lần; thêm ngữ cảnh xung quanh cho duy nhất, hoặc dùng replace_all", n)
			}
			line := strings.Count(s[:strings.Index(s, a.Old)], "\n") + 1
			count := 1
			if a.ReplaceAll {
				s, count = strings.ReplaceAll(s, a.Old, a.New), n
			} else {
				s = strings.Replace(s, a.Old, a.New, 1)
			}
			if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("Đã sửa %s (%d chỗ, chỗ đầu ở dòng %d).", a.Path, count, line), nil
		},
	})

	r.Add(Tool{
		Def: Def("grep", "Tìm theo regex (cú pháp Go/RE2) trong file của workspace. Trả về 'file:dòng: nội dung'.",
			map[string]any{
				"pattern":     Str("Regex cần tìm"),
				"path":        Str("Thư mục hoặc file (tương đối, mặc định toàn workspace)"),
				"glob":        Str("Lọc tên file, vd *.go hoặc *.{ts,tsx}"),
				"ignore_case": map[string]any{"type": "boolean"},
				"max_results": Int("Tối đa (mặc định 100)"),
			}, "pattern"),
		Run: func(ctx context.Context, c *Call, in json.RawMessage) (string, error) {
			a, err := Args[struct {
				Pattern    string `json:"pattern"`
				Path       string `json:"path"`
				Glob       string `json:"glob"`
				IgnoreCase bool   `json:"ignore_case"`
				Max        int    `json:"max_results"`
			}](in)
			if err != nil {
				return "", err
			}
			if a.IgnoreCase {
				a.Pattern = "(?i)" + a.Pattern
			}
			re, err := regexp.Compile(a.Pattern)
			if err != nil {
				return "", fmt.Errorf("regex lỗi: %v", err)
			}
			if a.Max <= 0 {
				a.Max = 100
			}
			root, err := inWorkspace(c.Workspace, a.Path)
			if err != nil {
				return "", err
			}
			var out []string
			walkFiles(ctx, root, func(p string) bool {
				if a.Glob != "" && !globMatch(a.Glob, filepath.Base(p)) {
					return true
				}
				f, err := os.Open(p)
				if err != nil {
					return true
				}
				defer f.Close()
				rel, _ := filepath.Rel(c.Workspace, p)
				sc := bufio.NewScanner(f)
				sc.Buffer(make([]byte, 64*1024), 1<<20)
				for n := 1; sc.Scan(); n++ {
					line := sc.Text()
					if n == 1 && !utf8.ValidString(line) {
						return true // binary
					}
					if re.MatchString(line) {
						out = append(out, fmt.Sprintf("%s:%d: %s", rel, n, Clip(strings.TrimRight(line, "\r"), 300)))
						if len(out) >= a.Max {
							return false
						}
					}
				}
				return true
			})
			if len(out) == 0 {
				return "Không có kết quả.", nil
			}
			res := strings.Join(out, "\n")
			if len(out) >= a.Max {
				res += fmt.Sprintf("\n…(dừng ở %d kết quả; thu hẹp path/glob)", a.Max)
			}
			return res, nil
		},
	})

	r.Add(Tool{
		Def: Def("glob", "Liệt kê file theo mẫu tên, vd **/*.go, src/**/*.ts, *.md (tương đối với workspace hoặc path).",
			map[string]any{"pattern": Str("Mẫu glob, hỗ trợ **"), "path": Str("Thư mục gốc (tương đối)")}, "pattern"),
		Run: func(ctx context.Context, c *Call, in json.RawMessage) (string, error) {
			a, err := Args[struct{ Pattern, Path string }](in)
			if err != nil {
				return "", err
			}
			root, err := inWorkspace(c.Workspace, a.Path)
			if err != nil {
				return "", err
			}
			var out []string
			walkFiles(ctx, root, func(p string) bool {
				rel, _ := filepath.Rel(root, p)
				if pathGlob(a.Pattern, filepath.ToSlash(rel)) {
					wrel, _ := filepath.Rel(c.Workspace, p)
					out = append(out, wrel)
				}
				return len(out) < 500
			})
			sort.Strings(out)
			if len(out) == 0 {
				return "Không có file nào khớp.", nil
			}
			return strings.Join(out, "\n"), nil
		},
	})
}

func walkFiles(ctx context.Context, root string, fn func(path string) bool) {
	st, err := os.Stat(root)
	if err != nil {
		return
	}
	if !st.IsDir() {
		fn(root)
		return
	}
	stop := false
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if stop || ctx.Err() != nil {
			return filepath.SkipAll
		}
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !fn(p) {
			stop = true
		}
		return nil
	})
}

// globMatch matches a file name, supporting {a,b} alternatives.
func globMatch(pattern, name string) bool {
	if i := strings.Index(pattern, "{"); i >= 0 {
		if j := strings.Index(pattern[i:], "}"); j > 0 {
			for _, alt := range strings.Split(pattern[i+1:i+j], ",") {
				if globMatch(pattern[:i]+alt+pattern[i+j+1:], name) {
					return true
				}
			}
			return false
		}
	}
	ok, _ := filepath.Match(pattern, name)
	return ok
}

// pathGlob matches slash paths where "**" spans any number of directories.
func pathGlob(pattern, path string) bool {
	if !strings.Contains(pattern, "/") && !strings.Contains(pattern, "**") {
		return globMatch(pattern, filepath.Base(path))
	}
	return matchParts(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

func matchParts(pat, parts []string) bool {
	if len(pat) == 0 {
		return len(parts) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(parts); i++ {
			if matchParts(pat[1:], parts[i:]) {
				return true
			}
		}
		return false
	}
	if len(parts) == 0 || !globMatch(pat[0], parts[0]) {
		return false
	}
	return matchParts(pat[1:], parts[1:])
}

func numberLines(s string, offset, limit int) string {
	lines := strings.Split(s, "\n")
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = 2000
	}
	if offset > len(lines) {
		return fmt.Sprintf("(file có %d dòng)", len(lines))
	}
	end := min(len(lines), offset-1+limit)
	var b strings.Builder
	for i := offset - 1; i < end; i++ {
		fmt.Fprintf(&b, "%d→%s\n", i+1, Clip(lines[i], 2000))
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "…(còn %d dòng; đọc tiếp với offset=%d)", len(lines)-end, end+1)
	}
	return b.String()
}

package browser

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The page records what a site could observe: automation flag, whether events
// are trusted, and whether our injected globals leak into the page world.
const page = `<!doctype html><html><head><title>Thử nghiệm</title></head><body>
<h1>Đăng nhập</h1>
<input id="q" placeholder="Tên đăng nhập">
<button id="go" onclick="window.clicked = event.isTrusted">Gửi đi</button>
<a href="/next">Trang sau</a>
<script>
window.typedTrusted = null;
document.getElementById('q').addEventListener('input', e => { if (window.typedTrusted === null) window.typedTrusted = e.isTrusted; });
</script></body></html>`

func chrome(t *testing.T) string {
	for _, p := range []string{os.Getenv("CHROME"), "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser"} {
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	t.Skip("no Chrome/Chromium available")
	return ""
}

func TestBrowserActsLikeAPerson(t *testing.T) {
	bin := chrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/next" {
			fmt.Fprint(w, "<title>Trang sau</title><p>ok</p>")
			return
		}
		fmt.Fprint(w, page)
	}))
	defer srv.Close()

	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command(bin, "--headless=new", fmt.Sprintf("--remote-debugging-port=%d", port), "--user-data-dir="+t.TempDir(),
		"--disable-blink-features=AutomationControlled", "--no-first-run", "--no-default-browser-check", "about:blank")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	b := New(fmt.Sprintf("http://127.0.0.1:%d", port))
	for i := 0; ; i++ {
		if _, err := b.Tabs(ctx); err == nil {
			break
		} else if i > 50 {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	if _, err := b.Navigate(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	snap, err := b.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snap, "Đăng nhập") || !strings.Contains(snap, `"Gửi đi"`) || !strings.Contains(snap, `"Tên đăng nhập"`) {
		t.Fatalf("snapshot missing content:\n%s", snap)
	}
	idx := func(label string) int {
		for i, l := range strings.Split(snap, "\n") {
			_ = i
			if strings.Contains(l, label) {
				var n int
				fmt.Sscanf(l, "[%d]", &n)
				return n
			}
		}
		t.Fatalf("no element %q", label)
		return -1
	}
	if _, err := b.Type(ctx, idx("Tên đăng nhập"), "Nguyễn Văn A", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Click(ctx, idx("Gửi đi"), 0, 0, false); err != nil {
		t.Fatal(err)
	}

	// Read the PAGE's own world through a plain Runtime.evaluate (no isolated world).
	var seen struct {
		Webdriver    bool
		Clicked      any
		TypedTrusted any
		Value        string
		Leak         string
	}
	err = b.withTab(ctx, func(tg Target, s *session) error {
		var res struct {
			Result struct {
				Value struct {
					Webdriver    bool   `json:"webdriver"`
					Clicked      any    `json:"clicked"`
					TypedTrusted any    `json:"typedTrusted"`
					Value        string `json:"value"`
					Leak         string `json:"leak"`
				} `json:"value"`
			} `json:"result"`
		}
		err := s.call(ctx, "Runtime.evaluate", map[string]any{"returnByValue": true, "expression": `({
			webdriver: navigator.webdriver === true, clicked: window.clicked, typedTrusted: window.typedTrusted,
			value: document.getElementById('q').value,
			leak: (() => { const f = document.createElement('iframe'); document.body.appendChild(f); const base = new Set(Object.keys(f.contentWindow)); f.remove(); return Object.keys(window).filter(k => !base.has(k) && !['clicked', 'typedTrusted'].includes(k)).join(','); })()})`}, &res)
		v := res.Result.Value
		seen.Webdriver, seen.Clicked, seen.TypedTrusted, seen.Value, seen.Leak = v.Webdriver, v.Clicked, v.TypedTrusted, v.Value, v.Leak
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen.Webdriver {
		t.Error("navigator.webdriver is true — automation is visible")
	}
	if seen.Clicked != true {
		t.Errorf("click not trusted/received: %v", seen.Clicked)
	}
	if seen.TypedTrusted != true {
		t.Errorf("typing not trusted: %v", seen.TypedTrusted)
	}
	if seen.Value != "Nguyễn Văn A" {
		t.Errorf("typed value = %q", seen.Value)
	}
	if seen.Leak != "" {
		t.Errorf("globals leaked into page: %s", seen.Leak)
	}

	snap, _ = b.Snapshot(ctx)
	if _, err := b.Click(ctx, idx("Trang sau"), 0, 0, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if s, _ := b.Snapshot(ctx); !strings.Contains(s, "# Trang sau") {
		t.Fatalf("link click did not navigate:\n%s", s)
	}
	if out, err := b.Back(ctx); err != nil || !strings.Contains(out, "Thử nghiệm") {
		t.Fatalf("back: %q %v", out, err)
	}
	if img, err := b.Screenshot(ctx); err != nil || len(img) < 1000 {
		t.Fatalf("screenshot: %d bytes, %v", len(img), err)
	}
}

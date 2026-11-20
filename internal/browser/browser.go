// Package browser drives a long-running, normal (headful) Chromium over the
// DevTools protocol. It is built to look like a person using their own browser:
// the profile (cookies, logins) persists, there is no automation flag, the
// Runtime domain is never enabled, page scripts run in an isolated world the
// site cannot see, and input goes through trusted mouse/keyboard events with
// human-like movement and timing.
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type Browser struct {
	Endpoint string // http://127.0.0.1:9222

	mu     sync.Mutex
	active string          // target id of the tab agents act on
	els    map[string][]El // last snapshot's elements per tab
	mouse  [2]float64
}

func New(endpoint string) *Browser {
	return &Browser{Endpoint: strings.TrimRight(endpoint, "/"), els: map[string][]El{}}
}

type Target struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
	URL   string `json:"url"`
	WS    string `json:"webSocketDebuggerUrl"`
}

func (b *Browser) http(ctx context.Context, method, path string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, method, b.Endpoint+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("không kết nối được trình duyệt ở %s (service agent-browser đang chạy chưa?): %w", b.Endpoint, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("CDP %s: HTTP %d %s", path, resp.StatusCode, body)
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// Tabs lists normal web page tabs.
func (b *Browser) Tabs(ctx context.Context) ([]Target, error) {
	var all []Target
	if err := b.http(ctx, "GET", "/json/list", &all); err != nil {
		return nil, err
	}
	var pages []Target
	for _, t := range all {
		if t.Type == "page" && !strings.HasPrefix(t.URL, "devtools://") && !strings.HasPrefix(t.URL, "chrome-extension://") {
			pages = append(pages, t)
		}
	}
	return pages, nil
}

func (b *Browser) current(ctx context.Context) (Target, error) {
	tabs, err := b.Tabs(ctx)
	if err != nil {
		return Target{}, err
	}
	if len(tabs) == 0 {
		var t Target
		if err := b.http(ctx, "PUT", "/json/new?about:blank", &t); err != nil {
			return Target{}, err
		}
		tabs = []Target{t}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range tabs {
		if t.ID == b.active {
			return t, nil
		}
	}
	b.active = tabs[0].ID
	return tabs[0], nil
}

func (b *Browser) NewTab(ctx context.Context, rawURL string) (Target, error) {
	var t Target
	if err := b.http(ctx, "PUT", "/json/new?"+url.QueryEscape(rawURL), &t); err != nil {
		return t, err
	}
	b.mu.Lock()
	b.active = t.ID
	b.mu.Unlock()
	return t, nil
}

func (b *Browser) SwitchTab(ctx context.Context, id string) error {
	if err := b.http(ctx, "GET", "/json/activate/"+id, nil); err != nil {
		return err
	}
	b.mu.Lock()
	b.active = id
	b.mu.Unlock()
	return nil
}

func (b *Browser) CloseTab(ctx context.Context, id string) error {
	return b.http(ctx, "GET", "/json/close/"+id, nil)
}

// ---------- CDP session ----------

type session struct {
	ws      *websocket.Conn
	mu      sync.Mutex
	id      int64
	pending map[int64]chan cdpReply
	done    chan struct{}
}

type cdpReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (b *Browser) open(ctx context.Context, t Target) (*session, error) {
	ws, _, err := websocket.Dial(ctx, t.WS, nil)
	if err != nil {
		return nil, fmt.Errorf("kết nối tab: %w", err)
	}
	ws.SetReadLimit(64 << 20)
	s := &session{ws: ws, pending: map[int64]chan cdpReply{}, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			_, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var msg struct {
				ID int64 `json:"id"`
				cdpReply
			}
			if json.Unmarshal(data, &msg) != nil || msg.ID == 0 {
				continue // events
			}
			s.mu.Lock()
			ch := s.pending[msg.ID]
			delete(s.pending, msg.ID)
			s.mu.Unlock()
			if ch != nil {
				ch <- msg.cdpReply
			}
		}
	}()
	return s, nil
}

func (s *session) close() { s.ws.Close(websocket.StatusNormalClosure, "") }

func (s *session) call(ctx context.Context, method string, params any, out any) error {
	s.mu.Lock()
	s.id++
	id := s.id
	ch := make(chan cdpReply, 1)
	s.pending[id] = ch
	s.mu.Unlock()
	raw, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err := s.ws.Write(ctx, websocket.MessageText, raw); err != nil {
		return err
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return fmt.Errorf("%s: %s", method, r.Error.Message)
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-s.done:
		return fmt.Errorf("tab đã đóng")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// eval runs JS in an isolated world (invisible to the page's own scripts).
func (s *session) eval(ctx context.Context, expr string, out any) error {
	var tree struct {
		FrameTree struct {
			Frame struct {
				ID string `json:"id"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := s.call(ctx, "Page.getFrameTree", nil, &tree); err != nil {
		return err
	}
	var world struct {
		ExecutionContextID int `json:"executionContextId"`
	}
	if err := s.call(ctx, "Page.createIsolatedWorld", map[string]any{
		"frameId": tree.FrameTree.Frame.ID, "worldName": "w" + fmt.Sprint(rand.IntN(1e6)),
	}, &world); err != nil {
		return err
	}
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := s.call(ctx, "Runtime.evaluate", map[string]any{
		"expression": expr, "contextId": world.ExecutionContextID, "returnByValue": true, "awaitPromise": true,
	}, &res); err != nil {
		return err
	}
	if res.Exception != nil {
		return fmt.Errorf("JS lỗi: %s %s", res.Exception.Text, res.Exception.Exception.Description)
	}
	if out != nil && len(res.Result.Value) > 0 {
		return json.Unmarshal(res.Result.Value, out)
	}
	return nil
}

func (b *Browser) withTab(ctx context.Context, fn func(t Target, s *session) error) error {
	t, err := b.current(ctx)
	if err != nil {
		return err
	}
	s, err := b.open(ctx, t)
	if err != nil {
		return err
	}
	defer s.close()
	return fn(t, s)
}

// ---------- actions ----------

func (b *Browser) Navigate(ctx context.Context, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("chỉ mở được URL http(s)")
	}
	var out string
	err = b.withTab(ctx, func(t Target, s *session) error {
		var nav struct {
			ErrorText string `json:"errorText"`
		}
		if err := s.call(ctx, "Page.navigate", map[string]any{"url": rawURL, "transitionType": "typed"}, &nav); err != nil {
			return err
		}
		if nav.ErrorText != "" {
			return fmt.Errorf("không mở được trang: %s", nav.ErrorText)
		}
		waitLoad(ctx, s)
		var info struct{ Title, URL string }
		s.eval(ctx, `({title: document.title, url: location.href})`, &info)
		out = fmt.Sprintf("Đã mở: %s\n%s", info.Title, info.URL)
		return nil
	})
	return out, err
}

func waitLoad(ctx context.Context, s *session) {
	deadline := time.Now().Add(20 * time.Second)
	time.Sleep(400 * time.Millisecond)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		var state string
		if s.eval(ctx, `document.readyState`, &state) == nil && state == "complete" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	time.Sleep(time.Duration(300+rand.IntN(500)) * time.Millisecond) // let late scripts settle
}

type El struct {
	Tag   string `json:"t"`
	Label string `json:"l"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	Href  string `json:"h"`
}

const snapshotJS = `(() => {
  const sel = 'a[href],button,input:not([type=hidden]),select,textarea,summary,[role=button],[role=link],[role=tab],[role=menuitem],[role=checkbox],[role=radio],[role=option],[role=switch],[role=combobox],[role=textbox],[contenteditable=""],[contenteditable=true],[onclick],[tabindex]:not([tabindex="-1"])';
  const vw = innerWidth, vh = innerHeight, els = [], seen = new Set();
  for (const e of document.querySelectorAll(sel)) {
    const r = e.getBoundingClientRect();
    if (r.width < 4 || r.height < 4 || r.bottom < 0 || r.top > vh || r.right < 0 || r.left > vw) continue;
    const st = getComputedStyle(e);
    if (st.visibility === 'hidden' || st.display === 'none' || +st.opacity === 0) continue;
    const x = Math.round(r.left + r.width / 2), y = Math.round(r.top + r.height / 2);
    const top = document.elementFromPoint(x, y);
    if (top && top !== e && !e.contains(top) && !top.contains(e)) continue; // covered
    const key = x + ',' + y; if (seen.has(key)) continue; seen.add(key);
    const tag = e.tagName.toLowerCase();
    let label = e.getAttribute('aria-label') || (tag === 'input' || tag === 'textarea' ? (e.value ? 'giá trị: ' + e.value : '') || e.placeholder : '') || e.innerText || e.title || e.alt || e.name || '';
    label = String(label).trim().replace(/\s+/g, ' ').slice(0, 80);
    const type = tag === 'input' ? ':' + (e.type || 'text') : '';
    els.push({t: tag + type, l: label, x, y, h: e.href ? String(e.href).slice(0, 100) : ''});
    if (els.length >= 120) break;
  }
  const text = (document.body ? document.body.innerText : '').replace(/[ \t]+/g, ' ').replace(/\n{3,}/g, '\n\n');
  return {url: location.href, title: document.title, y: Math.round(scrollY), h: document.documentElement.scrollHeight, vh, els, text: text.slice(0, 15000)};
})()`

// Snapshot returns the visible text and a numbered list of clickable elements.
func (b *Browser) Snapshot(ctx context.Context) (string, error) {
	var out string
	err := b.withTab(ctx, func(t Target, s *session) error {
		var snap struct {
			URL, Title string
			Y, H, VH   int
			Els        []El
			Text       string
		}
		if err := s.eval(ctx, snapshotJS, &snap); err != nil {
			return err
		}
		b.mu.Lock()
		b.els[t.ID] = snap.Els
		b.mu.Unlock()
		var sb strings.Builder
		fmt.Fprintf(&sb, "# %s\n%s\n(cuộn: %d/%d px, màn hình cao %d)\n\n## Phần tử tương tác (dùng số [n] với browser_click/browser_type)\n",
			snap.Title, snap.URL, snap.Y, snap.H, snap.VH)
		for i, e := range snap.Els {
			fmt.Fprintf(&sb, "[%d] %s %q", i, e.Tag, e.Label)
			if e.Href != "" {
				fmt.Fprintf(&sb, " → %s", e.Href)
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n## Nội dung trang\n" + snap.Text)
		out = sb.String()
		return nil
	})
	return out, err
}

func (b *Browser) element(t Target, idx int) (El, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	els := b.els[t.ID]
	if idx < 0 || idx >= len(els) {
		return El{}, fmt.Errorf("không có phần tử [%d]; gọi browser_snapshot để lấy danh sách mới", idx)
	}
	return els[idx], nil
}

// moveTo glides the mouse along a slightly curved path, like a hand would.
func (b *Browser) moveTo(ctx context.Context, s *session, x, y float64) error {
	b.mu.Lock()
	from := b.mouse
	b.mu.Unlock()
	dist := math.Hypot(x-from[0], y-from[1])
	steps := 6 + int(dist/60)
	if steps > 25 {
		steps = 25
	}
	cx := (from[0]+x)/2 + (rand.Float64()-0.5)*dist*0.2
	cy := (from[1]+y)/2 + (rand.Float64()-0.5)*dist*0.2
	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		px := (1-t)*(1-t)*from[0] + 2*(1-t)*t*cx + t*t*x
		py := (1-t)*(1-t)*from[1] + 2*(1-t)*t*cy + t*t*y
		if err := s.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": px, "y": py}, nil); err != nil {
			return err
		}
		time.Sleep(time.Duration(8+rand.IntN(18)) * time.Millisecond)
	}
	b.mu.Lock()
	b.mouse = [2]float64{x, y}
	b.mu.Unlock()
	return nil
}

func (b *Browser) clickAt(ctx context.Context, s *session, x, y float64, clicks int) error {
	x += (rand.Float64() - 0.5) * 4
	y += (rand.Float64() - 0.5) * 4
	if err := b.moveTo(ctx, s, x, y); err != nil {
		return err
	}
	time.Sleep(time.Duration(60+rand.IntN(120)) * time.Millisecond)
	for i := 1; i <= clicks; i++ {
		base := map[string]any{"x": x, "y": y, "button": "left", "clickCount": i}
		press, release := clone(base), clone(base)
		press["type"], release["type"] = "mousePressed", "mouseReleased"
		if err := s.call(ctx, "Input.dispatchMouseEvent", press, nil); err != nil {
			return err
		}
		time.Sleep(time.Duration(50+rand.IntN(70)) * time.Millisecond)
		if err := s.call(ctx, "Input.dispatchMouseEvent", release, nil); err != nil {
			return err
		}
		time.Sleep(time.Duration(70+rand.IntN(60)) * time.Millisecond)
	}
	return nil
}

func clone(m map[string]any) map[string]any {
	c := map[string]any{}
	for k, v := range m {
		c[k] = v
	}
	return c
}

// Click clicks element idx from the last snapshot, or coordinates when idx < 0.
func (b *Browser) Click(ctx context.Context, idx int, x, y float64, double bool) (string, error) {
	var out string
	err := b.withTab(ctx, func(t Target, s *session) error {
		label := fmt.Sprintf("(%.0f, %.0f)", x, y)
		if idx >= 0 {
			e, err := b.element(t, idx)
			if err != nil {
				return err
			}
			x, y, label = float64(e.X), float64(e.Y), fmt.Sprintf("[%d] %s %q", idx, e.Tag, e.Label)
		}
		clicks := 1
		if double {
			clicks = 2
		}
		if err := b.clickAt(ctx, s, x, y, clicks); err != nil {
			return err
		}
		time.Sleep(700 * time.Millisecond)
		var info struct{ URL, Title string }
		s.eval(ctx, `({url: location.href, title: document.title})`, &info)
		out = fmt.Sprintf("Đã click %s. Trang hiện tại: %s (%s). Gọi browser_snapshot để xem thay đổi.", label, info.Title, info.URL)
		return nil
	})
	return out, err
}

var keyCodes = map[string]struct {
	code string
	vk   int
	text string
}{
	"Enter": {"Enter", 13, "\r"}, "Tab": {"Tab", 9, ""}, "Escape": {"Escape", 27, ""}, "Backspace": {"Backspace", 8, ""},
	"Delete": {"Delete", 46, ""}, "ArrowUp": {"ArrowUp", 38, ""}, "ArrowDown": {"ArrowDown", 40, ""},
	"ArrowLeft": {"ArrowLeft", 37, ""}, "ArrowRight": {"ArrowRight", 39, ""}, "Home": {"Home", 36, ""},
	"End": {"End", 35, ""}, "PageUp": {"PageUp", 33, ""}, "PageDown": {"PageDown", 34, ""}, "Space": {"Space", 32, " "},
}

func pressKey(ctx context.Context, s *session, key string, modifiers int) error {
	k, ok := keyCodes[key]
	if !ok {
		if len([]rune(key)) == 1 { // single character, e.g. "a" with ctrl
			r := []rune(strings.ToUpper(key))[0]
			k = struct {
				code string
				vk   int
				text string
			}{"Key" + string(r), int(r), ""}
			if modifiers == 0 {
				k.text = key
			}
		} else {
			return fmt.Errorf("phím không hỗ trợ: %s", key)
		}
	}
	down := map[string]any{"type": "keyDown", "key": key, "code": k.code, "windowsVirtualKeyCode": k.vk, "modifiers": modifiers}
	if k.text != "" {
		down["text"] = k.text
	}
	if err := s.call(ctx, "Input.dispatchKeyEvent", down, nil); err != nil {
		return err
	}
	time.Sleep(time.Duration(30+rand.IntN(60)) * time.Millisecond)
	return s.call(ctx, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": key, "code": k.code,
		"windowsVirtualKeyCode": k.vk, "modifiers": modifiers}, nil)
}

// Type focuses element idx (when >= 0) and types text with human-like rhythm.
func (b *Browser) Type(ctx context.Context, idx int, text string, clear, submit bool) (string, error) {
	var out string
	err := b.withTab(ctx, func(t Target, s *session) error {
		if idx >= 0 {
			e, err := b.element(t, idx)
			if err != nil {
				return err
			}
			if err := b.clickAt(ctx, s, float64(e.X), float64(e.Y), 1); err != nil {
				return err
			}
		}
		if clear {
			pressKey(ctx, s, "a", ctrlOrMeta())
			pressKey(ctx, s, "Backspace", 0)
		}
		for _, r := range text {
			ch := string(r)
			var err error
			if r == '\n' {
				err = pressKey(ctx, s, "Enter", 0)
			} else {
				// insertText produces trusted input events and handles any script (Vietnamese, emoji).
				err = s.call(ctx, "Input.insertText", map[string]any{"text": ch}, nil)
			}
			if err != nil {
				return err
			}
			delay := 35 + rand.IntN(90)
			if r == ' ' || rand.IntN(25) == 0 {
				delay += 80 + rand.IntN(200) // occasional pause, like between words
			}
			time.Sleep(time.Duration(delay) * time.Millisecond)
		}
		if submit {
			time.Sleep(time.Duration(200+rand.IntN(300)) * time.Millisecond)
			if err := pressKey(ctx, s, "Enter", 0); err != nil {
				return err
			}
			waitLoad(ctx, s)
		}
		out = fmt.Sprintf("Đã gõ %d ký tự%s.", len([]rune(text)), map[bool]string{true: " và nhấn Enter", false: ""}[submit])
		return nil
	})
	return out, err
}

func ctrlOrMeta() int { return 2 } // Ctrl on Linux

// Key presses a named key, optionally with modifiers ("ctrl", "shift", "alt").
func (b *Browser) Key(ctx context.Context, key string, mods []string) (string, error) {
	m := 0
	for _, x := range mods {
		switch strings.ToLower(x) {
		case "alt":
			m |= 1
		case "ctrl", "control":
			m |= 2
		case "meta", "cmd":
			m |= 4
		case "shift":
			m |= 8
		}
	}
	err := b.withTab(ctx, func(t Target, s *session) error { return pressKey(ctx, s, key, m) })
	if err != nil {
		return "", err
	}
	return "Đã nhấn " + key, nil
}

// Scroll wheels the page by dy pixels (positive = down) in a few human-sized ticks.
func (b *Browser) Scroll(ctx context.Context, dy int) (string, error) {
	var out string
	err := b.withTab(ctx, func(t Target, s *session) error {
		var vp struct{ W, H float64 }
		s.eval(ctx, `({w: innerWidth, h: innerHeight})`, &vp)
		x, y := vp.W/2+(rand.Float64()-0.5)*100, vp.H/2+(rand.Float64()-0.5)*100
		if err := b.moveTo(ctx, s, x, y); err != nil {
			return err
		}
		left := dy
		for left != 0 {
			step := 100 + rand.IntN(60)
			if abs(left) < step {
				step = abs(left)
			}
			if left < 0 {
				step = -step
			}
			if err := s.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": x, "y": y, "deltaX": 0, "deltaY": step}, nil); err != nil {
				return err
			}
			left -= step
			time.Sleep(time.Duration(40+rand.IntN(80)) * time.Millisecond)
		}
		time.Sleep(400 * time.Millisecond)
		var pos struct{ Y, H int }
		s.eval(ctx, `({y: Math.round(scrollY), h: document.documentElement.scrollHeight})`, &pos)
		out = fmt.Sprintf("Đã cuộn. Vị trí %d/%d px.", pos.Y, pos.H)
		return nil
	})
	return out, err
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// Screenshot captures the visible viewport as JPEG (base64).
func (b *Browser) Screenshot(ctx context.Context) (string, error) {
	var data string
	err := b.withTab(ctx, func(t Target, s *session) error {
		var res struct {
			Data string `json:"data"`
		}
		if err := s.call(ctx, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 65}, &res); err != nil {
			return err
		}
		data = res.Data
		return nil
	})
	return data, err
}

// Eval runs JS in the isolated world and returns the JSON result.
func (b *Browser) Eval(ctx context.Context, js string) (string, error) {
	var out json.RawMessage
	err := b.withTab(ctx, func(t Target, s *session) error { return s.eval(ctx, js, &out) })
	return string(out), err
}

func (b *Browser) Back(ctx context.Context) (string, error) {
	var out string
	err := b.withTab(ctx, func(t Target, s *session) error {
		var hist struct {
			Current int `json:"currentIndex"`
			Entries []struct {
				ID int `json:"id"`
			} `json:"entries"`
		}
		if err := s.call(ctx, "Page.getNavigationHistory", nil, &hist); err != nil {
			return err
		}
		if hist.Current == 0 {
			return fmt.Errorf("không còn trang trước")
		}
		if err := s.call(ctx, "Page.navigateToHistoryEntry", map[string]any{"entryId": hist.Entries[hist.Current-1].ID}, nil); err != nil {
			return err
		}
		waitLoad(ctx, s)
		var info struct{ URL, Title string }
		s.eval(ctx, `({url: location.href, title: document.title})`, &info)
		out = fmt.Sprintf("Đã quay lại: %s (%s)", info.Title, info.URL)
		return nil
	})
	return out, err
}

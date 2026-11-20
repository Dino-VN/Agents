package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentd/internal/browser"
	"agentd/internal/llm"
	"agentd/internal/tools"
)

// registerBrowserTools exposes the user's real browser (persistent profile,
// cookies, logins) as browser_* tools. Disabled when browser.cdp is unset.
func (rt *Runtime) registerBrowserTools() {
	if rt.C().Browser.CDP == "" {
		return
	}
	b := browser.New(rt.C().Browser.CDP)
	rt.browser = b
	add := func(name, desc string, props map[string]any, req []string, run tools.RichFunc) {
		rt.Tools.Add(tools.Tool{Def: tools.Def(name, desc, props, req...), RunRich: run})
	}
	text := func(s string, err error) (string, []llm.Block, error) { return s, nil, err }

	add("browser_navigate", "Mở URL trong trình duyệt thật của user (giữ cookie/đăng nhập). Sau đó gọi browser_snapshot để đọc trang.",
		map[string]any{"url": tools.Str("URL http(s)")}, []string{"url"},
		func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, []llm.Block, error) {
			a, _ := tools.Args[struct{ URL string }](in)
			return text(b.Navigate(ctx, a.URL))
		})
	add("browser_snapshot", "Đọc trang hiện tại: tiêu đề, URL, danh sách phần tử tương tác có số [n] (đang thấy trên màn hình) và nội dung chữ. "+
		"Gọi lại sau mỗi thao tác làm trang thay đổi hoặc sau khi cuộn.", nil, nil,
		func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, []llm.Block, error) {
			return text(b.Snapshot(ctx))
		})
	add("browser_click", "Click phần tử [n] từ browser_snapshot gần nhất (hoặc toạ độ x,y trong khung nhìn). Chuột di chuyển và click như người thật.",
		map[string]any{"index": tools.Int("Số [n] của phần tử"), "x": tools.Int("Toạ độ x (khi không dùng index)"),
			"y": tools.Int("Toạ độ y"), "double": map[string]any{"type": "boolean", "description": "Double-click"}}, nil,
		func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, []llm.Block, error) {
			a, _ := tools.Args[struct {
				Index  *int    `json:"index"`
				X      float64 `json:"x"`
				Y      float64 `json:"y"`
				Double bool    `json:"double"`
			}](in)
			idx := -1
			if a.Index != nil {
				idx = *a.Index
			} else if a.X == 0 && a.Y == 0 {
				return "", nil, fmt.Errorf("cần index hoặc x,y")
			}
			return text(b.Click(ctx, idx, a.X, a.Y, a.Double))
		})
	add("browser_type", "Gõ chữ như người (từng ký tự, có nhịp). Nếu có index thì click vào ô đó trước.",
		map[string]any{"text": tools.Str("Nội dung cần gõ"), "index": tools.Int("Ô nhập [n] (tuỳ chọn)"),
			"clear":  map[string]any{"type": "boolean", "description": "Xoá nội dung cũ trong ô trước khi gõ"},
			"submit": map[string]any{"type": "boolean", "description": "Nhấn Enter sau khi gõ"}}, []string{"text"},
		func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, []llm.Block, error) {
			a, _ := tools.Args[struct {
				Text          string `json:"text"`
				Index         *int   `json:"index"`
				Clear, Submit bool
			}](in)
			idx := -1
			if a.Index != nil {
				idx = *a.Index
			}
			return text(b.Type(ctx, idx, a.Text, a.Clear, a.Submit))
		})
	add("browser_key", "Nhấn một phím: Enter, Tab, Escape, Backspace, Delete, ArrowUp/Down/Left/Right, Home, End, PageUp, PageDown, Space, hoặc một ký tự kèm modifiers (vd key 'a' + ['ctrl']).",
		map[string]any{"key": tools.Str("Tên phím"), "modifiers": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"ctrl", "shift", "alt", "meta"}}}},
		[]string{"key"},
		func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, []llm.Block, error) {
			a, _ := tools.Args[struct {
				Key       string   `json:"key"`
				Modifiers []string `json:"modifiers"`
			}](in)
			return text(b.Key(ctx, a.Key, a.Modifiers))
		})
	add("browser_scroll", "Cuộn trang bằng con lăn chuột. dy dương = xuống, âm = lên (mặc định 600).",
		map[string]any{"dy": tools.Int("Số pixel")}, nil,
		func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, []llm.Block, error) {
			a, _ := tools.Args[struct{ Dy int }](in)
			if a.Dy == 0 {
				a.Dy = 600
			}
			return text(b.Scroll(ctx, a.Dy))
		})
	add("browser_screenshot", "Chụp ảnh khung nhìn hiện tại để xem bố cục, ảnh, captcha... (tốn token hơn snapshot; dùng khi cần nhìn). "+
		"save = đường dẫn .jpg trong workspace để lưu ảnh lại (vd để gửi cho user).",
		map[string]any{"save": tools.Str("Lưu ảnh vào file này (tuỳ chọn), vd screenshots/trang.jpg")}, nil,
		func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, []llm.Block, error) {
			a, _ := tools.Args[struct{ Save string }](in)
			data, err := b.Screenshot(ctx)
			if err != nil {
				return "", nil, err
			}
			msg := "Ảnh chụp khung nhìn hiện tại."
			if a.Save != "" {
				p, err := tools.InWorkspace(c.Workspace, a.Save)
				if err != nil {
					return "", nil, err
				}
				raw, _ := base64.StdEncoding.DecodeString(data)
				os.MkdirAll(filepath.Dir(p), 0o755)
				if err := os.WriteFile(p, raw, 0o644); err != nil {
					return "", nil, err
				}
				msg += " Đã lưu vào " + a.Save
			}
			return msg, []llm.Block{{Type: llm.Image, MediaType: "image/jpeg", Data: data}}, nil
		})
	add("browser_view", "Xem trình duyệt đang hiện gì (chụp màn hình tab đang dùng), ví dụ để theo dõi operator. Ảnh được lưu trong workspace để send_file cho user nếu cần.",
		nil, nil,
		func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, []llm.Block, error) {
			rel, data, err := rt.saveBrowserShot(ctx, c.Workspace)
			if err != nil {
				return "", nil, err
			}
			return "Đã chụp, lưu tại " + rel, []llm.Block{{Type: llm.Image, MediaType: "image/jpeg", Data: data}}, nil
		})
	add("browser_back", "Quay lại trang trước.", nil, nil,
		func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, []llm.Block, error) {
			return text(b.Back(ctx))
		})
	add("browser_tabs", "Quản lý tab: action=list | new (mở url ở tab mới) | switch (chuyển sang tab id) | close (đóng tab id).",
		map[string]any{"action": map[string]any{"type": "string", "enum": []string{"list", "new", "switch", "close"}},
			"url": tools.Str("URL cho action=new"), "id": tools.Str("ID tab cho switch/close")}, []string{"action"},
		func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, []llm.Block, error) {
			a, _ := tools.Args[struct{ Action, URL, ID string }](in)
			switch a.Action {
			case "new":
				t, err := b.NewTab(ctx, a.URL)
				return text(fmt.Sprintf("Đã mở tab %s: %s", t.ID, a.URL), err)
			case "switch":
				return text("Đã chuyển tab.", b.SwitchTab(ctx, a.ID))
			case "close":
				return text("Đã đóng tab.", b.CloseTab(ctx, a.ID))
			}
			tabs, err := b.Tabs(ctx)
			if err != nil {
				return "", nil, err
			}
			var sb strings.Builder
			for _, t := range tabs {
				fmt.Fprintf(&sb, "%s  %s — %s\n", t.ID, clip(t.Title, 60), t.URL)
			}
			return sb.String(), nil, nil
		})
	add("browser_eval", "Chạy JavaScript đọc dữ liệu trang (trong vùng cô lập, trang không thấy). Trả về JSON. Dùng để trích xuất dữ liệu có cấu trúc.",
		map[string]any{"js": tools.Str("Biểu thức JS, vd [...document.querySelectorAll('h2')].map(e=>e.innerText)")}, []string{"js"},
		func(ctx context.Context, c *tools.Call, in json.RawMessage) (string, []llm.Block, error) {
			a, _ := tools.Args[struct{ JS string }](in)
			out, err := b.Eval(ctx, a.JS)
			return tools.Clip(out, 30000), nil, err
		})
}

// saveBrowserShot screenshots the active tab into <workspace>/screenshots/.
func (rt *Runtime) saveBrowserShot(ctx context.Context, workspace string) (rel, data string, err error) {
	if rt.browser == nil {
		return "", "", fmt.Errorf("trình duyệt chưa được cấu hình (browser.cdp)")
	}
	data, err = rt.browser.Screenshot(ctx)
	if err != nil {
		return "", "", err
	}
	raw, _ := base64.StdEncoding.DecodeString(data)
	rel = filepath.Join("screenshots", "view-"+time.Now().Format("20060102-150405")+".jpg")
	p := filepath.Join(workspace, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	return rel, data, os.WriteFile(p, raw, 0o644)
}

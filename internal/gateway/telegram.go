package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agentd/internal/llm"
)

// Telegram is a long-polling Bot API gateway. Conversation id = "tg:<chat id>".
// Files users send are saved under <workspace>/inbox/<date>/ so agents can use them.
type Telegram struct {
	token     string
	base      string // Bot API origin; overridden in tests
	allowed   map[int64]bool
	http      *http.Client
	workspace string

	mu     sync.Mutex
	groups map[string]*album // media_group_id → pending album
}

type album struct {
	conv   string
	texts  []string
	blocks []llm.Block
	timer  *time.Timer
}

func NewTelegram(token string, allowed []int64, workspace string) *Telegram {
	t := &Telegram{token: token, base: "https://api.telegram.org", allowed: map[int64]bool{}, workspace: workspace,
		http: &http.Client{Timeout: 70 * time.Second}, groups: map[string]*album{}}
	for _, id := range allowed {
		t.allowed[id] = true
	}
	return t
}

func (t *Telegram) api(ctx context.Context, method string, body any, out any) error {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", t.base+"/bot"+t.token+"/"+method, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return err
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func (t *Telegram) Send(conv, text string) {
	chatID, ok := strings.CutPrefix(conv, "tg:")
	if !ok {
		return
	}
	// Telegram caps messages at 4096 chars.
	for _, part := range chunk(text, 4000) {
		if err := t.api(context.Background(), "sendMessage", map[string]any{"chat_id": chatID, "text": part}, nil); err != nil {
			log.Printf("telegram send: %v", err)
		}
	}
}

type tgFile struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type tgMessage struct {
	MessageID    int64              `json:"message_id"`
	Chat         struct{ ID int64 } `json:"chat"`
	From         struct{ ID int64 } `json:"from"`
	Text         string             `json:"text"`
	Caption      string             `json:"caption"`
	MediaGroupID string             `json:"media_group_id"`
	Photo        []tgFile           `json:"photo"`
	Document     *tgFile            `json:"document"`
	Audio        *tgFile            `json:"audio"`
	Voice        *tgFile            `json:"voice"`
	Video        *tgFile            `json:"video"`
	VideoNote    *tgFile            `json:"video_note"`
}

type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

func (t *Telegram) Run(ctx context.Context, h Handler) {
	var offset int64
	for ctx.Err() == nil {
		var ups []tgUpdate
		err := t.api(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 50}, &ups)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("telegram poll: %v", err)
				time.Sleep(3 * time.Second)
			}
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			m := u.Message
			if m == nil {
				continue
			}
			if len(t.allowed) > 0 && !t.allowed[m.From.ID] {
				log.Printf("telegram: ignored user %d (not in allowed_users)", m.From.ID)
				continue
			}
			t.api(ctx, "sendChatAction", map[string]any{"chat_id": m.Chat.ID, "action": "typing"}, nil)
			go t.handle(ctx, m, h)
		}
	}
}

func (t *Telegram) handle(ctx context.Context, m *tgMessage, h Handler) {
	conv := fmt.Sprintf("tg:%d", m.Chat.ID)
	text := m.Text
	if text == "" {
		text = m.Caption
	}
	var notes []string
	var blocks []llm.Block

	var file *tgFile
	kind := ""
	switch {
	case len(m.Photo) > 0:
		file, kind = &m.Photo[len(m.Photo)-1], "ảnh"
	case m.Document != nil:
		file, kind = m.Document, "file"
	case m.Voice != nil:
		file, kind = m.Voice, "tin nhắn thoại"
	case m.Audio != nil:
		file, kind = m.Audio, "audio"
	case m.Video != nil:
		file, kind = m.Video, "video"
	case m.VideoNote != nil:
		file, kind = m.VideoNote, "video tròn"
	}
	if file != nil {
		note, blk, err := t.receive(ctx, m, file, kind)
		if err != nil {
			log.Printf("telegram file: %v", err)
			note = fmt.Sprintf("[User gửi %s %q nhưng không tải được: %v]", kind, file.FileName, err)
		}
		notes = append(notes, note)
		blocks = append(blocks, blk...)
	}
	if text == "" && len(notes) == 0 {
		return
	}
	full := strings.TrimSpace(strings.Join(append(notes, text), "\n\n"))

	if m.MediaGroupID == "" {
		h(conv, full, blocks)
		return
	}
	// Albums arrive as separate messages; collect them and deliver as one.
	t.mu.Lock()
	defer t.mu.Unlock()
	g := t.groups[m.MediaGroupID]
	if g == nil {
		g = &album{conv: conv}
		t.groups[m.MediaGroupID] = g
		id := m.MediaGroupID
		g.timer = time.AfterFunc(2*time.Second, func() {
			t.mu.Lock()
			g := t.groups[id]
			delete(t.groups, id)
			t.mu.Unlock()
			h(g.conv, strings.Join(g.texts, "\n\n"), g.blocks)
		})
	} else {
		g.timer.Reset(2 * time.Second)
	}
	g.texts = append(g.texts, full)
	g.blocks = append(g.blocks, blocks...)
}

const (
	maxDownload  = 20 << 20 // Bot API getFile limit
	maxInlineImg = 3_700_000
	maxInlinePDF = 15 << 20
	maxInlineTxt = 60_000
)

var supportedImages = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

// receive downloads a file into the workspace inbox and returns a note for the
// agent plus any blocks the model can read directly (image, PDF).
func (t *Telegram) receive(ctx context.Context, m *tgMessage, f *tgFile, kind string) (string, []llm.Block, error) {
	if f.FileSize > maxDownload {
		return fmt.Sprintf("[User gửi %s %q (%s) — quá 20MB, Telegram bot không tải được. Hãy nhờ user gửi link hoặc file nhỏ hơn.]",
			kind, f.FileName, humanSize(f.FileSize)), nil, nil
	}
	data, err := t.download(ctx, f.FileID)
	if err != nil {
		return "", nil, err
	}
	mime := f.MimeType
	if mime == "" || mime == "application/octet-stream" {
		mime = http.DetectContentType(data)
	}
	mime = strings.TrimSpace(strings.Split(mime, ";")[0])
	name := f.FileName
	if name == "" {
		name = fmt.Sprintf("%s-%d%s", strings.ReplaceAll(kind, " ", "-"), m.MessageID, extFor(mime))
	}
	rel, err := t.save(name, data)
	if err != nil {
		return "", nil, err
	}
	note := fmt.Sprintf("[User gửi %s: %s (%s, %s) — đã lưu trong workspace tại %s]", kind, name, mime, humanSize(int64(len(data))), rel)
	b64 := func() string { return base64.StdEncoding.EncodeToString(data) }

	switch {
	case supportedImages[mime] && len(data) <= maxInlineImg:
		return note, []llm.Block{{Type: llm.Image, MediaType: mime, Data: b64()}}, nil
	case mime == "application/pdf" && len(data) <= maxInlinePDF:
		return note, []llm.Block{{Type: llm.Document, MediaType: mime, Data: b64()}}, nil
	case isText(mime, name, data):
		body := string(data)
		if len(body) > maxInlineTxt {
			body = body[:maxInlineTxt] + fmt.Sprintf("\n…[cắt bớt; đọc toàn bộ bằng read_file %s]", rel)
		}
		return note + "\n[Nội dung " + name + "]\n" + body, nil, nil
	case strings.HasPrefix(mime, "audio/") || strings.HasPrefix(mime, "video/"):
		return note + "\n(Chưa hỗ trợ nghe/xem trực tiếp; có thể giao worker xử lý file này.)", nil, nil
	}
	return note, nil, nil
}

var unsafeName = regexp.MustCompile(`[^\p{L}\p{N}._-]+`)

func (t *Telegram) save(name string, data []byte) (string, error) {
	name = strings.Trim(unsafeName.ReplaceAllString(filepath.Base(name), "_"), "._")
	if name == "" {
		name = "file"
	}
	dir := filepath.Join("inbox", time.Now().Format("2006-01-02"))
	if err := os.MkdirAll(filepath.Join(t.workspace, dir), 0o755); err != nil {
		return "", err
	}
	rel := filepath.Join(dir, name)
	ext := filepath.Ext(name)
	for i := 2; ; i++ { // never overwrite an earlier upload
		if _, err := os.Stat(filepath.Join(t.workspace, rel)); os.IsNotExist(err) {
			break
		}
		rel = filepath.Join(dir, fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), i, ext))
	}
	return rel, os.WriteFile(filepath.Join(t.workspace, rel), data, 0o644)
}

func (t *Telegram) download(ctx context.Context, fileID string) ([]byte, error) {
	var f struct {
		FilePath string `json:"file_path"`
	}
	if err := t.api(ctx, "getFile", map[string]any{"file_id": fileID}, &f); err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", t.base+"/file/bot"+t.token+"/"+f.FilePath, nil)
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
}

var textExts = map[string]bool{
	".txt": true, ".md": true, ".csv": true, ".tsv": true, ".json": true, ".yaml": true, ".yml": true, ".toml": true,
	".xml": true, ".html": true, ".htm": true, ".log": true, ".ini": true, ".env": true, ".sql": true, ".srt": true,
	".go": true, ".py": true, ".js": true, ".ts": true, ".tsx": true, ".jsx": true, ".java": true, ".kt": true, ".c": true,
	".h": true, ".cpp": true, ".cs": true, ".rb": true, ".php": true, ".rs": true, ".swift": true, ".sh": true, ".css": true,
}

func isText(mime, name string, data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	return strings.HasPrefix(mime, "text/") || textExts[strings.ToLower(filepath.Ext(name))] ||
		strings.Contains(mime, "json") || strings.Contains(mime, "xml") || strings.Contains(mime, "yaml") ||
		strings.Contains(mime, "javascript")
}

func extFor(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "audio/ogg":
		return ".ogg"
	case "audio/mpeg":
		return ".mp3"
	case "video/mp4":
		return ".mp4"
	case "application/pdf":
		return ".pdf"
	}
	return ""
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func chunk(s string, n int) []string {
	r := []rune(s)
	var out []string
	for len(r) > n {
		cut := n
		for i := n; i > n/2; i-- {
			if r[i] == '\n' {
				cut = i
				break
			}
		}
		out = append(out, string(r[:cut]))
		r = r[cut:]
	}
	return append(out, string(r))
}

// SendFile uploads a file: images as photos (shown inline), everything else as documents.
func (t *Telegram) SendFile(conv, path, caption string) error {
	chatID, ok := strings.CutPrefix(conv, "tg:")
	if !ok {
		return fmt.Errorf("not a telegram conversation")
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	method, field := "sendDocument", "document"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		if st.Size() <= 10<<20 {
			method, field = "sendPhoto", "photo"
		}
	}
	if st.Size() > 50<<20 {
		return fmt.Errorf("file %s quá 50MB, Telegram bot không gửi được", humanSize(st.Size()))
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("chat_id", chatID)
	if caption != "" {
		w.WriteField("caption", clip(caption, 1000))
	}
	part, err := w.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	w.Close()

	req, _ := http.NewRequest("POST", t.base+"/bot"+t.token+"/"+method, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	json.NewDecoder(resp.Body).Decode(&env)
	if !env.OK {
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	return nil
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

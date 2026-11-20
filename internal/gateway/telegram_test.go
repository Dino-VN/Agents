package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentd/internal/llm"
)

func fakeBotAPI(t *testing.T, files map[string][]byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			var body struct {
				FileID string `json:"file_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]string{"file_path": "docs/" + body.FileID}})
		case strings.Contains(r.URL.Path, "/file/bot"):
			w.Write(files[filepath.Base(r.URL.Path)])
		default:
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		}
	}))
}

func TestReceiveFiles(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	files := map[string][]byte{
		"note":  []byte("# Ghi chú\nmua sữa"),
		"pdf":   []byte("%PDF-1.4 fake"),
		"img":   png,
		"bin":   {0xff, 0xfe, 0x00, 0x01, 0x02},
		"voice": []byte("OggS fake"),
	}
	srv := fakeBotAPI(t, files)
	defer srv.Close()
	ws := t.TempDir()
	tg := NewTelegram("TOKEN", nil, ws)
	tg.base = srv.URL

	type got struct {
		conv, text string
		blocks     []llm.Block
	}
	var out []got
	h := func(conv, text string, b []llm.Block) { out = append(out, got{conv, text, b}) }

	msgs := []*tgMessage{
		{MessageID: 1, Document: &tgFile{FileID: "note", FileName: "notes.md", MimeType: "text/markdown"}, Caption: "tóm tắt giúp"},
		{MessageID: 2, Document: &tgFile{FileID: "pdf", FileName: "bao cao.pdf", MimeType: "application/pdf"}},
		{MessageID: 3, Photo: []tgFile{{FileID: "img"}}},
		{MessageID: 4, Document: &tgFile{FileID: "bin", FileName: "data.bin"}},
		{MessageID: 5, Voice: &tgFile{FileID: "voice", MimeType: "audio/ogg"}},
	}
	for _, m := range msgs {
		m.Chat.ID = 42
		tg.handle(context.Background(), m, h)
	}
	if len(out) != 5 {
		t.Fatalf("got %d deliveries, want 5", len(out))
	}
	if !strings.Contains(out[0].text, "mua sữa") || !strings.Contains(out[0].text, "tóm tắt giúp") || out[0].conv != "tg:42" {
		t.Errorf("text file not inlined with caption: %q", out[0].text)
	}
	if len(out[1].blocks) != 1 || out[1].blocks[0].Type != llm.Document {
		t.Errorf("pdf should become a document block: %+v", out[1].blocks)
	}
	if len(out[2].blocks) != 1 || out[2].blocks[0].Type != llm.Image || out[2].blocks[0].MediaType != "image/png" {
		t.Errorf("photo should become an image block: %+v", out[2].blocks)
	}
	if len(out[3].blocks) != 0 || !strings.Contains(out[3].text, "data.bin") {
		t.Errorf("binary file should only be saved + noted: %q", out[3].text)
	}
	if !strings.Contains(out[4].text, "thoại") {
		t.Errorf("voice note missing: %q", out[4].text)
	}
	// Files land in the workspace inbox, with unsafe characters replaced.
	matches, _ := filepath.Glob(filepath.Join(ws, "inbox", "*", "bao_cao.pdf"))
	if len(matches) != 1 {
		t.Fatalf("pdf not saved in inbox: %v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); string(b) != "%PDF-1.4 fake" {
		t.Errorf("saved pdf content mismatch")
	}
	// A second upload with the same name must not overwrite the first.
	tg.handle(context.Background(), msgs[1], h)
	matches, _ = filepath.Glob(filepath.Join(ws, "inbox", "*", "bao_cao*.pdf"))
	if len(matches) != 2 {
		t.Errorf("expected 2 saved pdfs, got %v", matches)
	}
}

func TestSendFile(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(1 << 20)
		method := filepath.Base(r.URL.Path)
		for field := range r.MultipartForm.File {
			f := r.MultipartForm.File[field][0]
			got = append(got, method+":"+field+":"+f.Filename+":"+r.FormValue("chat_id")+":"+r.FormValue("caption"))
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{}})
	}))
	defer srv.Close()
	tg := NewTelegram("TOKEN", nil, t.TempDir())
	tg.base = srv.URL
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "shot.jpg"), []byte("\xff\xd8\xff jpeg"), 0o644)
	os.WriteFile(filepath.Join(dir, "report.pdf"), []byte("%PDF"), 0o644)
	if err := tg.SendFile("tg:42", filepath.Join(dir, "shot.jpg"), "ảnh chụp"); err != nil {
		t.Fatal(err)
	}
	if err := tg.SendFile("tg:42", filepath.Join(dir, "report.pdf"), ""); err != nil {
		t.Fatal(err)
	}
	want := []string{"sendPhoto:photo:shot.jpg:42:ảnh chụp", "sendDocument:document:report.pdf:42:"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v", got)
	}
}

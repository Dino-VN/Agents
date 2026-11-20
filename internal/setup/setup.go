// Package setup is the interactive `agentd setup` wizard: API endpoint and key,
// model choice (tested live), Telegram bot, then writes config.yaml + secrets.env.
package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"agentd"
	"agentd/internal/config"
	"agentd/internal/llm"
)

const (
	keyEnv = "AGENTD_API_KEY"
	tgEnv  = "TELEGRAM_BOT_TOKEN"
)

type answers struct {
	Kind    string // official | anthropic | openai
	BaseURL string
	Auth    string // "" | bearer
	Key     string

	Smart, Strong, Cheap string

	TGToken string
	TGUsers []int64

	Search    string // tavily | brave | ""
	SearchKey string
}

type wizard struct {
	in      *bufio.Reader
	cfgPath string
	envPath string
	old     *config.Config
	oldEnv  map[string]string
	a       answers
	models  []string
	http    *http.Client
}

func Run(cfgPath string) error {
	abs, _ := filepath.Abs(cfgPath)
	w := &wizard{
		in:      bufio.NewReader(os.Stdin),
		cfgPath: abs,
		envPath: filepath.Join(filepath.Dir(abs), "secrets.env"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
	if err := bootstrap(filepath.Dir(abs)); err != nil {
		return err
	}
	w.old, _ = config.Load(abs)
	w.oldEnv, _ = config.ReadEnvFile(w.envPath)
	if w.oldEnv == nil {
		w.oldEnv = map[string]string{}
	}

	fmt.Printf("=== agentd setup ===  thư mục: %s  (Enter = giữ giá trị trong [ngoặc])\n", filepath.Dir(abs))
	if err := w.stepAPI(); err != nil {
		return err
	}
	w.stepModels()
	w.stepTelegram()
	w.stepSearch()
	if err := w.write(); err != nil {
		return err
	}
	w.finish()
	return nil
}

// ---------- prompts ----------

func (w *wizard) ask(label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, err := w.in.ReadString('\n')
	if err != nil && line == "" {
		fmt.Println("\nĐã huỷ (hết input).")
		os.Exit(1)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func (w *wizard) askSecret(label, existing string) string {
	hint := ""
	if existing != "" {
		hint = " [Enter = giữ " + mask(existing) + "]"
	}
	fmt.Printf("%s%s: ", label, hint)
	var line string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, _ := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		line = string(b)
	} else {
		var err error
		if line, err = w.in.ReadString('\n'); err != nil && line == "" {
			fmt.Println("\nĐã huỷ (hết input).")
			os.Exit(1)
		}
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return existing
	}
	return line
}

func (w *wizard) yes(label string, def bool) bool {
	d := "y/N"
	if def {
		d = "Y/n"
	}
	ans := strings.ToLower(w.ask(label+" ("+d+")", ""))
	if ans == "" {
		return def
	}
	return ans == "y" || ans == "yes" || ans == "c" || ans == "co" || ans == "có"
}

func mask(s string) string {
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "…" + s[len(s)-4:]
}

// ---------- step 1: API ----------

func (w *wizard) stepAPI() error {
	fmt.Println("\n[1/4] API model")
	fmt.Println("  1) Anthropic chính thức (api.anthropic.com)")
	fmt.Println("  2) API tuỳ chỉnh kiểu Anthropic (/v1/messages) — proxy, hub...")
	fmt.Println("  3) API tuỳ chỉnh kiểu OpenAI (/v1/chat/completions) — OpenRouter, Ollama...")
	def := "2"
	if p, ok := w.oldProvider(); ok {
		switch {
		case p.Type == "openai":
			def = "3"
		case p.BaseURL == "":
			def = "1"
		}
		w.a.BaseURL, w.a.Auth = p.BaseURL, p.Auth
	}
	switch w.ask("Chọn", def) {
	case "1":
		w.a.Kind, w.a.BaseURL = "official", ""
	case "3":
		w.a.Kind = "openai"
	default:
		w.a.Kind = "anthropic"
	}
	if w.a.Kind != "official" {
		for {
			w.a.BaseURL = strings.TrimRight(w.ask("Base URL (vd https://api.example.com)", w.a.BaseURL), "/")
			if strings.HasPrefix(w.a.BaseURL, "http://") || strings.HasPrefix(w.a.BaseURL, "https://") {
				break
			}
			fmt.Println("  URL phải bắt đầu bằng http:// hoặc https://")
		}
		if w.a.Kind == "anthropic" {
			// The SDK appends /v1/messages itself.
			w.a.BaseURL = strings.TrimSuffix(w.a.BaseURL, "/v1")
		}
	}
	for w.a.Key == "" {
		w.a.Key = w.askSecret("API key", w.oldEnv[keyEnv])
	}

	fmt.Print("  Đang kết nối… ")
	models, err := w.listModels()
	if err != nil {
		fmt.Println("không lấy được danh sách model:", err)
		fmt.Println("  (Vẫn tiếp tục được: bạn tự gõ tên model ở bước sau.)")
		return nil
	}
	w.models = models
	auth := "x-api-key"
	if w.a.Auth == "bearer" {
		auth = "Bearer"
	}
	if w.a.Kind == "openai" {
		auth = "Bearer"
	}
	fmt.Printf("OK — %d model, xác thực bằng %s\n", len(models), auth)
	return nil
}

func (w *wizard) oldProvider() (config.Provider, bool) {
	if w.old == nil {
		return config.Provider{}, false
	}
	if p, ok := w.old.Providers["main"]; ok {
		return p, true
	}
	return config.Provider{}, false
}

// listModels fetches model ids and, for Anthropic-style APIs, detects whether the
// endpoint wants x-api-key or Bearer auth.
func (w *wizard) listModels() ([]string, error) {
	switch w.a.Kind {
	case "openai":
		ids, err := w.getModels(w.a.BaseURL+"/models", "bearer")
		if err != nil && !strings.HasSuffix(w.a.BaseURL, "/v1") {
			if ids2, err2 := w.getModels(w.a.BaseURL+"/v1/models", "bearer"); err2 == nil {
				w.a.BaseURL += "/v1"
				return ids2, nil
			}
		}
		return ids, err
	default:
		base := w.a.BaseURL
		if base == "" {
			base = "https://api.anthropic.com"
		}
		ids, err := w.getModels(base+"/v1/models", "x-api-key")
		if err == nil {
			w.a.Auth = ""
			return ids, nil
		}
		if ids2, err2 := w.getModels(base+"/v1/models", "bearer"); err2 == nil {
			w.a.Auth = "bearer"
			return ids2, nil
		}
		return nil, err
	}
}

func (w *wizard) getModels(url, auth string) ([]string, error) {
	req, _ := http.NewRequest("GET", url, nil)
	if auth == "bearer" {
		req.Header.Set("Authorization", "Bearer "+w.a.Key)
	} else {
		req.Header.Set("x-api-key", w.a.Key)
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, clip(string(body), 200))
	}
	var out struct {
		Data []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Data) == 0 {
		return nil, fmt.Errorf("phản hồi không phải danh sách model")
	}
	var ids []string
	for _, m := range out.Data {
		if m.Kind == "" || m.Kind == "chat" {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// ---------- step 2: models ----------

var prefs = map[string][]string{
	"smart":  {"claude-sonnet-5-5", "claude-sonnet-5", "claude-sonnet-4-6", "gpt-6-sol", "gemini-3.1-pro"},
	"strong": {"claude-opus-5-5", "claude-opus-5.5", "claude-opus-5", "claude-opus-4-8", "gpt-6-sol", "gemini-3.1-pro", "claude-sonnet-5"},
	"cheap":  {"claude-haiku-5-5", "gemini-3.5-flash", "gemini-3.5-flash-lite", "deepseek-v4-flash", "claude-haiku-4-5"},
}

func (w *wizard) stepModels() {
	fmt.Println("\n[2/4] Chọn model")
	if len(w.models) > 0 && w.yes(fmt.Sprintf("Xem danh sách %d model?", len(w.models)), len(w.models) <= 60) {
		printColumns(w.models)
	}
	tiers := []struct{ key, label string }{
		{"smart", "Model CHAT (nói chuyện với bạn, điều phối; cần thông minh + nhanh)"},
		{"strong", "Model MẠNH (worker code/suy luận khó)"},
		{"cheap", "Model RẺ (việc vặt, chưng cất trí nhớ)"},
	}
	tested := map[string]bool{}
	for _, t := range tiers {
		def := w.oldModel(t.key)
		if def == "" {
			def = w.pick(prefs[t.key])
		}
		for {
			m := w.ask(t.label, def)
			if m == "" {
				continue
			}
			if len(w.models) > 0 && !contains(w.models, m) {
				fmt.Printf("  ⚠️ %q không có trong danh sách của API.\n", m)
			}
			if tested[m] || w.testModel(m) {
				tested[m] = true
				w.set(t.key, m)
				break
			}
			if w.yes("  Vẫn dùng model này?", false) {
				w.set(t.key, m)
				break
			}
		}
	}
}

func (w *wizard) set(tier, m string) {
	switch tier {
	case "smart":
		w.a.Smart = m
	case "strong":
		w.a.Strong = m
	case "cheap":
		w.a.Cheap = m
	}
}

func (w *wizard) oldModel(tier string) string {
	if w.old == nil {
		return ""
	}
	return w.old.Models[tier].ID
}

func (w *wizard) pick(candidates []string) string {
	if len(w.models) == 0 && w.a.Kind == "official" {
		return candidates[0]
	}
	for _, c := range candidates {
		if contains(w.models, c) {
			return c
		}
	}
	return ""
}

// testModel makes a real tool-use call through the same code path agentd uses.
func (w *wizard) testModel(id string) bool {
	fmt.Printf("  Đang thử %s (gọi tool)… ", id)
	cfg := w.buildConfig(map[string]string{"t": id})
	os.Setenv(keyEnv, w.a.Key)
	router, err := llm.NewRouter(cfg)
	if err != nil {
		fmt.Println("lỗi:", err)
		return false
	}
	tool := llm.ToolDef{Name: "get_time", Description: "Get the current time", Properties: map[string]any{}}
	s, err := router.NewSession("t", "", "You are a test. Use tools when asked.", []llm.ToolDef{tool}, nil)
	if err != nil {
		fmt.Println("lỗi:", err)
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	start := time.Now()
	turn, err := s.Send(ctx, []llm.Block{llm.TextBlock("What time is it? Call the get_time tool.")})
	if err != nil {
		fmt.Println("❌", clip(err.Error(), 200))
		return false
	}
	took := time.Since(start).Round(100 * time.Millisecond)
	if len(turn.Calls) == 0 {
		fmt.Printf("⚠️ trả lời được (%s) nhưng không gọi tool — agent sẽ kém\n", took)
		return false
	}
	fmt.Printf("✅ %s\n", took)
	return true
}

// ---------- step 3: Telegram ----------

func (w *wizard) stepTelegram() {
	fmt.Println("\n[3/4] Telegram bot (Enter để bỏ qua nếu chỉ dùng terminal)")
	if w.old != nil {
		w.a.TGUsers = w.old.Telegram.AllowedUsers
	}
	for {
		w.a.TGToken = w.askSecret("Bot token (lấy từ @BotFather)", w.oldEnv[tgEnv])
		if w.a.TGToken == "" {
			return
		}
		var me struct {
			Username string `json:"username"`
		}
		if err := w.tg("getMe", nil, &me); err != nil {
			fmt.Println("  ❌ token không hợp lệ:", err)
			continue
		}
		fmt.Printf("  ✅ bot @%s\n", me.Username)
		if len(w.a.TGUsers) > 0 {
			fmt.Printf("  User được phép hiện tại: %v\n", w.a.TGUsers)
			if !w.yes("  Thêm user khác?", false) {
				return
			}
		}
		w.captureUser(me.Username)
		return
	}
}

func (w *wizard) captureUser(bot string) {
	fmt.Printf("  👉 Mở Telegram, nhắn một tin bất kỳ cho @%s (chờ 90s; hoặc gõ thẳng user id rồi Enter)…\n", bot)
	typed := make(chan string, 1)
	go func() {
		line, _ := w.in.ReadString('\n')
		typed <- strings.TrimSpace(line)
	}()
	var offset int64
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case s := <-typed:
			if id, err := strconv.ParseInt(s, 10, 64); err == nil {
				w.a.TGUsers = appendUnique(w.a.TGUsers, id)
				fmt.Printf("  ✅ thêm user %d\n", id)
			}
			return
		default:
		}
		var ups []struct {
			UpdateID int64 `json:"update_id"`
			Message  *struct {
				From struct {
					ID        int64  `json:"id"`
					FirstName string `json:"first_name"`
					Username  string `json:"username"`
				} `json:"from"`
			} `json:"message"`
		}
		err := w.tg("getUpdates", map[string]any{"offset": offset, "timeout": 3}, &ups)
		if err != nil && strings.Contains(err.Error(), "Conflict") {
			fmt.Println("  ⚠️ bot đang được agentd khác dùng. Dừng service (rc-service agentd stop) rồi chạy lại setup, hoặc gõ user id.")
			s := <-typed
			if id, err := strconv.ParseInt(s, 10, 64); err == nil {
				w.a.TGUsers = appendUnique(w.a.TGUsers, id)
			}
			return
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			if u.Message != nil {
				f := u.Message.From
				w.a.TGUsers = appendUnique(w.a.TGUsers, f.ID)
				fmt.Printf("  ✅ nhận tin từ %s (@%s, id %d) — đã cho phép. Nhấn Enter để tiếp tục.\n", f.FirstName, f.Username, f.ID)
				w.tg("getUpdates", map[string]any{"offset": offset, "timeout": 0}, nil) // ack
				<-typed
				return
			}
		}
	}
	fmt.Println("  Hết thời gian chờ. Có thể thêm user id vào telegram.allowed_users trong config.yaml sau. Nhấn Enter.")
	<-typed
}

func (w *wizard) tg(method string, body any, out any) error {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "https://api.telegram.org/bot"+w.a.TGToken+"/"+method, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.http.Do(req)
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
		return fmt.Errorf("%s", env.Description)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// ---------- step 4: search ----------

func (w *wizard) stepSearch() {
	fmt.Println("\n[4/4] Web search (mặc định DuckDuckGo miễn phí; Tavily/Brave tốt hơn)")
	def := "1"
	switch {
	case w.oldEnv["TAVILY_API_KEY"] != "":
		def = "2"
	case w.oldEnv["BRAVE_API_KEY"] != "":
		def = "3"
	}
	switch w.ask("  1) DuckDuckGo  2) Tavily  3) Brave", def) {
	case "2":
		w.a.Search = "tavily"
		w.a.SearchKey = w.askSecret("  Tavily API key", w.oldEnv["TAVILY_API_KEY"])
	case "3":
		w.a.Search = "brave"
		w.a.SearchKey = w.askSecret("  Brave API key", w.oldEnv["BRAVE_API_KEY"])
	}
}

// ---------- write ----------

func (w *wizard) write() error {
	cfg := renderConfig(w.a)
	backedUp := false
	if _, err := os.Stat(w.cfgPath); err == nil {
		backedUp = os.Rename(w.cfgPath, w.cfgPath+".bak") == nil
	}
	if err := os.WriteFile(w.cfgPath, []byte(cfg), 0o644); err != nil {
		return err
	}
	env := w.oldEnv
	env[keyEnv] = w.a.Key
	setOrDelete(env, tgEnv, w.a.TGToken)
	delete(env, "TAVILY_API_KEY")
	delete(env, "BRAVE_API_KEY")
	switch w.a.Search {
	case "tavily":
		setOrDelete(env, "TAVILY_API_KEY", w.a.SearchKey)
	case "brave":
		setOrDelete(env, "BRAVE_API_KEY", w.a.SearchKey)
	}
	var b strings.Builder
	b.WriteString("# agentd secrets — written by `agentd setup`. Keep private (chmod 600).\n")
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, strconv.Quote(env[k]))
	}
	if err := os.WriteFile(w.envPath, []byte(b.String()), 0o600); err != nil {
		return err
	}
	os.Chmod(w.envPath, 0o600)
	fmt.Printf("\n✅ Đã ghi %s và %s (chmod 600)\n", w.cfgPath, w.envPath)
	if backedUp {
		fmt.Println("   Bản config cũ: config.yaml.bak")
	}
	return nil
}

func (w *wizard) finish() {
	if _, err := os.Stat("/etc/init.d/agentd"); err == nil {
		if w.yes("Khởi động lại service agentd ngay?", true) {
			cmd := exec.Command("rc-service", "agentd", "restart")
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				fmt.Println("Lỗi:", err)
			}
		}
		return
	}
	if w.a.TGToken != "" {
		fmt.Println("Chạy bot: ./agentd -telegram   ·   Chat trong terminal: ./agentd")
	} else {
		fmt.Println("Chạy: ./agentd")
	}
}

// buildConfig makes an in-memory config with the chosen provider and the given models.
func (w *wizard) buildConfig(models map[string]string) *config.Config {
	p := config.Provider{Type: "anthropic", BaseURL: w.a.BaseURL, APIKeyEnv: keyEnv, Auth: w.a.Auth}
	if w.a.Kind == "openai" {
		p = config.Provider{Type: "openai", BaseURL: w.a.BaseURL, APIKeyEnv: keyEnv}
	}
	cfg := &config.Config{Providers: map[string]config.Provider{"main": p}, Models: map[string]config.Model{}}
	for name, id := range models {
		cfg.Models[name] = config.Model{Provider: "main", ID: id, MaxTokens: 2000}
	}
	return cfg
}

// bootstrap creates the home directory and copies the embedded default soul,
// role prompts and skills into it, never overwriting files the user has.
func bootstrap(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return fs.WalkDir(agentd.Defaults, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, p)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if _, err := os.Stat(dst); err == nil {
			return nil
		}
		data, err := agentd.Defaults.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
}

// ---------- helpers ----------

func printColumns(items []string) {
	width := 0
	for _, s := range items {
		width = max(width, len(s))
	}
	width += 2
	cols := max(1, 100/width)
	for i, s := range items {
		fmt.Printf("  %-*s", width, s)
		if (i+1)%cols == 0 || i == len(items)-1 {
			fmt.Println()
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func appendUnique(list []int64, id int64) []int64 {
	for _, x := range list {
		if x == id {
			return list
		}
	}
	return append(list, id)
}

func setOrDelete(m map[string]string, k, v string) {
	if v == "" {
		delete(m, k)
	} else {
		m[k] = v
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

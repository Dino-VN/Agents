package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126 Safari/537.36"

type searchResult struct{ Title, URL, Snippet string }

// RegisterWeb adds web_search and web_fetch. backend: auto | tavily | brave | duckduckgo.
func RegisterWeb(r *Registry, backend string) {
	r.Add(Tool{
		Def: Def("web_search", "Tìm kiếm web. Trả về danh sách tiêu đề, URL và đoạn trích.",
			map[string]any{"query": Str("Từ khoá tìm kiếm"), "max_results": Int("Số kết quả (mặc định 6)")}, "query"),
		Run: func(ctx context.Context, c *Call, in json.RawMessage) (string, error) {
			a, err := Args[struct {
				Query string `json:"query"`
				Max   int    `json:"max_results"`
			}](in)
			if err != nil {
				return "", err
			}
			if a.Max <= 0 || a.Max > 15 {
				a.Max = 6
			}
			res, err := search(ctx, backend, a.Query, a.Max)
			if err != nil {
				return "", err
			}
			if len(res) == 0 {
				return "Không có kết quả.", nil
			}
			var b strings.Builder
			for i, x := range res {
				fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1, x.Title, x.URL, x.Snippet)
			}
			return b.String(), nil
		},
	})
	r.Add(Tool{
		Def: Def("web_fetch", "Tải một URL và trả về nội dung dạng text (HTML đã được lược bỏ thẻ).",
			map[string]any{"url": Str("URL http(s)"), "max_chars": Int("Giới hạn ký tự (mặc định 15000)")}, "url"),
		Run: func(ctx context.Context, c *Call, in json.RawMessage) (string, error) {
			a, err := Args[struct {
				URL string `json:"url"`
				Max int    `json:"max_chars"`
			}](in)
			if err != nil {
				return "", err
			}
			if a.Max <= 0 {
				a.Max = 15000
			}
			return fetch(ctx, a.URL, a.Max)
		},
	})
}

func search(ctx context.Context, backend, q string, n int) ([]searchResult, error) {
	if backend == "auto" {
		switch {
		case os.Getenv("TAVILY_API_KEY") != "":
			backend = "tavily"
		case os.Getenv("BRAVE_API_KEY") != "":
			backend = "brave"
		default:
			backend = "duckduckgo"
		}
	}
	switch backend {
	case "tavily":
		return searchTavily(ctx, q, n)
	case "brave":
		return searchBrave(ctx, q, n)
	default:
		return searchDDG(ctx, q, n)
	}
}

func searchTavily(ctx context.Context, q string, n int) ([]searchResult, error) {
	body, _ := json.Marshal(map[string]any{"query": q, "max_results": n})
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.tavily.com/search", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("TAVILY_API_KEY"))
	var out struct {
		Results []struct{ Title, URL, Content string } `json:"results"`
	}
	if err := doJSON(req, &out); err != nil {
		return nil, err
	}
	var res []searchResult
	for _, r := range out.Results {
		res = append(res, searchResult{r.Title, r.URL, Clip(r.Content, 300)})
	}
	return res, nil
}

func searchBrave(ctx context.Context, q string, n int) ([]searchResult, error) {
	u := "https://api.search.brave.com/res/v1/web/search?count=" + fmt.Sprint(n) + "&q=" + url.QueryEscape(q)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("X-Subscription-Token", os.Getenv("BRAVE_API_KEY"))
	req.Header.Set("Accept", "application/json")
	var out struct {
		Web struct {
			Results []struct {
				Title, URL  string
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := doJSON(req, &out); err != nil {
		return nil, err
	}
	var res []searchResult
	for _, r := range out.Web.Results {
		res = append(res, searchResult{r.Title, r.URL, stripTags(r.Description)})
	}
	return res, nil
}

var (
	ddgLink    = regexp.MustCompile(`(?s)class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippet = regexp.MustCompile(`(?s)class="result__snippet"[^>]*>(.*?)</a>`)
)

func searchDDG(ctx context.Context, q string, n int) ([]searchResult, error) {
	form := url.Values{"q": {q}}
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://html.duckduckgo.com/html/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	page, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	links := ddgLink.FindAllStringSubmatch(string(page), n)
	snips := ddgSnippet.FindAllStringSubmatch(string(page), n)
	var res []searchResult
	for i, l := range links {
		href := html.UnescapeString(l[1])
		if u, err := url.Parse(href); err == nil && u.Query().Get("uddg") != "" {
			href = u.Query().Get("uddg")
		}
		r := searchResult{Title: stripTags(l[2]), URL: href}
		if i < len(snips) {
			r.Snippet = stripTags(snips[i][1])
		}
		res = append(res, r)
	}
	return res, nil
}

func doJSON(req *http.Request, out any) error {
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("search API %d: %s", resp.StatusCode, Clip(string(data), 300))
	}
	return json.Unmarshal(data, out)
}

func fetch(ctx context.Context, rawURL string, max int) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("chỉ hỗ trợ URL http(s)")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 3<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	text := string(data)
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "html") || strings.Contains(text[:min(len(text), 500)], "<html") {
		text = htmlToText(text)
	}
	return Clip(text, max), nil
}

var (
	reDrop   = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head|nav|footer)[^>]*>.*?</(script|style|noscript|svg|head|nav|footer)>`)
	reBlock  = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/h[1-6]|/tr)[^>]*>`)
	reTag    = regexp.MustCompile(`(?s)<[^>]+>`)
	reSpaces = regexp.MustCompile(`[ \t]+`)
	reLines  = regexp.MustCompile(`\n\s*\n+`)
)

func htmlToText(s string) string {
	s = reDrop.ReplaceAllString(s, " ")
	s = reBlock.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = reSpaces.ReplaceAllString(s, " ")
	s = reLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func stripTags(s string) string {
	return strings.TrimSpace(html.UnescapeString(reTag.ReplaceAllString(s, "")))
}

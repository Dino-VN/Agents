// Package config loads config.yaml: providers, models, roles and limits.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Provider struct {
	Type      string `yaml:"type"` // anthropic | openai
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
	Auth      string `yaml:"auth"` // anthropic only: x-api-key (default) | bearer
}

type Model struct {
	Provider  string `yaml:"provider"`
	ID        string `yaml:"id"`
	MaxTokens int64  `yaml:"max_tokens"`
	Effort    string `yaml:"effort"`
	// Fallback enables Anthropic's server-side refusal fallback ("default" routing).
	Fallback bool   `yaml:"fallback"`
	Card     string `yaml:"card"` // shown to the chat agent so it can pick models
}

type Role struct {
	Description  string   `yaml:"description"`
	Model        string   `yaml:"model"`
	Prompt       string   `yaml:"prompt"`
	Tools        []string `yaml:"tools"`       // glob patterns allowed
	Ask          []string `yaml:"ask"`         // glob patterns that need user approval
	ShellAllow   []string `yaml:"shell_allow"` // shell commands auto-approved (glob, no ; | & $ etc.)
	MaxToolCalls int      `yaml:"max_tool_calls"`
	// Effort is the role's default thinking effort; delegated tasks never run below it.
	Effort string `yaml:"effort"`
}

type Limits struct {
	MaxDepth        int           `yaml:"max_depth"`
	MaxChildren     int           `yaml:"max_children"`
	MaxTurns        int           `yaml:"max_turns"`
	WorkerTimeout   time.Duration `yaml:"worker_timeout"`
	TaskTokenBudget int64         `yaml:"task_token_budget"`
	ChatMaxMessages int           `yaml:"chat_max_messages"`
	ApprovalTimeout time.Duration `yaml:"approval_timeout"`
}

type Web struct {
	Search string `yaml:"search"` // auto | tavily | brave | duckduckgo
}

type Telegram struct {
	TokenEnv     string  `yaml:"token_env"`
	AllowedUsers []int64 `yaml:"allowed_users"`
}

// MCPServer is an external tool server spoken to over stdio (e.g. cua-driver).
type MCPServer struct {
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
	// Tools limits which of the server's tools are exposed (glob); empty = all.
	Tools []string `yaml:"tools"`
}

type Browser struct {
	// CDP endpoint of a long-running Chromium started with --remote-debugging-port.
	CDP string `yaml:"cdp"`
}

type Config struct {
	Workspace string               `yaml:"workspace"`
	DataDir   string               `yaml:"data_dir"`
	SkillsDir string               `yaml:"skills_dir"`
	Distiller string               `yaml:"distiller"`
	Providers map[string]Provider  `yaml:"providers"`
	Models    map[string]Model     `yaml:"models"`
	Roles     map[string]Role      `yaml:"roles"`
	Limits    Limits               `yaml:"limits"`
	Web       Web                  `yaml:"web"`
	Telegram  Telegram             `yaml:"telegram"`
	MCP       map[string]MCPServer `yaml:"mcp_servers"`
	Browser   Browser              `yaml:"browser"`

	Dir  string `yaml:"-"` // directory of the config file; relative paths resolve against it
	File string `yaml:"-"` // absolute path of the config file
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := yaml.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	abs, _ := filepath.Abs(path)
	c.Dir, c.File = filepath.Dir(abs), abs
	c.defaults()
	return c, c.validate()
}

func (c *Config) defaults() {
	l := &c.Limits
	if l.MaxDepth == 0 {
		l.MaxDepth = 3
	}
	if l.MaxChildren == 0 {
		l.MaxChildren = 5
	}
	if l.MaxTurns == 0 {
		l.MaxTurns = 40
	}
	if l.WorkerTimeout == 0 {
		l.WorkerTimeout = 30 * time.Minute
	}
	if l.ChatMaxMessages == 0 {
		l.ChatMaxMessages = 60
	}
	if l.ApprovalTimeout == 0 {
		l.ApprovalTimeout = 10 * time.Minute
	}
	if c.Workspace == "" {
		c.Workspace = "workspace"
	}
	if c.DataDir == "" {
		c.DataDir = "data"
	}
	if c.SkillsDir == "" {
		c.SkillsDir = "skills"
	}
	if c.Web.Search == "" {
		c.Web.Search = "auto"
	}
	if c.Browser.CDP == "" {
		c.Browser.CDP = os.Getenv("AGENTD_BROWSER_CDP")
	}
	if command := os.Getenv("AGENTD_CUA_DRIVER"); command != "" {
		if _, exists := c.MCP["cua"]; !exists {
			if c.MCP == nil {
				c.MCP = make(map[string]MCPServer)
			}
			c.MCP["cua"] = MCPServer{Command: command, Args: []string{"mcp"}}
		}
	}
	for name, m := range c.Models {
		if m.MaxTokens == 0 {
			m.MaxTokens = 16000
			c.Models[name] = m
		}
	}
}

func (c *Config) validate() error {
	if _, ok := c.Roles["chat"]; !ok {
		return fmt.Errorf("config: role 'chat' is required")
	}
	for name, r := range c.Roles {
		if _, ok := c.Models[r.Model]; !ok {
			return fmt.Errorf("config: role %q uses unknown model %q", name, r.Model)
		}
	}
	for name, m := range c.Models {
		if _, ok := c.Providers[m.Provider]; !ok {
			return fmt.Errorf("config: model %q uses unknown provider %q", name, m.Provider)
		}
	}
	if c.Distiller != "" {
		if _, ok := c.Models[c.Distiller]; !ok {
			return fmt.Errorf("config: distiller model %q not defined", c.Distiller)
		}
	}
	return nil
}

// Path resolves p relative to the config directory ("~/" means the home directory).
func (c *Config) Path(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

// ReadEnvFile parses KEY=VALUE lines (optionally "export KEY=VALUE", quoted values).
func ReadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, sc.Err()
}

// DefaultPath picks the config file when -config isn't given: $AGENTD_HOME/config.yaml,
// else ~/.agentd/config.yaml, unless only ./config.yaml exists (running from a checkout).
func DefaultPath() string {
	home := os.Getenv("AGENTD_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".agentd")
		}
	}
	p := filepath.Join(home, "config.yaml")
	if _, err := os.Stat(p); err != nil {
		if _, err := os.Stat("config.yaml"); err == nil {
			return "config.yaml"
		}
	}
	return p
}

// LoadEnvFile loads an env file into the process environment without
// overriding variables that are already set.
func LoadEnvFile(path string) error {
	vars, err := ReadEnvFile(path)
	if err != nil {
		return err
	}
	for k, v := range vars {
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return nil
}

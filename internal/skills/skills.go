// Package skills loads SKILL.md folders. Only name+description go into prompts;
// the body is loaded on demand via use_skill (progressive disclosure).
package skills

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Skill struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Roles       []string `yaml:"roles"` // empty = every role
	Dir         string   `yaml:"-"`
	Body        string   `yaml:"-"`
}

type Set struct{ byName map[string]*Skill }

func Load(dir string) (*Set, error) {
	s := &Set{byName: map[string]*Skill{}}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, e.Name(), "SKILL.md")
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		sk, err := parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if sk.Name == "" {
			sk.Name = e.Name()
		}
		sk.Dir = filepath.Join(dir, e.Name())
		s.byName[sk.Name] = sk
	}
	return s, nil
}

func parse(raw []byte) (*Skill, error) {
	sk := &Skill{}
	if !bytes.HasPrefix(raw, []byte("---")) {
		sk.Body = string(raw)
		return sk, nil
	}
	rest := raw[3:]
	end := bytes.Index(rest, []byte("\n---"))
	if end < 0 {
		return nil, fmt.Errorf("unterminated frontmatter")
	}
	if err := yaml.Unmarshal(rest[:end], sk); err != nil {
		return nil, err
	}
	sk.Body = strings.TrimSpace(string(rest[end+4:]))
	return sk, nil
}

func (sk *Skill) For(role string) bool {
	if len(sk.Roles) == 0 {
		return true
	}
	for _, r := range sk.Roles {
		if r == role {
			return true
		}
	}
	return false
}

func (s *Set) ForRole(role string) []*Skill {
	var out []*Skill
	for _, sk := range s.byName {
		if sk.For(role) {
			out = append(out, sk)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Index is the level-1 listing placed in a role's system prompt.
func (s *Set) Index(role string) string {
	var b strings.Builder
	for _, sk := range s.ForRole(role) {
		fmt.Fprintf(&b, "- %s: %s\n", sk.Name, sk.Description)
	}
	return b.String()
}

// Use returns the full skill body plus the list of bundled files (level 2).
func (s *Set) Use(name, role string) (string, error) {
	sk, ok := s.byName[name]
	if !ok || !sk.For(role) {
		return "", fmt.Errorf("skill %q not available for role %s", name, role)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Skill: %s\nThư mục: %s\n\n%s\n", sk.Name, sk.Dir, sk.Body)
	var files []string
	filepath.WalkDir(sk.Dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() != "SKILL.md" {
			rel, _ := filepath.Rel(sk.Dir, p)
			files = append(files, rel)
		}
		return nil
	})
	if len(files) > 0 {
		b.WriteString("\nFile đi kèm (đọc bằng read_skill_file nếu cần):\n")
		for _, f := range files {
			b.WriteString("- " + f + "\n")
		}
	}
	return b.String(), nil
}

// ReadFile reads a file bundled with a skill (level 3).
func (s *Set) ReadFile(name, rel, role string) (string, error) {
	sk, ok := s.byName[name]
	if !ok || !sk.For(role) {
		return "", fmt.Errorf("skill %q not available", name)
	}
	p := filepath.Join(sk.Dir, filepath.Clean("/"+rel))
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

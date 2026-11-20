// Package gateway connects messaging surfaces (terminal, Telegram) to the runtime.
package gateway

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"agentd/internal/llm"
)

// Handler is what gateways call with user input (Runtime.HandleUser).
// attachments are blocks the model reads directly (images, PDFs).
type Handler func(conv, text string, attachments []llm.Block)

type CLI struct{ mu sync.Mutex }

func (c *CLI) Send(conv, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Printf("\n\033[36m🤖 %s\033[0m\n> ", strings.TrimSpace(text))
}

// SendFile can't show a file in a terminal; it prints where the file is.
func (c *CLI) SendFile(conv, path, caption string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Printf("\n\033[36m📎 %s\033[0m %s\n> ", path, caption)
	return nil
}

// Run reads lines from stdin until EOF or ctx is cancelled.
func (c *CLI) Run(ctx context.Context, h Handler) {
	fmt.Print("Gõ tin nhắn (/help để xem lệnh, Ctrl+D để thoát)\n> ")
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	lines := make(chan string)
	go func() {
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case l, ok := <-lines:
			if !ok {
				return
			}
			if strings.TrimSpace(l) == "" {
				fmt.Print("> ")
				continue
			}
			h("cli", l, nil)
		}
	}
}

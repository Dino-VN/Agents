// agentd: a chat agent that delegates to worker agents, with memory and skills.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"agentd/internal/agent"
	"agentd/internal/config"
	"agentd/internal/gateway"
	"agentd/internal/llm"
	"agentd/internal/setup"
	"agentd/internal/skills"
	"agentd/internal/store"
)

type multiGW struct{ cli, tg agent.Gateway }

func (m multiGW) Send(conv, text string) {
	if len(conv) > 3 && conv[:3] == "tg:" {
		if m.tg != nil {
			m.tg.Send(conv, text)
		}
		return
	}
	m.cli.Send(conv, text)
}

func (m multiGW) SendFile(conv, path, caption string) error {
	if len(conv) > 3 && conv[:3] == "tg:" {
		if m.tg == nil {
			return fmt.Errorf("telegram gateway is not running")
		}
		return m.tg.SendFile(conv, path, caption)
	}
	return m.cli.SendFile(conv, path, caption)
}

func main() {
	cfgPath := flag.String("config", config.DefaultPath(), "config file (default ~/.agentd/config.yaml)")
	useTG := flag.Bool("telegram", false, "run the Telegram gateway (no terminal input)")
	flag.Usage = func() {
		os.Stderr.WriteString("usage: agentd [-telegram] | agentd setup   (home: ~/.agentd, or $AGENTD_HOME)\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.Arg(0) == "setup" {
		if err := setup.Run(*cfgPath); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("%v\n(chạy \"agentd setup\" để tạo cấu hình)", err)
	}
	// Secrets written by setup; real environment variables take precedence.
	config.LoadEnvFile(cfg.Path("secrets.env"))
	for _, d := range []string{cfg.Workspace, cfg.DataDir} {
		os.MkdirAll(cfg.Path(d), 0o755)
	}
	logf, err := os.OpenFile(filepath.Join(cfg.Path(cfg.DataDir), "agentd.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		log.SetOutput(logf) // keep the terminal for the conversation
	}

	st, err := store.Open(filepath.Join(cfg.Path(cfg.DataDir), "agentd.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	sk, err := skills.Load(cfg.Path(cfg.SkillsDir))
	if err != nil {
		log.Fatal(err)
	}
	router, err := llm.NewRouter(cfg)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cli := &gateway.CLI{}
	gw := multiGW{cli: cli}
	var tg *gateway.Telegram
	if *useTG {
		token := os.Getenv(cfg.Telegram.TokenEnv)
		if token == "" {
			log.SetOutput(os.Stderr)
			log.Fatalf("telegram: chưa có bot token (%s). Chạy \"agentd setup\".", cfg.Telegram.TokenEnv)
		}
		tg = gateway.NewTelegram(token, cfg.Telegram.AllowedUsers, cfg.Path(cfg.Workspace))
		gw.tg = tg
	}

	rt := agent.New(ctx, cfg, router, st, sk, gw)
	if tg != nil {
		os.Stderr.WriteString("agentd: Telegram gateway running (Ctrl+C to stop)\n")
		tg.Run(ctx, rt.HandleUser)
	} else {
		cli.Run(ctx, rt.HandleUser)
	}

	// Distill open conversations into memory before exiting.
	dctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rt.DistillAll(dctx)
}

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/version"
)

func run(ctx context.Context, args []string) error {
	cfg, err := loadConfig(args)
	if err != nil {
		return err
	}
	a, err := newApp(ctx, cfg)
	if err != nil {
		return err
	}
	a.log.Info("starting", "version", version.Version, "addr", cfg.HTTPAddr)

	errCh := make(chan error, 1)
	go func() { errCh <- a.run() }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	select {
	case err := <-errCh:
		return err
	case <-sig:
	case <-ctx.Done():
	}
	a.log.Info("shutdown")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return a.shutdown(sctx)
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

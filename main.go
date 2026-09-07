package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	config, err := loadConfig(os.Getenv)
	if err != nil {
		logger.Error("configuration rejected", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.InfoContext(ctx, "vk2tg bootstrap started", "dry_run", config.DryRun, "network_enabled", false)
	<-ctx.Done()
	logger.Info("vk2tg shutdown complete")
}

package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
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
	logger.InfoContext(ctx, "vk2tg relay started", "dry_run", config.DryRun)
	if err := runRelay(ctx, config, logger); err != nil && !errors.Is(err, context.Canceled) {
		stop()
		logger.Error("relay stopped", "error", err)
		os.Exit(1)
	}
	logger.Info("vk2tg shutdown complete")
}

func runRelay(ctx context.Context, config Config, logger *slog.Logger) error {
	vk, err := NewVKClient(config, "", 35*time.Second)
	if err != nil {
		return err
	}
	telegram, err := NewTelegramClient(config, "", 120*time.Second)
	if err != nil {
		return err
	}
	relay := Relay{
		config: config, vk: vk, telegram: telegram, logger: logger,
		mediaHTTP: &http.Client{Timeout: 60 * time.Second},
	}
	return relay.Run(ctx)
}

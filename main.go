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
	historyCount, err := parseHistoryCommand(os.Args[1:])
	if err != nil {
		logger.Error("command rejected", "error", err)
		os.Exit(1)
	}
	config, err := loadConfig(os.Getenv)
	if err != nil {
		logger.Error("configuration rejected", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.InfoContext(ctx, "vk2tg started", "dry_run", config.DryRun, "history_count", historyCount)
	if err := runRelay(ctx, config, logger, historyCount); err != nil && !errors.Is(err, context.Canceled) {
		stop()
		logger.Error("relay stopped", "error", err)
		os.Exit(1)
	}
	logger.Info("vk2tg shutdown complete")
}

func runRelay(ctx context.Context, config Config, logger *slog.Logger, historyCount int) (result error) {
	vk, err := NewVKClient(config, "", 35*time.Second)
	if err != nil {
		return err
	}
	telegram, err := NewTelegramClient(config, "", 120*time.Second)
	if err != nil {
		return err
	}
	store, err := OpenMessageStore(ctx, config.StateDBPath)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, store.Close()) }()
	logger.Info("state database opened")
	if err := vk.ConfigureRateProtection(ctx, store, logger); err != nil {
		return err
	}
	relay := Relay{
		config: config, vk: vk, telegram: telegram, logger: logger, store: store,
		mediaHTTP: &http.Client{Timeout: 60 * time.Second},
	}
	if historyCount > 0 {
		telegram.logger = logger
		return relay.ReplayHistory(ctx, historyCount)
	}
	return relay.Run(ctx)
}

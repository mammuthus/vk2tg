package main

import (
	"fmt"
	"strconv"
	"strings"
)

type Config struct {
	VKAccessToken        string
	VKTargetPeerID       int64
	TelegramBotToken     string
	TelegramTargetChatID int64
	VKBlockedSenderIDs   map[int64]struct{}
	DryRun               bool
}

func loadConfig(getenv func(string) string) (Config, error) {
	config := Config{
		VKAccessToken:      strings.TrimSpace(getenv("VK_ACCESS_TOKEN")),
		TelegramBotToken:   strings.TrimSpace(getenv("TELEGRAM_BOT_TOKEN")),
		VKBlockedSenderIDs: make(map[int64]struct{}),
		DryRun:             true,
	}
	if config.VKAccessToken == "" {
		return Config{}, fmt.Errorf("VK_ACCESS_TOKEN is required")
	}
	if config.TelegramBotToken == "" {
		return Config{}, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}
	var err error
	config.VKTargetPeerID, err = parseID("VK_TARGET_PEER_ID", getenv("VK_TARGET_PEER_ID"))
	if err != nil {
		return Config{}, err
	}
	config.TelegramTargetChatID, err = parseID("TELEGRAM_TARGET_CHAT_ID", getenv("TELEGRAM_TARGET_CHAT_ID"))
	if err != nil {
		return Config{}, err
	}
	if blocked := strings.TrimSpace(getenv("VK_BLOCKED_SENDER_IDS")); blocked != "" {
		for _, value := range strings.Split(blocked, ",") {
			senderID, err := parseID("VK_BLOCKED_SENDER_IDS", value)
			if err != nil {
				return Config{}, err
			}
			config.VKBlockedSenderIDs[senderID] = struct{}{}
		}
	}
	if value := strings.TrimSpace(getenv("DRY_RUN")); value != "" {
		config.DryRun, err = strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("DRY_RUN must be a boolean")
		}
	}
	return config, nil
}

func parseID(key, value string) (int64, error) {
	identifier, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || identifier == 0 {
		return 0, fmt.Errorf("%s must be a nonzero signed 64-bit decimal integer", key)
	}
	return identifier, nil
}

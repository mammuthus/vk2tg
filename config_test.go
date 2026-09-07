package main

import (
	"strings"
	"testing"
)

func TestLoadConfigValid(t *testing.T) {
	testingEnvironment := map[string]string{
		"VK_ACCESS_TOKEN":         "fake-vk-token",
		"VK_TARGET_PEER_ID":       "2000000123",
		"TELEGRAM_BOT_TOKEN":      "fake-telegram-token",
		"TELEGRAM_TARGET_CHAT_ID": "-1001234567890",
		"VK_BLOCKED_SENDER_IDS":   "42, 73,42,-12",
		"DRY_RUN":                 "false",
		"STATE_DB_PATH":           "custom.sqlite",
	}
	config, err := loadConfig(func(key string) string { return testingEnvironment[key] })
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if config.VKAccessToken != "fake-vk-token" || config.TelegramBotToken != "fake-telegram-token" {
		t.Error("tokens were not loaded")
	}
	if config.VKTargetPeerID != 2000000123 || config.TelegramTargetChatID != -1001234567890 {
		t.Error("numeric IDs were not loaded correctly")
	}
	if config.DryRun {
		t.Error("explicit false did not disable dry run")
	}
	if config.StateDBPath != "custom.sqlite" {
		t.Error("state database path not loaded")
	}
	if len(config.VKBlockedSenderIDs) != 3 {
		t.Fatal("blocklist must deduplicate IDs")
	}
	for _, senderID := range []int64{42, 73, -12} {
		if _, exists := config.VKBlockedSenderIDs[senderID]; !exists {
			t.Error("expected blocked sender missing")
		}
	}
}

func TestLoadConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr bool
	}{
		{name: "missing VK token", key: "VK_ACCESS_TOKEN", wantErr: true},
		{name: "blank VK token", key: "VK_ACCESS_TOKEN", value: "  ", wantErr: true},
		{name: "missing Telegram token", key: "TELEGRAM_BOT_TOKEN", wantErr: true},
		{name: "missing peer ID", key: "VK_TARGET_PEER_ID", wantErr: true},
		{name: "invalid peer ID", key: "VK_TARGET_PEER_ID", value: "private-invalid-input", wantErr: true},
		{name: "overflow peer ID", key: "VK_TARGET_PEER_ID", value: "9223372036854775808", wantErr: true},
		{name: "zero peer ID", key: "VK_TARGET_PEER_ID", value: "0", wantErr: true},
		{name: "negative peer ID", key: "VK_TARGET_PEER_ID", value: "-123"},
		{name: "missing Telegram chat ID", key: "TELEGRAM_TARGET_CHAT_ID", wantErr: true},
		{name: "invalid Telegram chat ID", key: "TELEGRAM_TARGET_CHAT_ID", value: "private-invalid-input", wantErr: true},
		{name: "overflow Telegram chat ID", key: "TELEGRAM_TARGET_CHAT_ID", value: "-9223372036854775809", wantErr: true},
		{name: "zero Telegram chat ID", key: "TELEGRAM_TARGET_CHAT_ID", value: "0", wantErr: true},
		{name: "positive Telegram chat ID", key: "TELEGRAM_TARGET_CHAT_ID", value: "123"},
		{name: "invalid blocked sender", key: "VK_BLOCKED_SENDER_IDS", value: "private-invalid-input", wantErr: true},
		{name: "empty blocked sender entry", key: "VK_BLOCKED_SENDER_IDS", value: "42,,73", wantErr: true},
		{name: "trailing blocklist comma", key: "VK_BLOCKED_SENDER_IDS", value: "42,", wantErr: true},
		{name: "zero blocked sender", key: "VK_BLOCKED_SENDER_IDS", value: "0", wantErr: true},
		{name: "invalid dry run", key: "DRY_RUN", value: "private-invalid-input", wantErr: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			environment := map[string]string{
				"VK_ACCESS_TOKEN":         "fake-vk-token",
				"VK_TARGET_PEER_ID":       "2000000123",
				"TELEGRAM_BOT_TOKEN":      "fake-telegram-token",
				"TELEGRAM_TARGET_CHAT_ID": "-1001234567890",
			}
			environment[testCase.key] = testCase.value
			_, err := loadConfig(func(key string) string { return environment[key] })
			if (err != nil) != testCase.wantErr {
				t.Fatalf("error presence = %v, want %v", err != nil, testCase.wantErr)
			}
			if err == nil {
				return
			}
			if !strings.Contains(err.Error(), testCase.key) {
				t.Error("error must identify the configuration key")
			}
			for _, privateValue := range []string{"fake-vk-token", "fake-telegram-token", "private-invalid-input"} {
				if strings.Contains(err.Error(), privateValue) {
					t.Error("configuration error leaked a value")
				}
			}
		})
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	tests := []struct {
		name       string
		dryRun     string
		blocklist  string
		wantDryRun bool
	}{
		{name: "default DRY_RUN and empty blocklist", wantDryRun: true},
		{name: "explicit DRY_RUN true", dryRun: "true", wantDryRun: true},
		{name: "explicit DRY_RUN false", dryRun: "false"},
		{name: "blank blocklist", blocklist: "  ", wantDryRun: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			environment := map[string]string{
				"VK_ACCESS_TOKEN":         "fake-vk-token",
				"VK_TARGET_PEER_ID":       "2000000123",
				"TELEGRAM_BOT_TOKEN":      "fake-telegram-token",
				"TELEGRAM_TARGET_CHAT_ID": "-1001234567890",
				"VK_BLOCKED_SENDER_IDS":   testCase.blocklist,
				"DRY_RUN":                 testCase.dryRun,
			}
			config, err := loadConfig(func(key string) string { return environment[key] })
			if err != nil {
				t.Fatalf("valid config rejected: %v", err)
			}
			if config.DryRun != testCase.wantDryRun {
				t.Errorf("dry run = %v, want %v", config.DryRun, testCase.wantDryRun)
			}
			if config.StateDBPath != "state/vk2tg.sqlite" {
				t.Error("unexpected default state path")
			}
			if config.VKBlockedSenderIDs == nil || len(config.VKBlockedSenderIDs) != 0 {
				t.Error("empty blocklist must produce an empty set")
			}
		})
	}
}

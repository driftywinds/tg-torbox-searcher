package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"torbox-tg-bot/internal/bot"
	"torbox-tg-bot/internal/config"
	"torbox-tg-bot/internal/search"
	"torbox-tg-bot/internal/store"
	"torbox-tg-bot/internal/torbox"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	api, err := tgbotapi.NewBotAPI(cfg.TelegramBotToken)
	if err != nil {
		log.Fatalf("failed to create telegram bot: %v", err)
	}
	log.Printf("authorized as @%s", api.Self.UserName)

	userStore, err := store.NewUserStore(cfg.DataDir, cfg.AdminTelegramID)
	if err != nil {
		log.Fatalf("failed to load user store: %v", err)
	}

	tbClient := torbox.NewClient(cfg.TorBoxAPIKey, cfg.GofileAPIKey, cfg.TorBoxAPIBaseURL, cfg.TorBoxSearchBaseURL)

	// Wire up the search backend.
	switch cfg.SearchBackend {
	case "torznab":
		if cfg.TorznabURL == "" {
			log.Fatalf("SEARCH_BACKEND=torznab requires TORZNAB_URL to be set")
		}
		z := search.NewTorznabSearcher(cfg.TorznabURL, cfg.TorznabAPIKey)
		tbClient.SetSearcher(z)
		log.Printf("search backend: torznab (%s)", cfg.TorznabURL)
	case "", "torbox":
		log.Printf("search backend: torbox (may be restricted to whitelisted IPs)")
	default:
		log.Fatalf("unknown SEARCH_BACKEND %q (expected 'torbox' or 'torznab')", cfg.SearchBackend)
	}

	// GoFile API key is now passed to the torbox client for use in the
	// TorBox integration API call (as gofile_token). It's the user's
	// GoFile Account API Token from https://gofile.io/myProfile.

	b := bot.New(api, tbClient, userStore)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Println("bot is running, press Ctrl+C to stop")
	if err := b.Run(ctx); err != nil && err != context.Canceled {
		log.Printf("bot stopped: %v", err)
	}
}

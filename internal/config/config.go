package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config holds all runtime configuration loaded from environment variables / .env file.
type Config struct {
	TelegramBotToken string
	TorBoxAPIKey     string
	GofileAPIKey     string
	AdminTelegramID  int64
	DataDir          string

	// TorBoxAPIBaseURL and TorBoxSearchBaseURL are configurable in case
	// TorBox changes their endpoints.
	TorBoxAPIBaseURL    string
	TorBoxSearchBaseURL string

	// SearchBackend selects the search backend: "torbox" (default, uses
	// TorBox's own search API which may be IP-restricted) or "torznab"
	// (queries a Torznab-compatible indexer such as Jackett or Prowlarr).
	SearchBackend string

	// TorznabURL is the base URL of a Torznab-compatible indexer
	// (required when SearchBackend is "torznab").
	// For Jackett: http://localhost:9117
	// For Prowlarr: http://localhost:9696
	TorznabURL string

	// TorznabAPIKey is the API key required by the Torznab indexer.
	// Jackett and Prowlarr both require one.
	TorznabAPIKey string
}

// Load reads a .env file if present (ignored if missing) and then reads
// environment variables, validating that everything required is present.
func Load() (*Config, error) {
	// It's fine if there's no .env file (e.g. running in a container with
	// real env vars already set).
	_ = godotenv.Load()

	cfg := &Config{
		TelegramBotToken:    strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		TorBoxAPIKey:        strings.TrimSpace(os.Getenv("TORBOX_API_KEY")),
		GofileAPIKey:        strings.TrimSpace(os.Getenv("GOFILE_API_KEY")),
		DataDir:             strings.TrimSpace(os.Getenv("DATA_DIR")),
		TorBoxAPIBaseURL:    strings.TrimSpace(os.Getenv("TORBOX_API_BASE_URL")),
		TorBoxSearchBaseURL: strings.TrimSpace(os.Getenv("TORBOX_SEARCH_BASE_URL")),
	}

	if cfg.DataDir == "" {
		cfg.DataDir = "./data"
	}
	if cfg.TorBoxAPIBaseURL == "" {
		cfg.TorBoxAPIBaseURL = "https://api.torbox.app/v1/api"
	}
	if cfg.TorBoxSearchBaseURL == "" {
		cfg.TorBoxSearchBaseURL = "https://search-api.torbox.app"
	}
	if cfg.SearchBackend == "" {
		cfg.SearchBackend = strings.ToLower(strings.TrimSpace(os.Getenv("SEARCH_BACKEND")))
	}
	if cfg.TorznabURL == "" {
		cfg.TorznabURL = strings.TrimSpace(os.Getenv("TORZNAB_URL"))
	}
	if cfg.TorznabAPIKey == "" {
		cfg.TorznabAPIKey = strings.TrimSpace(os.Getenv("TORZNAB_API_KEY"))
	}

	adminIDStr := strings.TrimSpace(os.Getenv("ADMIN_TELEGRAM_ID"))

	var missing []string
	if cfg.TelegramBotToken == "" {
		missing = append(missing, "TELEGRAM_BOT_TOKEN")
	}
	if cfg.TorBoxAPIKey == "" {
		missing = append(missing, "TORBOX_API_KEY")
	}
	if cfg.GofileAPIKey == "" {
		missing = append(missing, "GOFILE_API_KEY")
	}
	if adminIDStr == "" {
		missing = append(missing, "ADMIN_TELEGRAM_ID")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variable(s): %s", strings.Join(missing, ", "))
	}

	adminID, err := strconv.ParseInt(adminIDStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("ADMIN_TELEGRAM_ID must be a valid Telegram numeric user id: %w", err)
	}
	cfg.AdminTelegramID = adminID

	return cfg, nil
}

package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds application configuration loaded from environment variables.
type Config struct {
	Port               string
	GeminiAPIKey       string
	GeminiModel        string
	PromptsDir         string
	ThreadsUserID      string
	ThreadsAccessToken string
	PostLanguage       string
	NewsCount          int
	APISecret          string
	DryRun             bool
}

// Load reads configuration from environment variables with sensible defaults.
func Load() Config {
	newsCount := 3
	if v := strings.TrimSpace(os.Getenv("NEWS_COUNT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 10 {
			newsCount = n
		}
	}

	dryRun := false
	if v := strings.TrimSpace(os.Getenv("DRY_RUN")); v != "" {
		dryRun = strings.EqualFold(v, "true") || v == "1"
	}

	return Config{
		Port:               getEnvOrDefault("PORT", "8080"),
		GeminiAPIKey:       strings.TrimSpace(os.Getenv("GEMINI_API_KEY")),
		GeminiModel:        getEnvOrDefault("GEMINI_MODEL", "gemini-2.5-flash"),
		PromptsDir:         getEnvOrDefault("PROMPTS_DIR", "prompts"),
		ThreadsUserID:      strings.TrimSpace(os.Getenv("THREADS_USER_ID")),
		ThreadsAccessToken: strings.TrimSpace(os.Getenv("THREADS_ACCESS_TOKEN")),
		PostLanguage:       getEnvOrDefault("POST_LANGUAGE", "ja"),
		NewsCount:          newsCount,
		APISecret:          strings.TrimSpace(os.Getenv("API_SECRET")),
		DryRun:             dryRun,
	}
}

func getEnvOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

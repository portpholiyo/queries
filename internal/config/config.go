// Package config loads runtime configuration from environment variables and
// secret files. Secrets are only read from files, never from plain variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Config holds all runtime settings.
type Config struct {
	ListenAddr string

	DBHost string
	DBPort string
	DBName string
	DBUser string

	// Secrets (already read from their files).
	DBPassword    string
	IPSalt        string
	SlackBotToken string
	SlackAppToken string
	CaptchaSecret string

	SlackChannelID string // channel where queries are posted
	SlackOwnerID   string // the only Slack user whose thread replies count

	AllowedOrigins []string
	ClientIPHeader string

	DailyCap        int
	MaxMessageChars int
	MaxImages       int
	MaxImageBytes   int
}

var (
	channelRe = regexp.MustCompile(`^[CG][A-Z0-9]{8,}$`)
	userRe    = regexp.MustCompile(`^[UW][A-Z0-9]{8,}$`)
)

// Load reads configuration from the environment.
func Load() (*Config, error) {
	c := &Config{
		ListenAddr:      env("LISTEN_ADDR", ":8080"),
		DBHost:          env("DB_HOST", "postgres"),
		DBPort:          env("DB_PORT", "5432"),
		DBName:          env("DB_NAME", "chatbot"),
		DBUser:          env("DB_USER", "chatbot"),
		SlackChannelID:  os.Getenv("SLACK_CHANNEL_ID"),
		SlackOwnerID:    os.Getenv("SLACK_OWNER_ID"),
		AllowedOrigins:  splitCSV(env("ALLOWED_ORIGINS", "https://example.com,https://www.example.com")),
		ClientIPHeader:  os.Getenv("CLIENT_IP_HEADER"),
		DailyCap:        envInt("DAILY_CAP", 100),
		MaxMessageChars: envInt("MAX_MESSAGE_CHARS", 2000),
		MaxImages:       envInt("MAX_IMAGES", 3),
		MaxImageBytes:   envInt("MAX_IMAGE_BYTES", 5<<20),
	}

	var errs []error
	read := func(dst *string, key string) {
		v, err := secret(key)
		if err != nil {
			errs = append(errs, err)
		}
		*dst = v
	}
	read(&c.DBPassword, "DB_PASSWORD_FILE")
	read(&c.IPSalt, "IP_SALT_FILE")
	read(&c.SlackBotToken, "SLACK_BOT_TOKEN_FILE")
	read(&c.SlackAppToken, "SLACK_APP_TOKEN_FILE")
	read(&c.CaptchaSecret, "CAPTCHA_SECRET_FILE")
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	if !strings.HasPrefix(c.SlackBotToken, "xoxb-") {
		errs = append(errs, errors.New("slack bot token must start with xoxb-"))
	}
	if !strings.HasPrefix(c.SlackAppToken, "xapp-") {
		errs = append(errs, errors.New("slack app token must start with xapp-"))
	}
	if !channelRe.MatchString(c.SlackChannelID) {
		errs = append(errs, errors.New("SLACK_CHANNEL_ID is missing or malformed"))
	}
	if !userRe.MatchString(c.SlackOwnerID) {
		errs = append(errs, errors.New("SLACK_OWNER_ID is missing or malformed"))
	}
	if len(c.IPSalt) < 16 {
		errs = append(errs, errors.New("ip salt must be at least 16 characters"))
	}
	if c.DailyCap < 1 || c.MaxMessageChars < 1 || c.MaxImages < 0 || c.MaxImageBytes < 1 {
		errs = append(errs, errors.New("numeric settings out of range"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return -1
	}
	return n
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func secret(envKey string) (string, error) {
	path := os.Getenv(envKey)
	if path == "" {
		return "", fmt.Errorf("%s is not set", envKey)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", envKey, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("%s: file is empty", envKey)
	}
	return v, nil
}

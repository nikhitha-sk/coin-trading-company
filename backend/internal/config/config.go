// Package config loads runtime settings from environment variables.
package config

import (
	"errors"
	"os"
)

// Config holds all runtime settings for the API server.
type Config struct {
	AppEnv        string // "dev" or "prod"
	HTTPAddr      string
	DatabaseURL   string
	RedisURL      string
	SessionSecret string
}

const devSessionSecret = "dev-only-secret-change-me"

// Load reads configuration from the environment, applying local-dev defaults.
func Load() (Config, error) {
	c := Config{
		AppEnv:        getenv("APP_ENV", "dev"),
		HTTPAddr:      getenv("HTTP_ADDR", ":8081"),
		DatabaseURL:   getenv("DATABASE_URL", "postgres://coin:coin@localhost:55432/coin?sslmode=disable"),
		RedisURL:      getenv("REDIS_URL", "redis://localhost:56379/0"),
		SessionSecret: getenv("SESSION_SECRET", devSessionSecret),
	}
	if c.AppEnv != "dev" && c.AppEnv != "prod" {
		return c, errors.New("APP_ENV must be dev or prod")
	}
	if c.IsProd() && (c.SessionSecret == devSessionSecret || len(c.SessionSecret) < 32) {
		return c, errors.New("SESSION_SECRET must be set to a random string of at least 32 characters in prod")
	}
	return c, nil
}

// IsDev reports whether development-only features (like /dev/login) are enabled.
func (c Config) IsDev() bool { return c.AppEnv == "dev" }

// IsProd reports whether the server runs in production mode.
func (c Config) IsProd() bool { return c.AppEnv == "prod" }

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

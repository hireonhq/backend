// Package config loads and validates environment configuration.
//
// Validation is fail-fast: the old backend read process.env raw and happily
// booted without SUPABASE_URL, only logging a warning and failing later at
// request time. A service that cannot do its job should not start.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port         string
	NodeEnv      string
	DatabaseURL  string
	SupabaseURL  string
	MLServiceURL string
	MLServiceKey string
	FrontendURL  string

	// Object storage (S3-compatible; Cloudflare R2 in production).
	S3Endpoint        string
	S3Region          string
	S3Bucket          string
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3PublicBaseURL   string

	MLLatencyBudget time.Duration
}

func (c Config) IsProduction() bool { return c.NodeEnv == "production" }

// Load reads the environment, applies defaults, and returns an error listing
// every missing required variable at once rather than one per restart.
func Load() (Config, error) {
	c := Config{
		Port:              getenv("PORT", "3000"),
		NodeEnv:           getenv("NODE_ENV", "development"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		SupabaseURL:       strings.TrimRight(os.Getenv("SUPABASE_URL"), "/"),
		MLServiceURL:      strings.TrimRight(os.Getenv("ML_SERVICE_URL"), "/"),
		MLServiceKey:      os.Getenv("ML_SERVICE_API_KEY"),
		FrontendURL:       os.Getenv("FRONTEND_URL"),
		S3Endpoint:        os.Getenv("S3_ENDPOINT"),
		S3Region:          getenv("S3_REGION", "auto"),
		S3Bucket:          os.Getenv("S3_BUCKET"),
		S3AccessKeyID:     os.Getenv("S3_ACCESS_KEY_ID"),
		S3SecretAccessKey: os.Getenv("S3_SECRET_ACCESS_KEY"),
		S3PublicBaseURL:   strings.TrimRight(os.Getenv("S3_PUBLIC_BASE_URL"), "/"),
		MLLatencyBudget:   30 * time.Second,
	}
	if v := os.Getenv("ML_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.MLLatencyBudget = time.Duration(n) * time.Second
		}
	}

	var missing []string
	require := func(name, value string) {
		if value == "" {
			missing = append(missing, name)
		}
	}
	require("DATABASE_URL", c.DatabaseURL)
	require("SUPABASE_URL", c.SupabaseURL)
	require("ML_SERVICE_URL", c.MLServiceURL)
	require("ML_SERVICE_API_KEY", c.MLServiceKey)
	require("S3_BUCKET", c.S3Bucket)
	require("S3_ACCESS_KEY_ID", c.S3AccessKeyID)
	require("S3_SECRET_ACCESS_KEY", c.S3SecretAccessKey)

	if len(missing) > 0 {
		return c, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

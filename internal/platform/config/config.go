// Package config loads server configuration from the environment (and a .env file in development).
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const placeholder = "CHANGE_ME"

type Config struct {
	Env        string // "development" or "production"
	HTTPAddr   string
	LogLevel   string
	TrustProxy bool
	// MaintenanceMode makes every /v1 route answer 503 maintenance. MinClientVersion refuses older app builds with 426.
	MaintenanceMode bool
	// AccountDeletionGrace is how long after a deletion request the account is erased for good (default 30 days).
	AccountDeletionGrace time.Duration
	MinClientVersion     string
	DatabaseURL          string
	DBMaxConns           int32
	DBUsePooler          bool

	TokenPepper    []byte
	AccessTokenTTL time.Duration
	RequestTimeout time.Duration

	R2 R2 // used from Phase 4; loaded now so configuration is in one place
}

type R2 struct {
	AccountID, AccessKeyID, SecretAccessKey, Bucket, Endpoint string
}

func (r R2) Configured() bool {
	return r.AccountID != "" && !isPlaceholder(r.AccountID) && !isPlaceholder(r.AccessKeyID) &&
		!isPlaceholder(r.SecretAccessKey) && !isPlaceholder(r.Bucket)
}

func (c Config) IsProduction() bool { return c.Env == "production" }

func isPlaceholder(v string) bool { return v == "" || strings.Contains(v, placeholder) }

// Load reads configuration. Existing environment variables win over .env values.
func Load() (Config, error) {
	LoadDotEnv()
	c := Config{
		Env:                  str("APP_ENV", "development"),
		HTTPAddr:             str("HTTP_ADDR", ":8080"),
		LogLevel:             str("LOG_LEVEL", "info"),
		TrustProxy:           boolean("TRUST_PROXY", false),
		MaintenanceMode:      boolean("MAINTENANCE_MODE", false),
		AccountDeletionGrace: duration("ACCOUNT_DELETION_GRACE", 30*24*time.Hour),
		MinClientVersion:     os.Getenv("MIN_CLIENT_VERSION"),
		DatabaseURL:          str("DATABASE_URL", ""),
		DBMaxConns:           int32(integer("DB_MAX_CONNS", 10)),
		DBUsePooler:          boolean("DB_USE_POOLER", true),
		TokenPepper:          []byte(str("TOKEN_PEPPER", "")),
		AccessTokenTTL:       duration("ACCESS_TOKEN_TTL", 15*time.Minute),
		RequestTimeout:       duration("REQUEST_TIMEOUT", 15*time.Second),
		R2: R2{
			AccountID: str("R2_ACCOUNT_ID", ""), AccessKeyID: str("R2_ACCESS_KEY_ID", ""),
			SecretAccessKey: str("R2_SECRET_ACCESS_KEY", ""), Bucket: str("R2_BUCKET", ""),
			Endpoint: str("R2_ENDPOINT", ""),
		},
	}
	return c, c.Validate()
}

// Validate checks values the server cannot run safely without. Error messages never include secret values.
func (c Config) Validate() error {
	var errs []error
	if c.Env != "development" && c.Env != "production" {
		errs = append(errs, fmt.Errorf("APP_ENV must be development or production, got %q", c.Env))
	}
	if isPlaceholder(c.DatabaseURL) {
		errs = append(errs, errors.New("DATABASE_URL is missing or still a CHANGE_ME placeholder (set your Neon pooled connection string in .env)"))
	}
	if len(c.TokenPepper) < 32 || isPlaceholder(string(c.TokenPepper)) {
		errs = append(errs, errors.New("TOKEN_PEPPER must be at least 32 characters (generate with: openssl rand -hex 32)"))
	}
	if c.DBMaxConns < 1 {
		errs = append(errs, errors.New("DB_MAX_CONNS must be at least 1"))
	}
	if c.AccessTokenTTL <= 0 {
		errs = append(errs, errors.New("ACCESS_TOKEN_TTL must be positive"))
	}
	return errors.Join(errs...)
}

// LoadDotEnv loads the nearest .env file, searching from the working directory upwards.
func LoadDotEnv() {
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	for i := 0; i < 4; i++ {
		if f, err := os.Open(filepath.Join(dir, ".env")); err == nil {
			defer f.Close()
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if k, v, ok := parseLine(sc.Text()); ok {
					if _, set := os.LookupEnv(k); !set {
						_ = os.Setenv(k, v)
					}
				}
			}
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

func parseLine(line string) (key, val string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	k, v, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		v = v[1 : len(v)-1]
	}
	return strings.TrimSpace(k), v, true
}

func str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
func integer(key string, def int) int {
	if v, err := strconv.Atoi(str(key, "")); err == nil {
		return v
	}
	return def
}
func boolean(key string, def bool) bool {
	if v, err := strconv.ParseBool(str(key, "")); err == nil {
		return v
	}
	return def
}
func duration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(str(key, "")); err == nil {
		return v
	}
	return def
}

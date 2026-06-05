// Package config loads and validates opensearch-cli runtime configuration.
// Empty optional environment variables are treated as unset, and invalid
// values return errors instead of silently falling back.
package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Defaults and bounds.
const (
	DefaultOutputDir     = ".opensearch"
	DefaultScrapeWorkers = 4
	MinScrapeWorkers     = 1
	MaxScrapeWorkers     = 16
	userAgentPrefix      = "opensearch-cli/"
)

// Environment variable names.
const (
	EnvExaAPIKey         = "EXA_API_KEY"
	EnvOutputDir         = "OPENSEARCH_OUTPUT_DIR"
	EnvUserAgent         = "OPENSEARCH_USER_AGENT"
	EnvScrapeConcurrency = "OPENSEARCH_SCRAPE_CONCURRENCY"
)

// ErrInvalidConfig means a config value is unparsable or out of range.
var ErrInvalidConfig = errors.New("invalid config")

func invalid(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...))
}

// Config is the parsed runtime config snapshot. ExaAPIKey only comes from the
// environment and must never appear in stdout, stderr, result files, or errors.
type Config struct {
	ExaAPIKey     string
	OutputDir     string
	UserAgent     string
	ScrapeWorkers int
	exaAPIKeySet  bool
}

// Getenv abstracts environment lookup for tests.
type Getenv func(key string) string

// Load is a back-compat / test entry point that loads the full scrape config.
// Production code calls LoadSearch or LoadScrape directly per subcommand.
func Load(getenv Getenv, version string) (*Config, error) {
	return LoadScrape(getenv, version)
}

// LoadSearch loads only the config needed by the search subcommand. Scrape-only
// config is intentionally ignored so unrelated env values do not break search.
func LoadSearch(getenv Getenv, version string) (*Config, error) {
	c := baseConfig(version)
	loadShared(getenv, c)
	return c, nil
}

// LoadScrape loads the full scrape config. OPENSEARCH_SCRAPE_CONCURRENCY is
// always validated; CLI --concurrency only overrides it after the env value is
// known to be valid.
func LoadScrape(getenv Getenv, version string) (*Config, error) {
	c := baseConfig(version)
	loadShared(getenv, c)

	if v, ok := lookup(getenv, EnvUserAgent); ok {
		if !isValidHeaderValue(v) {
			return nil, invalid("%s must be a valid single-line HTTP header value", EnvUserAgent)
		}
		c.UserAgent = v
	}

	if v, ok := lookup(getenv, EnvScrapeConcurrency); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil, invalid("%s must be an integer", EnvScrapeConcurrency)
		}
		if n < MinScrapeWorkers || n > MaxScrapeWorkers {
			return nil, invalid("%s must be within %d-%d", EnvScrapeConcurrency, MinScrapeWorkers, MaxScrapeWorkers)
		}
		c.ScrapeWorkers = n
	}

	return c, nil
}

func baseConfig(version string) *Config {
	c := &Config{
		OutputDir:     DefaultOutputDir,
		UserAgent:     userAgentPrefix + version,
		ScrapeWorkers: DefaultScrapeWorkers,
	}
	return c
}

func loadShared(getenv Getenv, c *Config) {
	if v, ok := lookup(getenv, EnvExaAPIKey); ok {
		c.ExaAPIKey = v
		c.exaAPIKeySet = true
	}

	if v, ok := lookup(getenv, EnvOutputDir); ok {
		c.OutputDir = v
	}
}

// HasExaAPIKey reports whether optional EXA_API_KEY is configured.
func (c *Config) HasExaAPIKey() bool { return c.exaAPIKeySet && c.ExaAPIKey != "" }

// lookup reads an environment variable and treats an empty value as unset.
func lookup(getenv Getenv, key string) (string, bool) {
	v := getenv(key)
	if v == "" {
		return "", false
	}
	return v, true
}

// isValidHeaderValue checks for a non-empty single-line HTTP header value.
func isValidHeaderValue(v string) bool {
	if v == "" {
		return false
	}
	for i := 0; i < len(v); i++ {
		b := v[i]
		if b == '\r' || b == '\n' || b == 0 {
			return false
		}
		if b < 0x20 && b != '\t' {
			return false
		}
		if b == 0x7f {
			return false
		}
	}
	return true
}

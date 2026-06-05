package config

import (
	"errors"
	"testing"
)

// envMap builds a map-backed Getenv.
func envMap(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(envMap(nil), "1.2.3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.OutputDir != DefaultOutputDir {
		t.Errorf("OutputDir = %q, want %q", c.OutputDir, DefaultOutputDir)
	}
	if c.ScrapeWorkers != DefaultScrapeWorkers {
		t.Errorf("ScrapeWorkers = %d, want %d", c.ScrapeWorkers, DefaultScrapeWorkers)
	}
	if c.UserAgent != "opensearch-cli/1.2.3" {
		t.Errorf("UserAgent = %q, want opensearch-cli/1.2.3", c.UserAgent)
	}
	if c.HasExaAPIKey() {
		t.Error("HasExaAPIKey should be false when unset")
	}
}

func TestLoadOverrides(t *testing.T) {
	c, err := Load(envMap(map[string]string{
		EnvExaAPIKey:         "secret-key",
		EnvOutputDir:         "/tmp/out",
		EnvUserAgent:         "my-agent/9",
		EnvScrapeConcurrency: "16",
	}), "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.HasExaAPIKey() || c.ExaAPIKey != "secret-key" {
		t.Error("EXA_API_KEY not loaded")
	}
	if c.OutputDir != "/tmp/out" {
		t.Errorf("OutputDir = %q", c.OutputDir)
	}
	if c.UserAgent != "my-agent/9" {
		t.Errorf("UserAgent = %q", c.UserAgent)
	}
	if c.ScrapeWorkers != 16 {
		t.Errorf("ScrapeWorkers = %d", c.ScrapeWorkers)
	}
}

func TestLoadEmptyOptionalTreatedAsUnset(t *testing.T) {
	c, err := Load(envMap(map[string]string{
		EnvExaAPIKey:         "",
		EnvOutputDir:         "",
		EnvScrapeConcurrency: "",
	}), "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.HasExaAPIKey() {
		t.Error("empty EXA_API_KEY should be treated as unset")
	}
	if c.OutputDir != DefaultOutputDir {
		t.Errorf("empty output dir should fall back to default, got %q", c.OutputDir)
	}
	if c.ScrapeWorkers != DefaultScrapeWorkers {
		t.Errorf("empty concurrency should default, got %d", c.ScrapeWorkers)
	}
}

func TestLoadInvalidConcurrency(t *testing.T) {
	for _, v := range []string{"0", "17", "-1", "abc", "4.5"} {
		_, err := Load(envMap(map[string]string{EnvScrapeConcurrency: v}), "1.0.0")
		if err == nil {
			t.Errorf("concurrency %q should be rejected, not silently fall back", v)
			continue
		}
		if !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("error for %q should wrap ErrInvalidConfig: %v", v, err)
		}
	}
}

func TestLoadScrapeAlwaysValidatesInvalidConcurrencyEnv(t *testing.T) {
	// Invalid OPENSEARCH_SCRAPE_CONCURRENCY must fail even if a later CLI
	// flag would otherwise override the value.
	for _, v := range []string{"bad", "0", "999", "-1"} {
		_, err := LoadScrape(envMap(map[string]string{
			EnvScrapeConcurrency: v,
		}), "1.0.0")
		if err == nil {
			t.Fatalf("OPENSEARCH_SCRAPE_CONCURRENCY=%q should be rejected", v)
		}
	}
}

func TestLoadInvalidUserAgent(t *testing.T) {
	for _, v := range []string{"bad\nagent", "bad\ragent", "with\x00null"} {
		_, err := Load(envMap(map[string]string{EnvUserAgent: v}), "1.0.0")
		if err == nil {
			t.Errorf("user agent %q should be rejected", v)
		}
	}
}

func TestLoadSearchIgnoresScrapeOnlyInvalidConfig(t *testing.T) {
	c, err := LoadSearch(envMap(map[string]string{
		EnvExaAPIKey:         "secret-key",
		EnvOutputDir:         "/tmp/out",
		EnvUserAgent:         "bad\nagent",
		EnvScrapeConcurrency: "abc",
	}), "1.0.0")
	if err != nil {
		t.Fatalf("LoadSearch should ignore scrape-only invalid config: %v", err)
	}
	if !c.HasExaAPIKey() || c.OutputDir != "/tmp/out" {
		t.Fatalf("search config did not load shared fields: %+v", c)
	}
}

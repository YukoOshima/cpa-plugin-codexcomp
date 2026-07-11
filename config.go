package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

const defaultMarkerText = "Continue thinking..."

const (
	defaultTruncationStep        = 518
	defaultMaxTierN              = 11
	defaultMaxContinue           = 3
	defaultMaxStartupRetries     = 3
	defaultRetryInitialBackoffMS = 500
	defaultRetryMaxBackoffMS     = 2000
)

// foldConfig mirrors cpa-model-fallback-router's pluginConfig pattern:
// yaml-tagged struct, decoded by yaml.Unmarshal, normalized and validated.
type foldConfig struct {
	MarkerText            string `yaml:"marker_text"`
	MaxTierN              int    `yaml:"max_tier_n"`
	MaxContinue           int    `yaml:"max_continue"`
	MaxStartupRetries     int    `yaml:"max_startup_retries"`
	RetryInitialBackoffMS int    `yaml:"retry_initial_backoff_ms"`
	RetryMaxBackoffMS     int    `yaml:"retry_max_backoff_ms"`
	DebugLog              bool   `yaml:"debug_log"`
}

var globalFoldConfig atomic.Value

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

func applyLifecycleConfig(raw []byte) error {
	if len(raw) == 0 {
		setFoldConfig(defaultFoldConfig())
		return nil
	}

	var req lifecycleRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("decode lifecycle request: %w", err)
	}

	cfg, err := decodeFoldConfig(req.ConfigYAML)
	if err != nil {
		return err
	}
	setFoldConfig(cfg)
	if cfg.DebugLog {
		pluginLog("debug", fmt.Sprintf(
			"config applied: max_tier_n=%d max_continue=%d max_startup_retries=%d retry_initial_backoff_ms=%d retry_max_backoff_ms=%d marker_text_len=%d",
			cfg.MaxTierN,
			cfg.MaxContinue,
			cfg.MaxStartupRetries,
			cfg.RetryInitialBackoffMS,
			cfg.RetryMaxBackoffMS,
			len(cfg.MarkerText),
		))
	}
	return nil
}

func defaultFoldConfig() foldConfig {
	return foldConfig{
		MarkerText:            defaultMarkerText,
		MaxTierN:              defaultMaxTierN,
		MaxContinue:           defaultMaxContinue,
		MaxStartupRetries:     defaultMaxStartupRetries,
		RetryInitialBackoffMS: defaultRetryInitialBackoffMS,
		RetryMaxBackoffMS:     defaultRetryMaxBackoffMS,
	}
}

func currentFoldConfig() foldConfig {
	if raw, ok := globalFoldConfig.Load().(foldConfig); ok {
		return raw
	}
	return defaultFoldConfig()
}

func setFoldConfig(cfg foldConfig) {
	globalFoldConfig.Store(cfg)
}

// decodeFoldConfig follows the fallback-router pattern: start from defaults,
// unmarshal on top, normalize, validate.
func decodeFoldConfig(raw []byte) (foldConfig, error) {
	cfg := defaultFoldConfig()
	if strings.TrimSpace(string(raw)) != "" {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return foldConfig{}, fmt.Errorf("invalid %s config: %w", pluginIdentifier, err)
		}
	}
	normalizeFoldConfig(&cfg)
	if err := validateFoldConfig(cfg); err != nil {
		return foldConfig{}, err
	}
	return cfg, nil
}

func normalizeFoldConfig(cfg *foldConfig) {
	if cfg == nil {
		return
	}
	cfg.MarkerText = strings.TrimSpace(cfg.MarkerText)
	if cfg.MarkerText == "" {
		cfg.MarkerText = defaultMarkerText
	}
}

func validateFoldConfig(cfg foldConfig) error {
	if cfg.MaxTierN < 0 {
		return fmt.Errorf("max_tier_n must be a non-negative integer")
	}
	if cfg.MaxContinue < 0 {
		return fmt.Errorf("max_continue must be a non-negative integer")
	}
	if cfg.MaxStartupRetries < 0 {
		return fmt.Errorf("max_startup_retries must be a non-negative integer")
	}
	if cfg.RetryInitialBackoffMS <= 0 {
		return fmt.Errorf("retry_initial_backoff_ms must be a positive integer")
	}
	if cfg.RetryMaxBackoffMS <= 0 {
		return fmt.Errorf("retry_max_backoff_ms must be a positive integer")
	}
	if cfg.RetryMaxBackoffMS < cfg.RetryInitialBackoffMS {
		return fmt.Errorf("retry_max_backoff_ms must be greater than or equal to retry_initial_backoff_ms")
	}
	return nil
}

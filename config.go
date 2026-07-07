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
	defaultTruncationStep = 518
	defaultMaxTierN       = 6
	defaultMaxContinue    = 3
)

// foldConfig mirrors cpa-model-fallback-router's pluginConfig pattern:
// yaml-tagged struct, decoded by yaml.Unmarshal, normalized and validated.
type foldConfig struct {
	MarkerText     string `yaml:"marker_text"`
	TruncationStep int    `yaml:"truncation_step"`
	MaxTierN       int    `yaml:"max_tier_n"`
	MaxContinue    int    `yaml:"max_continue"`
	DebugLog       bool   `yaml:"debug_log"`
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
		pluginLog("debug", fmt.Sprintf("config applied: truncation_step=%d max_tier_n=%d max_continue=%d marker_text_len=%d", cfg.TruncationStep, cfg.MaxTierN, cfg.MaxContinue, len(cfg.MarkerText)))
	}
	return nil
}

func defaultFoldConfig() foldConfig {
	return foldConfig{
		MarkerText:     defaultMarkerText,
		TruncationStep: defaultTruncationStep,
		MaxTierN:       defaultMaxTierN,
		MaxContinue:    defaultMaxContinue,
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
	if cfg.TruncationStep <= 0 {
		return fmt.Errorf("truncation_step must be a positive integer")
	}
	if cfg.MaxTierN < 0 {
		return fmt.Errorf("max_tier_n must be a non-negative integer")
	}
	if cfg.MaxContinue < 0 {
		return fmt.Errorf("max_continue must be a non-negative integer")
	}
	return nil
}

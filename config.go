package main

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Enabled         bool     `yaml:"enabled"`
	PrimaryLanguage string   `yaml:"primary_language"`
	MinWordLength   int      `yaml:"min_word_length"`
	ExcludedApps    []string `yaml:"excluded_apps"`
	AutoConvert     bool     `yaml:"auto_convert"`
}

func DefaultConfig() Config {
	return Config{
		Enabled:         true,
		PrimaryLanguage: "ru",
		MinWordLength:   2,
		ExcludedApps:    []string{"idea"},
		AutoConvert:     true,
	}
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "RuSwitch", "config.yaml")
}

// LoadConfig reads ~/Library/Application Support/RuSwitch/config.yaml and
// merges its contents over DefaultConfig. Pre-existing `hotkeys:` blocks
// from older versions are silently ignored: yaml.Unmarshal in non-strict
// mode discards unknown top-level keys, so the field is dropped on the
// next SaveConfig write without any user-visible parse error.
func LoadConfig() (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(configPath())
	if err != nil {
		// No config file — save defaults and return
		_ = SaveConfig(&cfg)
		return &cfg, nil
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return &cfg, err
	}

	return &cfg, nil
}

// IsAppExcluded checks if the given app bundle ID (or substring) matches the excluded list.
// Matching is case-insensitive substring — e.g. "idea" matches "com.jetbrains.intellij.idea.ce".
func (c *Config) IsAppExcluded(bundleID string) bool {
	if bundleID == "" || len(c.ExcludedApps) == 0 {
		return false
	}
	lower := strings.ToLower(bundleID)
	for _, ex := range c.ExcludedApps {
		if ex == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(ex)) {
			return true
		}
	}
	return false
}

func SaveConfig(cfg *Config) error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

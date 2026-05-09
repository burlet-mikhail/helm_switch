package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// HotkeyConfig holds raw hotkey specifications loaded from config.yaml.
// Strings use the form "Cmd+Shift+X"; parsing happens via ParseHotkey.
type HotkeyConfig struct {
	ConvertSelection string `yaml:"convert_selection"`
	ConvertLastWord  string `yaml:"convert_last_word"`
}

type Config struct {
	Enabled         bool         `yaml:"enabled"`
	PrimaryLanguage string       `yaml:"primary_language"`
	MinWordLength   int          `yaml:"min_word_length"`
	ExcludedApps    []string     `yaml:"excluded_apps"`
	AutoConvert     bool         `yaml:"auto_convert"`
	Hotkeys         HotkeyConfig `yaml:"hotkeys"`
}

// Default hotkey specs — kept in one place so DefaultConfig and the
// LoadConfig backfill stay in sync.
const (
	defaultHotkeyConvertSelection = "Cmd+Shift+X"
	defaultHotkeyConvertLastWord  = "Ctrl+A"
)

// CGEventFlags modifier bitmasks (mirrors hook_darwin.go constants but kept
// here so config parsing has no build-tag dependency on darwin code).
const (
	hotkeyFlagShift   int64 = 1 << 17
	hotkeyFlagControl int64 = 1 << 18
	hotkeyFlagAlt     int64 = 1 << 19
	hotkeyFlagCommand int64 = 1 << 20
)

// Hotkey is a parsed hotkey spec ready for keycode/flag matching against
// a CGEventTap event.
type Hotkey struct {
	KeyCode   uint16
	Modifiers int64
}

// modifierMap maps lowercased modifier names to their CGEventFlags bitmask.
var modifierMap = map[string]int64{
	"cmd":     hotkeyFlagCommand,
	"command": hotkeyFlagCommand,
	"shift":   hotkeyFlagShift,
	"ctrl":    hotkeyFlagControl,
	"control": hotkeyFlagControl,
	"alt":     hotkeyFlagAlt,
	"option":  hotkeyFlagAlt,
	"opt":     hotkeyFlagAlt,
}

// keyMap maps lowercased key tokens to macOS virtual keycodes (kVK_ANSI_*).
var keyMap = map[string]uint16{
	"a": 0x00, "b": 0x0B, "c": 0x08, "d": 0x02, "e": 0x0E,
	"f": 0x03, "g": 0x05, "h": 0x04, "i": 0x22, "j": 0x26,
	"k": 0x28, "l": 0x25, "m": 0x2E, "n": 0x2D, "o": 0x1F,
	"p": 0x23, "q": 0x0C, "r": 0x0F, "s": 0x01, "t": 0x11,
	"u": 0x20, "v": 0x09, "w": 0x0D, "x": 0x07, "y": 0x10,
	"z": 0x06,
	"0": 0x1D, "1": 0x12, "2": 0x13, "3": 0x14, "4": 0x15,
	"5": 0x17, "6": 0x16, "7": 0x1A, "8": 0x1C, "9": 0x19,
	"space":  0x31,
	"return": 0x24,
	"enter":  0x24,
}

// ParseHotkey parses a string spec like "Cmd+Shift+X" into a Hotkey.
// The last token is the key, preceding tokens are modifiers. Matching is
// case-insensitive. Whitespace around tokens is trimmed.
func ParseHotkey(spec string) (Hotkey, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return Hotkey{}, fmt.Errorf("empty hotkey spec")
	}

	parts := strings.Split(trimmed, "+")
	if len(parts) == 0 {
		return Hotkey{}, fmt.Errorf("empty hotkey spec")
	}

	// Trim individual tokens — users may write "Cmd + Shift + X".
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
		if parts[i] == "" {
			return Hotkey{}, fmt.Errorf("empty token in hotkey spec %q", spec)
		}
	}

	keyTok := strings.ToLower(parts[len(parts)-1])
	keyCode, ok := keyMap[keyTok]
	if !ok {
		return Hotkey{}, fmt.Errorf("unknown key %q in hotkey spec %q", parts[len(parts)-1], spec)
	}

	var mods int64
	for _, modTok := range parts[:len(parts)-1] {
		bits, ok := modifierMap[strings.ToLower(modTok)]
		if !ok {
			return Hotkey{}, fmt.Errorf("unknown modifier %q in hotkey spec %q", modTok, spec)
		}
		mods |= bits
	}

	return Hotkey{KeyCode: keyCode, Modifiers: mods}, nil
}

// ParsedHotkeys returns parsed selection and last-word hotkeys, falling back
// to the hardcoded defaults (Cmd+Shift+X, Cmd+Shift+Z) on parse error and
// logging a warning so misconfigured users still get a working app.
func (c *Config) ParsedHotkeys() (selection, lastWord Hotkey) {
	defaultSelection := Hotkey{KeyCode: 0x07, Modifiers: hotkeyFlagCommand | hotkeyFlagShift}
	defaultLastWord := Hotkey{KeyCode: 0x06, Modifiers: hotkeyFlagCommand | hotkeyFlagShift}

	selection = defaultSelection
	if spec := c.Hotkeys.ConvertSelection; spec != "" {
		if hk, err := ParseHotkey(spec); err == nil {
			selection = hk
		} else {
			log.Printf("hotkey parse warning: convert_selection=%q invalid (%v); using default %s",
				spec, err, defaultHotkeyConvertSelection)
		}
	}

	lastWord = defaultLastWord
	if spec := c.Hotkeys.ConvertLastWord; spec != "" {
		if hk, err := ParseHotkey(spec); err == nil {
			lastWord = hk
		} else {
			log.Printf("hotkey parse warning: convert_last_word=%q invalid (%v); using default %s",
				spec, err, defaultHotkeyConvertLastWord)
		}
	}

	return selection, lastWord
}

func DefaultConfig() Config {
	return Config{
		Enabled:         true,
		PrimaryLanguage: "ru",
		MinWordLength:   2,
		ExcludedApps:    []string{"idea"},
		AutoConvert:     true,
		Hotkeys: HotkeyConfig{
			ConvertSelection: defaultHotkeyConvertSelection,
			ConvertLastWord:  defaultHotkeyConvertLastWord,
		},
	}
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "RuSwitch", "config.yaml")
}

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

	// Backfill hotkeys with defaults if the user's config predates the
	// hotkeys block. Empty strings mean "not set" — both old configs (no
	// key at all) and configs with explicit empty values get the default.
	if cfg.Hotkeys.ConvertSelection == "" {
		cfg.Hotkeys.ConvertSelection = defaultHotkeyConvertSelection
		log.Printf("config: convert_selection not set, defaulting to %s", defaultHotkeyConvertSelection)
	}
	if cfg.Hotkeys.ConvertLastWord == "" {
		cfg.Hotkeys.ConvertLastWord = defaultHotkeyConvertLastWord
		log.Printf("config: convert_last_word not set, defaulting to %s", defaultHotkeyConvertLastWord)
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

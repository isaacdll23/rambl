// Package config centralizes rambl's user-configurable settings, persisted in
// a human-editable JSON file (~/.rambl/config.json), with environment-variable
// overrides and built-in defaults.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Config holds rambl's user-configurable settings.
type Config struct {
	// TurnTimeout is the per-turn wall-clock cap for a worker session.
	TurnTimeout time.Duration
}

// configAlias is the on-disk representation, storing the duration as a
// human-friendly string (e.g. "15m") rather than nanoseconds.
type configAlias struct {
	TurnTimeout string `json:"turnTimeout,omitempty"`
}

// MarshalJSON renders Config with the duration formatted as a string.
func (c Config) MarshalJSON() ([]byte, error) {
	a := configAlias{}
	if c.TurnTimeout != 0 {
		a.TurnTimeout = c.TurnTimeout.String()
	}
	return json.Marshal(a)
}

// UnmarshalJSON parses Config, interpreting the duration from its string form.
// An empty/missing turnTimeout leaves the field zero (Load fills it).
func (c *Config) UnmarshalJSON(data []byte) error {
	var a configAlias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	if a.TurnTimeout != "" {
		d, err := time.ParseDuration(a.TurnTimeout)
		if err != nil {
			return fmt.Errorf("config: invalid turnTimeout %q: %w", a.TurnTimeout, err)
		}
		c.TurnTimeout = d
	}
	return nil
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{TurnTimeout: 15 * time.Minute}
}

// Path returns the absolute path to the config file (~/.rambl/config.json).
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".rambl", "config.json"), nil
}

// Load reads the config file if present (a missing file is NOT an error —
// defaults are used), applies environment overrides, fills any zero-valued
// field with its default, and returns the effective config.
func Load() (Config, error) {
	var cfg Config

	path, err := Path()
	if err != nil {
		return Config{}, err
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("config: parsing %s: %w", path, err)
		}
	case os.IsNotExist(err):
		// Missing file: fall back to defaults.
	default:
		return Config{}, fmt.Errorf("config: reading %s: %w", path, err)
	}

	// Environment overrides (a non-parsing value is ignored, not fatal).
	if v := os.Getenv("RAMBL_TURN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.TurnTimeout = d
		}
	}

	// Fill zero-valued fields with defaults.
	def := Default()
	if cfg.TurnTimeout == 0 {
		cfg.TurnTimeout = def.TurnTimeout
	}

	return cfg, nil
}

// Save writes cfg to the config file as human-friendly JSON, creating the
// ~/.rambl directory if needed.
func Save(cfg Config) error {
	path, err := Path()
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("config: creating %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("config: writing %s: %w", path, err)
	}
	return nil
}

const keyTurnTimeout = "turn-timeout"

// Keys returns the configurable key names, sorted.
func Keys() []string {
	return []string{keyTurnTimeout}
}

// Get returns the string representation of key's current value on cfg.
// An unknown key is an error.
func Get(cfg Config, key string) (string, error) {
	switch key {
	case keyTurnTimeout:
		return cfg.TurnTimeout.String(), nil
	default:
		return "", fmt.Errorf("config: unknown key %q", key)
	}
}

// Set parses value, validates it, and returns a copy of cfg with key set.
// An unknown key or an invalid value is an error.
func Set(cfg Config, key, value string) (Config, error) {
	switch key {
	case keyTurnTimeout:
		d, err := time.ParseDuration(value)
		if err != nil {
			return cfg, fmt.Errorf("config: invalid value for %s: %q is not a duration", key, value)
		}
		if d <= 0 {
			return cfg, fmt.Errorf("config: invalid value for %s: %s must be greater than zero", key, d)
		}
		cfg.TurnTimeout = d
		return cfg, nil
	default:
		return cfg, fmt.Errorf("config: unknown key %q", key)
	}
}

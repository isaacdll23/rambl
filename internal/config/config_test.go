package config

import (
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	cfg := Config{TurnTimeout: 42 * time.Minute}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.TurnTimeout != cfg.TurnTimeout {
		t.Fatalf("TurnTimeout = %s, want %s", got.TurnTimeout, cfg.TurnTimeout)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.TurnTimeout != Default().TurnTimeout {
		t.Fatalf("TurnTimeout = %s, want default %s", got.TurnTimeout, Default().TurnTimeout)
	}
}

func TestLoadEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if err := Save(Config{TurnTimeout: 5 * time.Minute}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	t.Setenv("RAMBL_TURN_TIMEOUT", "30m")

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.TurnTimeout != 30*time.Minute {
		t.Fatalf("TurnTimeout = %s, want 30m", got.TurnTimeout)
	}
}

func TestLoadEnvOverrideIgnoresInvalid(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if err := Save(Config{TurnTimeout: 5 * time.Minute}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	t.Setenv("RAMBL_TURN_TIMEOUT", "not-a-duration")

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.TurnTimeout != 5*time.Minute {
		t.Fatalf("TurnTimeout = %s, want 5m (invalid env ignored)", got.TurnTimeout)
	}
}

func TestSetValidation(t *testing.T) {
	base := Default()

	for _, bad := range []string{"0", "-5m", "abc"} {
		if _, err := Set(base, "turn-timeout", bad); err == nil {
			t.Errorf("Set(turn-timeout, %q): expected error, got nil", bad)
		}
	}

	got, err := Set(base, "turn-timeout", "20m")
	if err != nil {
		t.Fatalf("Set(turn-timeout, 20m): %v", err)
	}
	if got.TurnTimeout != 20*time.Minute {
		t.Fatalf("TurnTimeout = %s, want 20m", got.TurnTimeout)
	}
}

func TestGetFormatsValue(t *testing.T) {
	cfg := Config{TurnTimeout: 20 * time.Minute}
	got, err := Get(cfg, "turn-timeout")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "20m0s" {
		t.Fatalf("Get = %q, want %q", got, "20m0s")
	}
}

func TestGetSetUnknownKey(t *testing.T) {
	cfg := Default()
	if _, err := Get(cfg, "nope"); err == nil {
		t.Error("Get(nope): expected error, got nil")
	}
	if _, err := Set(cfg, "nope", "value"); err == nil {
		t.Error("Set(nope): expected error, got nil")
	}
}

func TestKeys(t *testing.T) {
	keys := Keys()
	if len(keys) != 1 || keys[0] != "turn-timeout" {
		t.Fatalf("Keys() = %v, want [turn-timeout]", keys)
	}
}

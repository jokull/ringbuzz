// Package config handles on-disk state: refresh token, selected device, HAP pairing dir.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type Config struct {
	// HardwareID is a stable UUID Ring uses to identify this client. Generated
	// on first login and never changes (rotating it triggers 2FA again).
	HardwareID string `toml:"hardware_id"`

	// RefreshToken is the long-lived Ring OAuth refresh token. The ring client
	// rotates this on every access-token refresh and persists it back.
	RefreshToken string `toml:"refresh_token"`

	// DeviceID is the numeric Ring device ID of the intercom to control.
	// Set via `ringbuzz use <id>`.
	DeviceID int64 `toml:"device_id"`

	// DeviceName is the friendly name from Ring, for logging only.
	DeviceName string `toml:"device_name,omitempty"`
}

// Dir returns the config directory (~/.config/ringbuzz), creating it if needed.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".config", "ringbuzz")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// HAPDir returns the directory brutella/hap uses for pairing state.
func HAPDir() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	hap := filepath.Join(d, "hap")
	if err := os.MkdirAll(hap, 0o700); err != nil {
		return "", err
	}
	return hap, nil
}

func path() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.toml"), nil
}

func Load() (*Config, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	c := &Config{}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := toml.Decode(string(data), c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return c, nil
}

func (c *Config) Save() error {
	p, err := path()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(c)
}

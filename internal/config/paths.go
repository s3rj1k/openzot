package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func homeDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}

	// Fallback for unusual environments.
	return "/tmp/zot-" + strconv.Itoa(os.Getuid())
}

func xdgConfigHome() string {
	if dir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); dir != "" {
		return dir
	}

	return filepath.Join(homeDir(), ".config")
}

// DefaultConfigPath returns the config file path. That is $ZOT_CONFIG when set and non-empty, else
// $XDG_CONFIG_HOME/zot/config.yaml when XDG_CONFIG_HOME is set, else ~/.config/zot/config.yaml.
func DefaultConfigPath() string {
	if envPath := strings.TrimSpace(os.Getenv("ZOT_CONFIG")); envPath != "" {
		return envPath
	}

	return filepath.Join(xdgConfigHome(), "zot", "config.yaml")
}

// ConfigDir returns the directory that holds the config file - and any global
// AGENTS.md - for the given --config value ("" uses the default path).
func ConfigDir(path string) string {
	if strings.TrimSpace(path) == "" {
		path = DefaultConfigPath()
	}

	return filepath.Dir(path)
}

package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func HomeDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}

	// Fallback for unusual environments.
	return "/tmp/agent-" + strconv.Itoa(os.Getuid())
}

func xdgConfigHome() string {
	if dir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); dir != "" {
		return dir
	}

	return filepath.Join(HomeDir(), ".config")
}

// DefaultConfigPath returns the config file path. That is $AGENT_CONFIG when set and non-empty, else
// $XDG_CONFIG_HOME/agent/config.yaml when XDG_CONFIG_HOME is set, else ~/.config/agent/config.yaml.
func DefaultConfigPath() string {
	if envPath := strings.TrimSpace(os.Getenv("AGENT_CONFIG")); envPath != "" {
		return envPath
	}

	return filepath.Join(xdgConfigHome(), "agent", "config.yaml")
}

// ConfigDir returns the directory that holds the config file - and any global
// AGENTS.md - for the given --config value ("" uses the default path).
func ConfigDir(path string) string {
	if strings.TrimSpace(path) == "" {
		path = DefaultConfigPath()
	}

	return filepath.Dir(path)
}

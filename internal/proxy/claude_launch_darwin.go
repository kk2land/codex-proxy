package proxy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func prepareClaudeLaunch(kind claudeCommand, args []string) (launchSpec, error) {
	if kind == claudePassThrough {
		return launchSpec{args: args}, nil
	}
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if !filepath.IsAbs(configDir) || filepath.Base(filepath.Clean(configDir)) != ".claude" || !safeSandboxPath(configDir) {
		return launchSpec{}, errors.New("macOS Claude tasks require absolute CLAUDE_CONFIG_DIR in the form /shared/user/.claude")
	}
	// Reuse the existing directory-isolation policy without changing Claude's
	// permission mode, SDK callbacks, or plan mode.
	profile, err := sandboxProfile(filepath.Join(filepath.Dir(filepath.Clean(configDir)), ".codex"))
	if err != nil {
		return launchSpec{}, errors.New(strings.ReplaceAll(err.Error(), "CODEX_HOME", "CLAUDE_CONFIG_DIR"))
	}
	return launchSpec{profile: profile, args: args}, nil
}

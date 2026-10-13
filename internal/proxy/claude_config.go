package proxy

import (
	"fmt"
	"strings"
)

// Claude uses the same validated scheduler settings in a separate account-owned
// directory. Neither HOME nor CLAUDE_CONFIG_DIR can create a different quota.
func LoadClaudeConfig(dir, self string) (Config, error) {
	values, err := loadConfigValues(dir)
	if err != nil {
		return DefaultConfig(), err
	}
	mapped := make(map[string]string, len(values))
	for key, value := range values {
		switch key {
		case "CLAUDE_PROXY_REAL_BIN", "CLAUDE_PROXY_MAX_CONCURRENCY", "CLAUDE_PROXY_WAIT_TIMEOUT", "CLAUDE_PROXY_LOG_MAX_SIZE_MIB", "CLAUDE_PROXY_LOG_MAX_BACKUPS":
			mapped[strings.Replace(key, "CLAUDE_PROXY_", "CODEX_PROXY_", 1)] = value
		default:
			return DefaultConfig(), fmt.Errorf("unknown config key %q", key)
		}
	}
	cfg, err := configFromValues(mapped, self)
	if err != nil {
		err = fmt.Errorf("%s", strings.ReplaceAll(err.Error(), "CODEX_PROXY_", "CLAUDE_PROXY_"))
	}
	return cfg, err
}

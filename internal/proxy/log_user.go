package proxy

import "path/filepath"

// codexHomeUser identifies the runtime user from CODEX_HOME without consulting
// the account database or resolving filesystem aliases.
func codexHomeUser(codexHome string) string {
	clean := filepath.Clean(codexHome)
	if !filepath.IsAbs(codexHome) || filepath.Base(clean) != ".codex" || !safeSandboxPath(codexHome) {
		return ""
	}
	user := filepath.Base(filepath.Dir(clean))
	if user == string(filepath.Separator) {
		return ""
	}
	return user
}

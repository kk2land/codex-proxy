package proxy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// sandboxProfile isolates siblings of the runtime user's directory, while
// allowing metadata queries on their common parent and the workspace tree.
// It does not use HOME.
func sandboxProfile(codexHome string) (string, error) {
	return sandboxProfileWithAuth(codexHome, "")
}

func sandboxProfileWithAuth(codexHome, authFile string) (string, error) {
	if !filepath.IsAbs(codexHome) || filepath.Base(filepath.Clean(codexHome)) != ".codex" {
		return "", errors.New("macOS exec requires an absolute CODEX_HOME in the form /shared/user/.codex")
	}
	if !safeSandboxPath(codexHome) {
		return "", errors.New("CODEX_HOME contains invalid text or control characters")
	}
	userDir := filepath.Dir(filepath.Clean(codexHome))
	sharedRoot := filepath.Dir(userDir)
	if sharedRoot == string(filepath.Separator) {
		return "", errors.New("CODEX_HOME must have a shared parent directory other than /")
	}
	root, err := canonicalSandboxPath(sharedRoot)
	if err != nil {
		return "", fmt.Errorf("resolve CODEX_HOME shared parent: %w", err)
	}
	user, err := canonicalSandboxPath(userDir)
	if err != nil {
		return "", fmt.Errorf("resolve CODEX_HOME user directory: %w", err)
	}
	if root == string(filepath.Separator) || filepath.Dir(user) != root || user == root {
		return "", errors.New("CODEX_HOME user directory must resolve to a direct child of its shared parent")
	}
	if !safeSandboxPath(root) || !safeSandboxPath(user) {
		return "", errors.New("resolved CODEX_HOME paths contain invalid text or control characters")
	}
	allowed := fmt.Sprintf("(subpath %s)", quoteSandboxPath(user))
	extra := ""
	if authFile != "" {
		if !filepath.IsAbs(authFile) || !safeSandboxPath(authFile) {
			return "", errors.New("invalid selected auth file for sandbox")
		}
		allowed = fmt.Sprintf("(require-any (subpath %s) (literal %s))", quoteSandboxPath(user), quoteSandboxPath(authFile))
		extra = fmt.Sprintf("(allow file-read-metadata\n    (literal %s))\n", quoteSandboxPath(filepath.Dir(authFile)))
	}
	return fmt.Sprintf(`(version 1)
(allow default)
(deny file-read* file-write*
    (require-all
        (subpath %s)
        (require-not
            %s)
    )
)
(allow file-read-metadata
    (literal %s))
(allow file-read-metadata
    (subpath "/Users/agent/workspaces"))
`, quoteSandboxPath(root), allowed, quoteSandboxPath(root)) + extra, nil
}

func safeSandboxPath(path string) bool {
	return utf8.ValidString(path) && strings.IndexFunc(path, unicode.IsControl) < 0
}

func quoteSandboxPath(path string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path) + `"`
}

// Resolve existing ancestors so aliases such as /tmp and /var match the paths
// seen by the kernel. New user directories may be created later by Codex.
func canonicalSandboxPath(path string) (string, error) {
	ancestor := path
	var missing []string
	for {
		info, err := os.Lstat(ancestor)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(ancestor)
			if err != nil {
				return "", err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				info, err = os.Stat(resolved)
				if err != nil {
					return "", err
				}
			}
			if !info.IsDir() {
				return "", errors.New("path ancestor is not a directory")
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", err
		}
		missing = append(missing, filepath.Base(ancestor))
		ancestor = parent
	}
}

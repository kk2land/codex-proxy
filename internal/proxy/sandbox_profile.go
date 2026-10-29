package proxy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

const (
	sandboxRulesFilename = "sandbox.sb"
	maxSandboxRulesSize  = 64 << 10
)

func sandboxLaunch(codexHome, proxyDir string, args []string) (launchSpec, error) {
	profile, err := sandboxProfile(codexHome)
	if err != nil {
		return launchSpec{}, err
	}
	suffix, err := sandboxProfileSuffix(proxyDir)
	if err != nil {
		return launchSpec{}, err
	}
	return launchSpec{profile: profile + suffix, profileSuffix: suffix, args: args}, nil
}

// The system account's proxy directory is independent of runtime HOME,
// CODEX_HOME, DSH_HOME and PI_CODING_AGENT_DIR. Read its rules before sandboxing,
// then deny access after all other rules, including the selected account's
// authentication grant.
func sandboxProfileSuffix(proxyDir string) (string, error) {
	if !filepath.IsAbs(proxyDir) || !safeSandboxPath(proxyDir) {
		return "", errors.New("invalid sandbox proxy directory")
	}
	canonical, err := canonicalSandboxPath(proxyDir)
	if err != nil {
		return "", fmt.Errorf("resolve sandbox proxy directory: %w", err)
	}
	if canonical == string(filepath.Separator) || !safeSandboxPath(canonical) {
		return "", errors.New("invalid resolved sandbox proxy directory")
	}
	rules, err := readSandboxRules(filepath.Join(proxyDir, sandboxRulesFilename))
	if err != nil {
		return "", err
	}
	// Separate lines also keep a trailing comment in sandbox.sb from consuming
	// the mandatory deny. Explicit operations cover earlier specific grants,
	// notably the base policy's file-read-metadata exception.
	deny := fmt.Sprintf(`
(deny file-read* file-read-data file-read-metadata file-read-xattr
      file-write* file-write-create file-write-data file-write-flags
      file-write-mode file-write-owner file-write-setugid file-write-times
      file-write-unlink file-write-xattr
    (subpath %s)`, quoteSandboxPath(canonical))
	if original := filepath.Clean(proxyDir); original != canonical {
		deny += fmt.Sprintf("\n    (subpath %s)", quoteSandboxPath(original))
	}
	return "\n" + string(rules) + "\n" + deny + ")\n", nil
}

func readSandboxRules(path string) ([]byte, error) {
	// A symlink could put the effective rules outside the protected directory.
	// Nonblocking open also lets us reject FIFOs without waiting for a writer.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read sandbox rules %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat sandbox rules %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("sandbox rules %s must be a regular file", path)
	}
	rules, err := io.ReadAll(io.LimitReader(file, maxSandboxRulesSize+1))
	if err != nil {
		return nil, fmt.Errorf("read sandbox rules %s: %w", path, err)
	}
	if len(rules) > maxSandboxRulesSize {
		return nil, fmt.Errorf("sandbox rules %s exceed %d bytes", path, maxSandboxRulesSize)
	}
	if !utf8.Valid(rules) || strings.IndexByte(string(rules), 0) >= 0 {
		return nil, fmt.Errorf("sandbox rules %s must be UTF-8 text without NUL bytes", path)
	}
	return rules, nil
}

// sandboxProfile isolates siblings of the runtime user's directory, while
// allowing metadata queries on their common parent and the workspace tree.
// It does not use HOME.
func sandboxProfile(codexHome string) (string, error) {
	return sandboxProfileWithAuth(codexHome, "")
}

func sandboxProfileWithAuth(codexHome, authFile string) (string, error) {
	return sandboxProfileForHome(codexHome, "CODEX_HOME", ".codex", authFile)
}

// All launchers use the same isolation policy. The suffix locates the config
// directory below the user: .codex, .pi/agent, or any single directory for dsh.
func sandboxProfileForHome(home, homeEnv, suffix, authFile string) (string, error) {
	clean := filepath.Clean(home)
	if !filepath.IsAbs(home) || clean == string(filepath.Separator) ||
		(suffix != "" && !strings.HasSuffix(clean, string(filepath.Separator)+suffix)) {
		if suffix == "" {
			suffix = ".dsh"
		}
		return "", fmt.Errorf("macOS sandbox requires an absolute %s in the form /shared/user/%s", homeEnv, suffix)
	}
	if !safeSandboxPath(home) {
		return "", fmt.Errorf("%s contains invalid text or control characters", homeEnv)
	}
	userDir := filepath.Dir(clean)
	for range strings.Count(suffix, string(filepath.Separator)) {
		userDir = filepath.Dir(userDir)
	}
	sharedRoot := filepath.Dir(userDir)
	if sharedRoot == string(filepath.Separator) {
		return "", fmt.Errorf("%s must have a shared parent directory other than /", homeEnv)
	}
	root, err := canonicalSandboxPath(sharedRoot)
	if err != nil {
		return "", fmt.Errorf("resolve %s shared parent: %w", homeEnv, err)
	}
	user, err := canonicalSandboxPath(userDir)
	if err != nil {
		return "", fmt.Errorf("resolve %s user directory: %w", homeEnv, err)
	}
	if root == string(filepath.Separator) || filepath.Dir(user) != root || user == root {
		return "", fmt.Errorf("%s user directory must resolve to a direct child of its shared parent", homeEnv)
	}
	if !safeSandboxPath(root) || !safeSandboxPath(user) {
		return "", fmt.Errorf("resolved %s paths contain invalid text or control characters", homeEnv)
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
// seen by the kernel. New user directories may be created later by the program.
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

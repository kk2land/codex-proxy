package proxy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestSandboxLaunchAppendsRulesAndProtectsConfigFiles(t *testing.T) {
	for _, placement := range []string{"outside shared root", "inside user directory"} {
		t.Run(placement, func(t *testing.T) {
			base := t.TempDir()
			home := filepath.Join(base, "shared", "alice", ".codex")
			account := filepath.Join(base, `system-"quoted"\账户`)
			if placement == "inside user directory" {
				account = filepath.Dir(home)
			}
			dir := filepath.Join(account, ".codex-proxy")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			// Runtime HOME must not select either the rules or the denied path.
			t.Setenv("HOME", t.TempDir())
			rules := "(deny network-outbound)\n(allow file-read-metadata file-write-data) ; trailing comment"
			if err := os.WriteFile(filepath.Join(dir, sandboxRulesFilename), []byte(rules), 0600); err != nil {
				t.Fatal(err)
			}
			spec, err := sandboxLaunch(home, dir, []string{"exec", "prompt"})
			if err != nil {
				t.Fatal(err)
			}
			baseProfile, err := sandboxProfile(home)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(spec.profile, baseProfile+"\n") || !strings.Contains(spec.profile, "\n"+rules+"\n") {
				t.Fatalf("custom rules were not appended verbatim after the base policy: %s", spec.profile)
			}
			assertProxyConfigReadOnly(t, spec.profile, canonicalSandboxTestPath(t, dir), dir)
			if !reflect.DeepEqual(spec.args, []string{"exec", "prompt"}) {
				t.Fatalf("launch changed arguments: %q", spec.args)
			}
		})
	}
}

func TestSandboxRulesResolveSystemHomeAlias(t *testing.T) {
	base := t.TempDir()
	account := filepath.Join(base, "account")
	alias := filepath.Join(base, "alias")
	dir := filepath.Join(account, ".codex-proxy")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(account, alias); err != nil {
		t.Fatal(err)
	}
	aliasedDir := filepath.Join(alias, ".codex-proxy")
	suffix, err := sandboxProfileSuffix(aliasedDir)
	if err != nil {
		t.Fatal(err)
	}
	assertProxyConfigReadOnly(t, suffix, canonicalSandboxTestPath(t, dir), aliasedDir)
}

func TestSandboxRulesMissingOrEmptyKeepsConfigReadOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".codex-proxy")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"missing", "empty"} {
		t.Run(state, func(t *testing.T) {
			if state == "empty" {
				if err := os.WriteFile(filepath.Join(dir, sandboxRulesFilename), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			suffix, err := sandboxProfileSuffix(dir)
			if err != nil {
				t.Fatal(err)
			}
			assertProxyConfigReadOnly(t, suffix, canonicalSandboxTestPath(t, dir), dir)
		})
	}
}

func TestSandboxRulesRejectInvalidFiles(t *testing.T) {
	for _, kind := range []string{"directory", "fifo", "symlink", "dangling symlink", "NUL", "invalid UTF-8", "too large"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, sandboxRulesFilename)
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(path, 0700)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			case "symlink", "dangling symlink":
				target := filepath.Join(t.TempDir(), "rules.sb")
				if kind == "symlink" {
					err = os.WriteFile(target, []byte("(allow default)"), 0600)
				}
				if err == nil {
					err = os.Symlink(target, path)
				}
			case "NUL":
				err = os.WriteFile(path, []byte("(deny network*)\x00"), 0600)
			case "invalid UTF-8":
				err = os.WriteFile(path, []byte{0xff}, 0600)
			case "too large":
				err = os.WriteFile(path, []byte(strings.Repeat(";", maxSandboxRulesSize+1)), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			spec, err := sandboxLaunch(filepath.Join(dir, "shared", "alice", ".codex"), dir, []string{"exec"})
			if err == nil || spec.profile != "" || !strings.Contains(err.Error(), sandboxRulesFilename) {
				t.Fatalf("invalid rules accepted or missing diagnostic: profile=%q err=%v", spec.profile, err)
			}
		})
	}
}

func assertProxyConfigReadOnly(t *testing.T, profile string, paths ...string) {
	t.Helper()
	start := strings.LastIndex(profile, "\n(deny file-write* file-write-create")
	if start < 0 {
		t.Fatalf("missing final config file write deny: %s", profile)
	}
	forms, err := parseSandboxTestProfile(profile[start:])
	if err != nil {
		t.Fatal(err)
	}
	writeDeny := []any{"deny", "file-write*", "file-write-create", "file-write-data", "file-write-flags", "file-write-mode",
		"file-write-owner", "file-write-setugid", "file-write-times", "file-write-unlink", "file-write-xattr"}
	readAllow := []any{"allow", "file-read*", "file-read-data", "file-read-metadata", "file-read-xattr"}
	seen := make(map[string]bool)
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		for _, name := range []string{"env", "sandbox.sb"} {
			file := []any{"literal", sandboxTestLiteral(filepath.Join(path, name))}
			writeDeny = append(writeDeny, file)
			readAllow = append(readAllow, file)
		}
	}
	want := []any{writeDeny, readAllow}
	if !reflect.DeepEqual(forms, want) {
		t.Fatalf("unexpected config file protection:\ngot: %#v\nwant: %#v", forms, want)
	}
}

package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSandboxProfileDerivesCanonicalUserAndSharedRoot(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "shared")
	user := filepath.Join(shared, "alice")
	home := filepath.Join(user, ".codex")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	assertSandboxProfileRules(t, home, canonicalSandboxTestPath(t, shared), canonicalSandboxTestPath(t, user))
	// An optional trailing separator does not change the selected user.
	assertSandboxProfileRules(t, home+string(os.PathSeparator), canonicalSandboxTestPath(t, shared), canonicalSandboxTestPath(t, user))
}

func TestSandboxProfileEscapesPathLiterals(t *testing.T) {
	// The user name resembles an injected rule; quotes and backslashes must
	// remain part of one path literal, with the profile's rules unchanged.
	shared := filepath.Join(t.TempDir(), `共享-"quoted"\root`)
	user := filepath.Join(shared, `用户") (allow file-write*) ("\owner`)
	home := filepath.Join(user, ".codex")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	assertSandboxProfileRules(t, home, canonicalSandboxTestPath(t, shared), canonicalSandboxTestPath(t, user))
}

func TestSandboxProfileCanonicalizesTemporaryDirectory(t *testing.T) {
	// macOS resolves /tmp through /private/tmp. This also works on Linux,
	// where /tmp may already be canonical.
	shared, err := os.MkdirTemp("/tmp", "codex-proxy-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shared) })
	user := filepath.Join(shared, "alice")
	home := filepath.Join(user, ".codex")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	assertSandboxProfileRules(t, home, canonicalSandboxTestPath(t, shared), canonicalSandboxTestPath(t, user))
}

func TestSandboxProfileAllowsMissingDirectories(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(shared, 0700); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(shared, "not-created-yet")
	home := filepath.Join(user, ".codex")
	canonicalShared := canonicalSandboxTestPath(t, shared)
	assertSandboxProfileRules(t, home, canonicalShared, filepath.Join(canonicalShared, "not-created-yet"))
	if _, err := os.Stat(user); !os.IsNotExist(err) {
		t.Fatalf("profile generation created the missing user directory: %v", err)
	}

	if err := os.Mkdir(user, 0700); err != nil {
		t.Fatal(err)
	}
	assertSandboxProfileRules(t, home, canonicalShared, canonicalSandboxTestPath(t, user))
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("profile generation created CODEX_HOME: %v", err)
	}

	base := t.TempDir()
	missingShared := filepath.Join(base, "missing-shared")
	missingUser := filepath.Join(missingShared, "alice")
	canonicalBase := canonicalSandboxTestPath(t, base)
	assertSandboxProfileRules(t, filepath.Join(missingUser, ".codex"),
		filepath.Join(canonicalBase, "missing-shared"), filepath.Join(canonicalBase, "missing-shared", "alice"))
	if _, err := os.Stat(missingShared); !os.IsNotExist(err) {
		t.Fatalf("profile generation created the missing shared directory: %v", err)
	}
}

func TestSandboxProfileResolvesSharedRootSymlink(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, "actual-shared")
	alias := filepath.Join(base, "shared-alias")
	user := filepath.Join(shared, "alice")
	if err := os.MkdirAll(user, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, alias); err != nil {
		t.Fatal(err)
	}
	canonicalShared := canonicalSandboxTestPath(t, shared)
	assertSandboxProfileRules(t, filepath.Join(alias, "alice", ".codex"), canonicalShared, canonicalSandboxTestPath(t, user))
	// Resolving the nearest existing ancestor must work through the alias too.
	assertSandboxProfileRules(t, filepath.Join(alias, "missing", ".codex"), canonicalShared, filepath.Join(canonicalShared, "missing"))
}

func TestSandboxProfileRejectsInvalidHomeShape(t *testing.T) {
	base := t.TempDir()
	for _, test := range []struct {
		name string
		home string
	}{
		{"missing", ""},
		{"relative", "shared/alice/.codex"},
		{"relative single component", ".codex"},
		{"wrong basename", filepath.Join(base, "shared", "alice", "codex")},
		{"wrong hidden basename", filepath.Join(base, "shared", "alice", ".codex-other")},
		{"root home", string(os.PathSeparator)},
		{"user root", string(os.PathSeparator) + ".codex"},
		{"shared root", filepath.Join(string(os.PathSeparator), "alice", ".codex")},
		{"NUL", filepath.Join(base, "shared", "alice\x00", ".codex")},
		{"newline", filepath.Join(base, "shared", "alice\n", ".codex")},
		{"invalid UTF-8", filepath.Join(base, "shared", "alice\xff", ".codex")},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile, err := sandboxProfile(test.home)
			if err == nil {
				t.Fatalf("sandboxProfile(%q) succeeded with profile %q", test.home, profile)
			}
			if profile != "" {
				t.Fatalf("invalid home returned a usable profile: %q", profile)
			}
		})
	}
}

func TestSandboxProfileRejectsSharedRootSymlinkToRoot(t *testing.T) {
	alias := filepath.Join(t.TempDir(), "root-alias")
	if err := os.Symlink(string(os.PathSeparator), alias); err != nil {
		t.Fatal(err)
	}
	if profile, err := sandboxProfile(filepath.Join(alias, "alice", ".codex")); err == nil || profile != "" {
		t.Fatalf("shared root resolving to / was accepted: profile=%q, err=%v", profile, err)
	}
}

func TestSandboxProfileRejectsUserSymlinkOutsideSharedRoot(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, "shared")
	outside := filepath.Join(base, "outside", "alice")
	if err := os.MkdirAll(shared, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(shared, "alice")); err != nil {
		t.Fatal(err)
	}
	if profile, err := sandboxProfile(filepath.Join(shared, "alice", ".codex")); err == nil || profile != "" {
		t.Fatalf("escaped user symlink was accepted: profile=%q, err=%v", profile, err)
	}
}

func assertSandboxProfileRules(t *testing.T, home, shared, user string) {
	t.Helper()
	profile, err := sandboxProfile(home)
	if err != nil {
		t.Fatalf("sandboxProfile(%q): %v", home, err)
	}
	forms, err := parseSandboxTestProfile(profile)
	if err != nil {
		t.Fatalf("malformed profile for %q: %v\n%s", home, err, profile)
	}
	// Check rule structure rather than whitespace or one specific escaping
	// representation. An unescaped quote would introduce extra forms or values.
	want := []any{
		[]any{"version", "1"},
		[]any{"allow", "default"},
		[]any{"deny", "file-read*", "file-write*", []any{"require-all",
			[]any{"subpath", sandboxTestLiteral(shared)},
			[]any{"require-not", []any{"subpath", sandboxTestLiteral(user)}},
		}},
		[]any{"allow", "file-read-metadata", []any{"literal", sandboxTestLiteral(shared)}},
	}
	if !reflect.DeepEqual(forms, want) {
		t.Fatalf("unexpected sandbox policy for %q:\ngot:  %#v\nwant: %#v\n%s", home, forms, want, profile)
	}
}

func canonicalSandboxTestPath(t *testing.T, path string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

type sandboxTestLiteral string

// parseSandboxTestProfile understands the small S-expression grammar needed
// for the policy, preserving the difference between atoms and path literals.
func parseSandboxTestProfile(profile string) ([]any, error) {
	if !utf8.ValidString(profile) {
		return nil, fmt.Errorf("invalid UTF-8")
	}
	index := 0
	skipSpace := func() {
		for index < len(profile) && strings.ContainsRune(" \t\r\n", rune(profile[index])) {
			index++
		}
	}
	var expression func() (any, error)
	expression = func() (any, error) {
		skipSpace()
		if index >= len(profile) {
			return nil, fmt.Errorf("unexpected end of profile")
		}
		switch profile[index] {
		case '(':
			index++
			var list []any
			for {
				skipSpace()
				if index >= len(profile) {
					return nil, fmt.Errorf("unterminated list")
				}
				if profile[index] == ')' {
					index++
					return list, nil
				}
				value, err := expression()
				if err != nil {
					return nil, err
				}
				list = append(list, value)
			}
		case ')':
			return nil, fmt.Errorf("unexpected closing parenthesis")
		case '"':
			start := index
			index++
			for index < len(profile) {
				switch profile[index] {
				case '\\':
					index += 2
					continue
				case '"':
					index++
					literal, err := strconv.Unquote(profile[start:index])
					return sandboxTestLiteral(literal), err
				}
				index++
			}
			return nil, fmt.Errorf("unterminated string literal")
		default:
			start := index
			for index < len(profile) && !strings.ContainsRune(" \t\r\n()\"", rune(profile[index])) {
				index++
			}
			return profile[start:index], nil
		}
	}
	var forms []any
	for {
		skipSpace()
		if index == len(profile) {
			return forms, nil
		}
		form, err := expression()
		if err != nil {
			return nil, err
		}
		forms = append(forms, form)
	}
}

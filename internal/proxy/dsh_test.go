package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func configureDSH(t *testing.T, account, real string) string {
	t.Helper()
	dir := filepath.Join(account, ".codex-proxy")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeConfigFixture(t, dir, "export DSH_PROXY_REAL_BIN="+strconv.Quote(real)+" # real executable\n")
	return dir
}

func TestResolveDSHBinaryRejectsWrapperAliases(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	wrapper, realDir := filepath.Join(base, "wrapper"), filepath.Join(base, "real")
	for _, dir := range []string{wrapper, realDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(self, filepath.Join(wrapper, "dsh")); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(realDir, "dsh")
	if err := os.WriteFile(real, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{wrapper, realDir}, string(os.PathListSeparator)))
	got, err := resolveDSHBinary(real, self)
	if err != nil || got != real {
		t.Fatalf("resolveDSHBinary(%q) = %q, %v; want %q", real, got, err, real)
	}
	for _, configured := range []string{"", "dsh", "./real/dsh", self, filepath.Join(wrapper, "dsh"), filepath.Join(base, "missing")} {
		if got, err := resolveDSHBinary(configured, self); err == nil || got != "" {
			t.Fatalf("invalid binary %q accepted or looked up in PATH: %q, %v", configured, got, err)
		}
	}
	// An explicitly configured executable works independently of PATH.
	t.Setenv("PATH", "")
	if got, err := resolveDSHBinary(real, self); err != nil || got != real {
		t.Fatalf("explicit path required PATH: %q, %v", got, err)
	}
	t.Run("hardlink", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dsh")
		if err := os.Link(self, path); err != nil {
			if errors.Is(err, syscall.EXDEV) {
				t.Skip("hard-link check requires the executable and temp directory on the same filesystem")
			}
			t.Fatal(err)
		}
		if got, err := resolveDSHBinary(path, self); err == nil || got != "" {
			t.Fatalf("wrapper hard link accepted: %q, %v", got, err)
		}
	})
}

func TestRunDSHRequiresRealBinBeforeExec(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	blocked, directory, realDir := filepath.Join(base, "blocked"), filepath.Join(base, "directory"), filepath.Join(base, "real")
	for _, dir := range []string{blocked, directory, realDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(blocked, "dsh"), []byte("not executable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "dsh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "dsh"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{blocked, directory, realDir}, string(os.PathListSeparator)))
	for _, configured := range []string{filepath.Join(blocked, "dsh"), filepath.Join(directory, "dsh")} {
		if got, err := resolveDSHBinary(configured, self); err == nil || got != "" {
			t.Fatalf("invalid executable fell back to PATH: %q, %v", got, err)
		}
	}
	// A valid process setting and PATH entry must not replace the required file setting.
	t.Setenv("DSH_PROXY_REAL_BIN", filepath.Join(realDir, "dsh"))
	for _, goos := range []string{"darwin", "linux"} {
		for _, tc := range []struct {
			name, content, diagnostic string
		}{
			{"missing file", "", "read config"},
			{"missing key", "# no dsh setting\n", "DSH_PROXY_REAL_BIN is required"},
			{"empty key", "DSH_PROXY_REAL_BIN=\n", "DSH_PROXY_REAL_BIN is required"},
			{"relative path", "DSH_PROXY_REAL_BIN=dsh\n", "absolute executable path"},
			{"invalid syntax", "DSH_PROXY_REAL_BIN\n", "expected KEY=value"},
			{"duplicate key", "DSH_PROXY_REAL_BIN=\nDSH_PROXY_REAL_BIN=\n", "duplicate key"},
		} {
			t.Run(goos+"/"+tc.name, func(t *testing.T) {
				account := t.TempDir()
				dir := filepath.Join(account, ".codex-proxy")
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if tc.name != "missing file" {
					writeConfigFixture(t, dir, tc.content)
				}
				var stderr bytes.Buffer
				code := runDSH(nil, dshOptions{goos: goos, stderr: &stderr,
					accountHome: func() (string, error) { return account, nil },
					exec:        func(string, []string, []string) error { t.Fatal("missing real bin executed a process"); return nil },
				})
				if code != 2 || !strings.Contains(stderr.String(), tc.diagnostic) {
					t.Fatalf("invalid file setting was accepted: code=%d stderr=%s", code, stderr.String())
				}
			})
		}
	}
}

func TestDSHSandboxUsesSamePolicyAndAdministratorRules(t *testing.T) {
	base := t.TempDir()
	user := filepath.Join(base, `shared-"quoted"`, "alice")
	account := filepath.Join(base, "system-account")
	dir := filepath.Join(account, ".codex-proxy")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	rules := "(deny network-outbound) ; trailing comment"
	if err := os.WriteFile(filepath.Join(dir, sandboxRulesFilename), []byte(rules), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(base, "ignored-home"))
	t.Setenv("CODEX_HOME", filepath.Join(base, "other-shared", "bob", ".codex"))
	want, err := sandboxLaunch(filepath.Join(user, ".codex"), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, home := range []string{filepath.Join(user, ".dsh"), filepath.Join(user, "custom-dsh-home") + string(os.PathSeparator)} {
		got, err := dshSandboxProfile(home, dir)
		if err != nil || got != want.profile {
			t.Fatalf("dsh policy differs from Codex: %v\ngot: %s\nwant: %s", err, got, want.profile)
		}
		assertProxyDirectoryDeny(t, got, canonicalSandboxTestPath(t, dir), dir)
	}
	if _, err := os.Stat(user); !os.IsNotExist(err) {
		t.Fatalf("profile generation created DSH_HOME or its parents: %v", err)
	}
}

func TestRunDSHExecPreservesArgumentsAndEnvironment(t *testing.T) {
	real := fakeCodex(t)
	args := []string{"--help", "--sandbox", "read-only", "", "--", "提示词 with spaces", `quote"\\$()`, "--proxy-app-server-guard"}
	for _, test := range []struct {
		name, goos string
		sandboxed  bool
	}{
		{"darwin", "darwin", false},
		{"darwin inherited sandbox", "darwin", true},
		{"linux", "linux", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			account := filepath.Join(base, "account")
			dir := filepath.Join(account, ".codex-proxy")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			// Codex's binary and typed settings do not participate in dsh validation.
			writeConfigFixture(t, dir, "DSH_PROXY_REAL_BIN="+strconv.Quote(real)+"\n"+
				"CODEX_PROXY_REAL_BIN=not-required-for-dsh\nCODEX_PROXY_MAX_CONCURRENCY=invalid-for-codex\n")
			t.Setenv("DSH_PROXY_REAL_BIN", filepath.Join(base, "ignored-runtime-dsh"))
			t.Setenv("DSH_HOME", filepath.Join(base, "shared", "alice", ".dsh"))
			if test.sandboxed {
				// No new profile or administrator rules are read in an inherited sandbox.
				t.Setenv("DSH_HOME", "")
				if err := os.Symlink(filepath.Join(base, "missing.sb"), filepath.Join(dir, sandboxRulesFilename)); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CODEX_HOME", "invalid-and-ignored")
			var stderr bytes.Buffer
			calls := 0
			opts := dshOptions{goos: test.goos, stderr: &stderr, sandboxed: func() (bool, error) {
				if test.goos != "darwin" {
					t.Fatal("Linux must not query the macOS sandbox")
				}
				return test.sandboxed, nil
			}, accountHome: func() (string, error) {
				return account, nil
			}, exec: func(path string, argv, env []string) error {
				calls++
				wantPath := real
				wantArgs := append([]string{real}, args...)
				if test.goos == "darwin" && !test.sandboxed {
					wantPath = "/usr/bin/sandbox-exec"
					if len(argv) < 4 || argv[1] != "-p" || argv[2] == "" {
						t.Fatalf("missing sandbox profile: %q", argv)
					}
					assertProxyDirectoryDeny(t, argv[2], canonicalSandboxTestPath(t, dir), dir)
					wantArgs = append([]string{wantPath, "-p", argv[2], real}, args...)
				}
				if path != wantPath || !reflect.DeepEqual(argv, wantArgs) || !reflect.DeepEqual(env, os.Environ()) {
					t.Fatalf("exec changed invocation: path=%q argv=%q env=%v", path, argv, reflect.DeepEqual(env, os.Environ()))
				}
				return errors.New("test exec failure")
			}}
			if code := runDSH(args, opts); code != 2 || calls != 1 || !strings.Contains(stderr.String(), "test exec failure") {
				t.Fatalf("exec failure was lost or retried: code=%d calls=%d stderr=%s", code, calls, stderr.String())
			}
			entries, err := os.ReadDir(dir)
			wantEntries := 1
			if test.sandboxed {
				wantEntries++
			}
			if err != nil || len(entries) != wantEntries || entries[0].Name() != "env" {
				t.Fatalf("dsh created proxy state or logs: %v, %v", entries, err)
			}
		})
	}
}

func TestRunDSHRejectsInvalidSandboxBeforeExec(t *testing.T) {
	real := fakeCodex(t)
	base := t.TempDir()
	account := filepath.Join(base, "account")
	dir := configureDSH(t, account, real)
	t.Setenv("DSH_PROXY_REAL_BIN", "")
	for _, home := range []string{"", "relative/alice/.dsh", "/alice/.dsh", "/", filepath.Join(base, "shared", "alice\n", ".dsh")} {
		t.Run(strconv.Quote(home), func(t *testing.T) {
			t.Setenv("DSH_HOME", home)
			var stderr bytes.Buffer
			code := runDSH(nil, dshOptions{goos: "darwin", stderr: &stderr,
				sandboxed:   func() (bool, error) { return false, nil },
				accountHome: func() (string, error) { return account, nil },
				exec:        func(string, []string, []string) error { t.Fatal("invalid DSH_HOME executed a process"); return nil },
			})
			if code != 2 || !strings.Contains(stderr.String(), "DSH_HOME") {
				t.Fatalf("missing DSH_HOME diagnostic: code=%d stderr=%s", code, stderr.String())
			}
		})
	}
	// Administrator rules use the same no-symlink validation as Codex.
	t.Setenv("DSH_HOME", filepath.Join(base, "shared", "alice", ".dsh"))
	if err := os.Symlink(filepath.Join(base, "missing.sb"), filepath.Join(dir, sandboxRulesFilename)); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code := runDSH(nil, dshOptions{goos: "darwin", stderr: &stderr,
		sandboxed:   func() (bool, error) { return false, nil },
		accountHome: func() (string, error) { return account, nil },
		exec:        func(string, []string, []string) error { t.Fatal("invalid rules executed a process"); return nil },
	})
	if code != 2 || !strings.Contains(stderr.String(), sandboxRulesFilename) {
		t.Fatalf("missing rules diagnostic: code=%d stderr=%s", code, stderr.String())
	}
}

func TestRunDSHHelperProcess(t *testing.T) {
	if os.Getenv("DSH_PROXY_TEST_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Exit(runDSH(os.Args[i+1:], dshOptions{accountHome: func() (string, error) {
				return os.Getenv("DSH_PROXY_TEST_ACCOUNT"), nil
			}}))
		}
	}
	t.Fatal("missing helper argument separator")
}

func TestRunDSHReplacesProcessAndPreservesStdio(t *testing.T) {
	if !platformSandboxAvailable() {
		t.Skip("sandbox-exec unavailable: sandbox_apply rejected by this environment")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	real := fakeCodex(t)
	for _, viaSymlink := range []bool{false, true} {
		t.Run("symlink="+strconv.FormatBool(viaSymlink), func(t *testing.T) {
			base := t.TempDir()
			markers, wrapperDir, realDir := filepath.Join(base, "markers"), filepath.Join(base, "wrapper"), filepath.Join(base, "real")
			for _, dir := range []string{markers, wrapperDir, realDir} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(self, filepath.Join(wrapperDir, "dsh")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, filepath.Join(realDir, "dsh")); err != nil {
				t.Fatal(err)
			}
			args := []string{"--future-option", "", "--", "原样传递 with spaces", "--sandbox=read-only"}
			cmd := exec.Command(self, append([]string{"-test.run=^TestRunDSHHelperProcess$", "--"}, args...)...)
			configured := real
			if viaSymlink {
				configured = filepath.Join(realDir, "dsh")
			}
			dir := configureDSH(t, base, configured)
			cmd.Env = append(os.Environ(), "DSH_PROXY_TEST_PROCESS=1", "DSH_PROXY_TEST_ACCOUNT="+base,
				"DSH_PROXY_REAL_BIN="+self, "PATH="+wrapperDir+string(os.PathListSeparator)+realDir,
				"DSH_HOME="+filepath.Join(base, "shared", "alice", ".dsh"), "CODEX_HOME=ignored",
				"HOME="+filepath.Join(base, "runtime-home"), "USER=dsh-test-user",
				"FAKE_CODEX_MODE=echo", "FAKE_CODEX_DIR="+markers, "FAKE_CODEX_EXIT=37")
			cmd.Dir = base
			cmd.Stdin = strings.NewReader("stdin through exec\n")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 37 {
				t.Fatalf("dsh exit=%v stderr=%s", err, stderr.String())
			}
			var output struct {
				Args  []string `json:"args"`
				Input string   `json:"stdin"`
				Home  string   `json:"home"`
				User  string   `json:"user"`
				Cwd   string   `json:"cwd"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatalf("invalid stdout %q: %v", stdout.String(), err)
			}
			if !reflect.DeepEqual(output.Args, args) || output.Input != "stdin through exec\n" || output.Home != filepath.Join(base, "runtime-home") || output.User != "dsh-test-user" || output.Cwd != canonicalSandboxTestPath(t, base) {
				t.Fatalf("dsh changed its inherited invocation: %+v", output)
			}
			if stderr.String() != "fake-codex-stderr\n" {
				t.Fatalf("stderr=%q", stderr.String())
			}
			if pids := startedPIDs(t, markers); !reflect.DeepEqual(pids, []string{strconv.Itoa(cmd.Process.Pid)}) {
				t.Fatalf("dsh spawned a child instead of replacing itself: %v, want %d", pids, cmd.Process.Pid)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "env" {
				t.Fatalf("dsh created state or logs next to its configuration: %v, %v", entries, err)
			}
		})
	}
}

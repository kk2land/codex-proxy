package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func configurePI(t *testing.T, account, real string) string {
	t.Helper()
	dir := filepath.Join(account, ".codex-proxy")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeConfigFixture(t, dir, "export PI_PROXY_REAL_BIN="+strconv.Quote(real)+" # real executable\n")
	return dir
}

func TestResolvePIBinaryRejectsWrapperAliases(t *testing.T) {
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
	if err := os.Symlink(self, filepath.Join(wrapper, "pi")); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(realDir, "pi")
	if err := os.WriteFile(real, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{wrapper, realDir}, string(os.PathListSeparator)))
	got, err := resolvePIBinary(real, self)
	if err != nil || got != real {
		t.Fatalf("resolvePIBinary(%q) = %q, %v; want %q", real, got, err, real)
	}
	for _, configured := range []string{"", "pi", "./real/pi", self, filepath.Join(wrapper, "pi"), filepath.Join(base, "missing")} {
		if got, err := resolvePIBinary(configured, self); err == nil || got != "" {
			t.Fatalf("invalid binary %q accepted or looked up in PATH: %q, %v", configured, got, err)
		}
	}
	// An explicitly configured executable works independently of PATH.
	t.Setenv("PATH", "")
	if got, err := resolvePIBinary(real, self); err != nil || got != real {
		t.Fatalf("explicit path required PATH: %q, %v", got, err)
	}
	t.Run("hardlink", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pi")
		if err := os.Link(self, path); err != nil {
			if errors.Is(err, syscall.EXDEV) {
				t.Skip("hard-link check requires the executable and temp directory on the same filesystem")
			}
			t.Fatal(err)
		}
		if got, err := resolvePIBinary(path, self); err == nil || got != "" {
			t.Fatalf("wrapper hard link accepted: %q, %v", got, err)
		}
	})
}

func TestRunPIRequiresRealBinBeforeExec(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(blocked, "pi"), []byte("not executable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "pi"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "pi"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{blocked, directory, realDir}, string(os.PathListSeparator)))
	for _, configured := range []string{filepath.Join(blocked, "pi"), filepath.Join(directory, "pi")} {
		if got, err := resolvePIBinary(configured, self); err == nil || got != "" {
			t.Fatalf("invalid executable fell back to PATH: %q, %v", got, err)
		}
	}
	// A valid process setting and PATH entry must not replace the required file setting.
	t.Setenv("PI_PROXY_REAL_BIN", filepath.Join(realDir, "pi"))
	for _, goos := range []string{"darwin", "linux"} {
		for _, tc := range []struct {
			name, content, diagnostic string
		}{
			{"missing file", "", "read config"},
			{"missing key", "# no pi setting\n", "PI_PROXY_REAL_BIN is required"},
			{"empty key", "PI_PROXY_REAL_BIN=\n", "PI_PROXY_REAL_BIN is required"},
			{"relative path", "PI_PROXY_REAL_BIN=pi\n", "absolute executable path"},
			{"invalid syntax", "PI_PROXY_REAL_BIN\n", "expected KEY=value"},
			{"duplicate key", "PI_PROXY_REAL_BIN=\nPI_PROXY_REAL_BIN=\n", "duplicate key"},
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
				code := runPI(nil, piOptions{goos: goos, stderr: &stderr,
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

func TestPISandboxUsesSamePolicyAndAdministratorRules(t *testing.T) {
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
	t.Setenv("DSH_HOME", filepath.Join(base, "other-shared", "bob", ".dsh"))
	want, err := sandboxLaunch(filepath.Join(user, ".codex"), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, home := range []string{filepath.Join(user, ".pi", "agent"), filepath.Join(user, ".pi", "agent") + string(os.PathSeparator)} {
		got, err := piSandboxProfile(home, dir)
		if err != nil || got != want.profile {
			t.Fatalf("pi policy differs from Codex: %v\ngot: %s\nwant: %s", err, got, want.profile)
		}
		assertProxyDirectoryDeny(t, got, canonicalSandboxTestPath(t, dir), dir)
	}
	if _, err := os.Stat(user); !os.IsNotExist(err) {
		t.Fatalf("profile generation created PI_CODING_AGENT_DIR or its parents: %v", err)
	}
}

func TestRunPIExecPreservesArgumentsAndEnvironment(t *testing.T) {
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
			// Other launchers' binary and typed settings do not participate in pi validation.
			writeConfigFixture(t, dir, "PI_PROXY_REAL_BIN="+strconv.Quote(real)+"\n"+
				"DSH_PROXY_REAL_BIN=not-required-for-pi\nCODEX_PROXY_REAL_BIN=not-required-for-pi\nCODEX_PROXY_MAX_CONCURRENCY=invalid-for-codex\n")
			t.Setenv("PI_PROXY_REAL_BIN", filepath.Join(base, "ignored-runtime-pi"))
			t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(base, "shared", "alice", ".pi", "agent"))
			if test.goos == "linux" || test.sandboxed {
				t.Setenv("PI_CODING_AGENT_DIR", "")
			}
			if test.sandboxed {
				// No new profile or administrator rules are read in an inherited sandbox.
				if err := os.Symlink(filepath.Join(base, "missing.sb"), filepath.Join(dir, sandboxRulesFilename)); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CODEX_HOME", "invalid-and-ignored")
			t.Setenv("DSH_HOME", "invalid-and-ignored")
			var stderr bytes.Buffer
			calls := 0
			opts := piOptions{goos: test.goos, stderr: &stderr, sandboxed: func() (bool, error) {
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
			if code := runPI(args, opts); code != 2 || calls != 1 || !strings.Contains(stderr.String(), "test exec failure") {
				t.Fatalf("exec failure was lost or retried: code=%d calls=%d stderr=%s", code, calls, stderr.String())
			}
			entries, err := os.ReadDir(dir)
			wantEntries := 1
			if test.sandboxed {
				wantEntries++
			}
			if err != nil || len(entries) != wantEntries || entries[0].Name() != "env" {
				t.Fatalf("pi created proxy state or logs: %v, %v", entries, err)
			}
		})
	}
}

func TestRunPIRejectsInvalidSandboxBeforeExec(t *testing.T) {
	real := fakeCodex(t)
	base := t.TempDir()
	account := filepath.Join(base, "account")
	dir := configurePI(t, account, real)
	t.Setenv("PI_PROXY_REAL_BIN", "")
	for _, home := range []string{
		"", "relative/alice/.pi/agent", "/alice/.pi/agent", "/.pi/agent", "/",
		filepath.Join(base, "shared", "alice", ".pi"),
		filepath.Join(base, "shared", "alice", ".pi", "wrong"),
		filepath.Join(base, "shared", "alice", "other", "agent"),
		filepath.Join(base, "shared", "alice\n", ".pi", "agent"),
	} {
		t.Run(strconv.Quote(home), func(t *testing.T) {
			t.Setenv("PI_CODING_AGENT_DIR", home)
			var stderr bytes.Buffer
			code := runPI(nil, piOptions{goos: "darwin", stderr: &stderr,
				sandboxed:   func() (bool, error) { return false, nil },
				accountHome: func() (string, error) { return account, nil },
				exec: func(string, []string, []string) error {
					t.Fatal("invalid PI_CODING_AGENT_DIR executed a process")
					return nil
				},
			})
			if code != 2 || !strings.Contains(stderr.String(), "PI_CODING_AGENT_DIR") {
				t.Fatalf("missing PI_CODING_AGENT_DIR diagnostic: code=%d stderr=%s", code, stderr.String())
			}
		})
	}
	// Administrator rules use the same no-symlink validation as Codex.
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(base, "shared", "alice", ".pi", "agent"))
	if err := os.Symlink(filepath.Join(base, "missing.sb"), filepath.Join(dir, sandboxRulesFilename)); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code := runPI(nil, piOptions{goos: "darwin", stderr: &stderr,
		sandboxed:   func() (bool, error) { return false, nil },
		accountHome: func() (string, error) { return account, nil },
		exec:        func(string, []string, []string) error { t.Fatal("invalid rules executed a process"); return nil },
	})
	if code != 2 || !strings.Contains(stderr.String(), sandboxRulesFilename) {
		t.Fatalf("missing rules diagnostic: code=%d stderr=%s", code, stderr.String())
	}
}

func TestRunPIHelperProcess(t *testing.T) {
	if os.Getenv("PI_PROXY_TEST_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Exit(runPI(os.Args[i+1:], piOptions{
				goos:        os.Getenv("PI_PROXY_TEST_GOOS"),
				accountHome: func() (string, error) { return os.Getenv("PI_PROXY_TEST_ACCOUNT"), nil },
			}))
		}
	}
	t.Fatal("missing helper argument separator")
}

func TestRunPIReplacesProcessAndPreservesStdio(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	real := fakeCodex(t)
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			if goos == "darwin" && (runtime.GOOS != "darwin" || !platformSandboxAvailable()) {
				t.Skip("sandbox-exec unavailable on this platform or rejected by this environment")
			}
			for _, viaSymlink := range []bool{false, true} {
				t.Run("symlink="+strconv.FormatBool(viaSymlink), func(t *testing.T) {
					base := t.TempDir()
					markers, wrapperDir, realDir := filepath.Join(base, "markers"), filepath.Join(base, "wrapper"), filepath.Join(base, "real")
					for _, dir := range []string{markers, wrapperDir, realDir} {
						if err := os.Mkdir(dir, 0700); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.Symlink(self, filepath.Join(wrapperDir, "pi")); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(real, filepath.Join(realDir, "pi")); err != nil {
						t.Fatal(err)
					}
					args := []string{"--future-option", "", "--", "原样传递 with spaces", "--sandbox=read-only"}
					cmd := exec.Command(self, append([]string{"-test.run=^TestRunPIHelperProcess$", "--"}, args...)...)
					configured := real
					if viaSymlink {
						configured = filepath.Join(realDir, "pi")
					}
					dir := configurePI(t, base, configured)
					cmd.Env = append(os.Environ(), "PI_PROXY_TEST_PROCESS=1", "PI_PROXY_TEST_ACCOUNT="+base, "PI_PROXY_TEST_GOOS="+goos,
						"PI_PROXY_REAL_BIN="+self, "PATH="+wrapperDir+string(os.PathListSeparator)+realDir,
						"PI_CODING_AGENT_DIR="+filepath.Join(base, "shared", "alice", ".pi", "agent"), "CODEX_HOME=ignored",
						"HOME="+filepath.Join(base, "runtime-home"), "USER=pi-test-user",
						"FAKE_CODEX_MODE=echo", "FAKE_CODEX_DIR="+markers, "FAKE_CODEX_EXIT=37")
					cmd.Dir = base
					cmd.Stdin = strings.NewReader("stdin through exec\n")
					var stdout, stderr bytes.Buffer
					cmd.Stdout, cmd.Stderr = &stdout, &stderr
					if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 37 {
						t.Fatalf("pi exit=%v stderr=%s", err, stderr.String())
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
					if !reflect.DeepEqual(output.Args, args) || output.Input != "stdin through exec\n" || output.Home != filepath.Join(base, "runtime-home") || output.User != "pi-test-user" || output.Cwd != canonicalSandboxTestPath(t, base) {
						t.Fatalf("pi changed its inherited invocation: %+v", output)
					}
					if stderr.String() != "fake-codex-stderr\n" {
						t.Fatalf("stderr=%q", stderr.String())
					}
					if pids := startedPIDs(t, markers); !reflect.DeepEqual(pids, []string{strconv.Itoa(cmd.Process.Pid)}) {
						t.Fatalf("pi spawned a child instead of replacing itself: %v, want %d", pids, cmd.Process.Pid)
					}
					entries, err := os.ReadDir(dir)
					if err != nil || len(entries) != 1 || entries[0].Name() != "env" {
						t.Fatalf("pi created state or logs next to its configuration: %v, %v", entries, err)
					}
				})
			}
		})
	}
}

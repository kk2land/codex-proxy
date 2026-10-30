package proxy

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareSandboxLaunchInheritsExistingSandbox(t *testing.T) {
	args := []string{"exec", "--dangerously-bypass-approvals-and-sandbox", "", "--", "prompt with spaces"}
	// Invalid paths prove that an inherited sandbox does not regenerate rules.
	spec, err := prepareSandboxLaunch("invalid-home", "invalid-proxy-dir", args, func() (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	cmd := spec.command("/real/codex")
	if spec.profile != "" || spec.profileSuffix != "" || cmd.Path != "/real/codex" || !reflect.DeepEqual(cmd.Args, append([]string{"/real/codex"}, args...)) {
		t.Fatalf("unexpected inherited launch: %+v, %q", spec, cmd.Args)
	}
}

func TestPrepareSandboxLaunchCreatesSandboxWhenNeeded(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "shared", "alice", ".codex")
	dir := filepath.Join(base, ".codex-proxy")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	spec, err := prepareSandboxLaunch(home, dir, []string{"exec", "prompt"}, func() (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	cmd := spec.command("/real/codex")
	if cmd.Path != "/usr/bin/sandbox-exec" || spec.profile == "" {
		t.Fatalf("missing sandbox: %+v, %q", spec, cmd.Args)
	}
	assertProxyDirectoryDeny(t, spec.profile, canonicalSandboxTestPath(t, dir), dir)
}

func TestSandboxDetectionFailureRejectsLaunch(t *testing.T) {
	cause := errors.New("sandbox status unavailable")
	spec, err := prepareSandboxLaunch("", "", nil, func() (bool, error) { return true, cause })
	if !errors.Is(err, cause) || spec.profile != "" || spec.args != nil {
		t.Fatalf("sandbox check failure ignored: %+v, %v", spec, err)
	}
	for _, agent := range []string{"pi", "dsh"} {
		t.Run(agent, func(t *testing.T) {
			account := t.TempDir()
			var stderr bytes.Buffer
			calls := 0
			check := func() (bool, error) { calls++; return true, cause }
			execute := func(string, []string, []string) error { t.Fatal("executed after failed sandbox check"); return nil }
			var code int
			if agent == "pi" {
				configurePI(t, account, fakeCodex(t))
				code = runPI(nil, piOptions{goos: "darwin", sandboxed: check, stderr: &stderr, exec: execute,
					accountHome: func() (string, error) { return account, nil }})
			} else {
				configureDSH(t, account, fakeCodex(t))
				code = runDSH(nil, dshOptions{goos: "darwin", sandboxed: check, stderr: &stderr, exec: execute,
					accountHome: func() (string, error) { return account, nil }})
			}
			if code != 2 || calls != 1 || !strings.Contains(stderr.String(), cause.Error()) {
				t.Fatalf("sandbox check failure ignored: code=%d checks=%d stderr=%s", code, calls, stderr.String())
			}
		})
	}
}

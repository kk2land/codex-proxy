//go:build darwin && cgo

package proxy

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestDarwinInheritedSandboxHelperProcess(t *testing.T) {
	if os.Getenv("CODEX_PROXY_TEST_INHERITED_SANDBOX") != "1" {
		return
	}
	sandboxed, err := processSandboxed()
	if err != nil || !sandboxed {
		t.Fatalf("inherited sandbox not detected: %v, %v", sandboxed, err)
	}
	// Missing CODEX_HOME and proxy rules must not prevent using an existing sandbox.
	t.Setenv("CODEX_HOME", "")
	for _, kind := range []CommandKind{CommandExec, CommandAppServer} {
		var spec launchSpec
		if kind == CommandExec {
			spec, err = prepareLaunch(kind, []string{"exec", "--sandbox", "read-only", "prompt"}, "invalid-dir")
		} else {
			spec, err = prepareAppServerLaunch(appServerStartup{Args: []string{"app-server"}}, "invalid-dir")
		}
		if err != nil || spec.profile != "" || spec.command("/real/codex").Path != "/real/codex" {
			t.Fatalf("inherited launch failed: %+v, %v", spec, err)
		}
		args := strings.Join(spec.args, " ")
		if kind == CommandExec && (!strings.Contains(args, "--dangerously-bypass-approvals-and-sandbox") || strings.Contains(args, "--sandbox read-only")) {
			t.Fatalf("Codex internal sandbox was not disabled: %q", spec.args)
		}
		if kind == CommandAppServer && (!strings.Contains(args, `sandbox_mode="danger-full-access"`) || !strings.Contains(args, `approval_policy="never"`)) {
			t.Fatalf("app-server controls were lost: %q", spec.args)
		}
	}
}

func TestDarwinProcessDetectsInheritedSandbox(t *testing.T) {
	sandboxed, err := processSandboxed()
	if err != nil {
		t.Fatal(err)
	}
	if sandboxed || !platformSandboxAvailable() {
		t.Skip("cannot create a new sandbox in this environment")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/sandbox-exec", "-p", "(version 1)(allow default)", self, "-test.run=^TestDarwinInheritedSandboxHelperProcess$")
	cmd.Env = append(os.Environ(), "CODEX_PROXY_TEST_INHERITED_SANDBOX=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("inherited sandbox launch: %v\n%s", err, output)
	}
}

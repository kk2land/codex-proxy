package proxy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAppServerLaunchTransportAndTooling(t *testing.T) {
	clientInfo := `{"name":"startup-client","version":"1"}`
	for _, test := range []struct {
		args     []string
		mode     appServerLaunchMode
		rejected bool
	}{
		{[]string{"app-server"}, appServerStdio, false},
		{[]string{"app-server", "--stdio"}, appServerStdio, false},
		{[]string{"app-server", "--listen=stdio://"}, appServerStdio, false},
		{[]string{"app-server", "--ws-issuer", "--help"}, appServerStdio, false},
		{[]string{"app-server", "--help"}, appServerTooling, false},
		{[]string{"app-server", "generate-ts", "--out", "/tmp/types"}, appServerTooling, false},
		{[]string{"app-server", "--print-startup-config"}, appServerTooling, false},
		{[]string{"app-server", "--print-startup-config", "--startup-client-info", clientInfo}, appServerTooling, false},
		{[]string{"app-server", "--print-startup-config", "--startup-client-info=" + clientInfo}, appServerTooling, false},
		{[]string{"app-server", "--print-startup-config", "--cloud-skills-snapshot", "/tmp/snapshot.json"}, appServerTooling, false},
		{[]string{"app-server", "--print-startup-config", "--cloud-skills-snapshot=/tmp/snapshot.json"}, appServerTooling, false},
		{[]string{"app-server", "--startup-client-info", clientInfo, "--print-startup-config"}, appServerTooling, false},
		{[]string{"app-server", "--cloud-skills-snapshot", "/tmp/snapshot.json", "--print-startup-config"}, appServerTooling, false},
		{[]string{"app-server", "--startup-client-info", clientInfo}, appServerStdio, false},
		{[]string{"app-server", "--startup-client-info=" + clientInfo}, appServerStdio, false},
		{[]string{"app-server", "--cloud-skills-snapshot", "/tmp/snapshot.json"}, appServerStdio, false},
		{[]string{"app-server", "--cloud-skills-snapshot=/tmp/snapshot.json"}, appServerStdio, false},
		// A path that happens to spell a native subcommand is still an option
		// value, not an app-server mode.
		{[]string{"app-server", "--cloud-skills-snapshot", "daemon"}, appServerStdio, false},
		{[]string{"app-server", "--cloud-skills-snapshot=daemon"}, appServerStdio, false},
		{[]string{"app-server", "--listen", "unix:///tmp/server.sock"}, appServerStdio, true},
		{[]string{"app-server", "--listen", "off"}, appServerStdio, true},
		{[]string{"app-server", "--listen"}, appServerStdio, true},
		{[]string{"app-server", "-c", "setting=value", "daemon"}, appServerStdio, true},
		{[]string{"app-server", "proxy", "--sock", "/tmp/server.sock"}, appServerNative, true},
	} {
		mode, err := appServerMode(test.args)
		if (err != nil) != test.rejected || (err == nil && mode != test.mode) {
			t.Errorf("appServerMode(%q) = %v, %v", test.args, mode, err)
		}
		if test.rejected {
			continue
		}
		args := appServerArgs(test.args)
		if mode == appServerTooling {
			if !reflect.DeepEqual(args, test.args) {
				t.Errorf("tooling arguments changed: %q", args)
			}
			continue
		}
		if !reflect.DeepEqual(args[:len(test.args)], test.args) {
			t.Errorf("app-server arguments were rewritten: %q", args)
		}
		listeners := 0
		for _, arg := range args {
			if arg == "--stdio" || arg == "--listen" || strings.HasPrefix(arg, "--listen=") {
				listeners++
			}
		}
		if listeners != 1 {
			t.Errorf("duplicate or missing listener: %q", args)
		}
	}
}

// Run must classify startup discovery as ordinary tooling, preserving every
// argument and injecting neither a listener nor thread execution policies.
func TestAppServerStartupConfigPassesThroughUnchanged(t *testing.T) {
	clientInfo := `{"name":"startup-client","version":"1"}`
	for _, test := range []struct {
		name string
		args []string
	}{
		{"separate values", []string{"app-server", "--print-startup-config", "--startup-client-info", clientInfo, "--cloud-skills-snapshot", "/tmp/snapshot.json"}},
		{"attached values", []string{"app-server", "--print-startup-config", "--startup-client-info=" + clientInfo, "--cloud-skills-snapshot=/tmp/snapshot.json"}},
		{"options before flag", []string{"app-server", "--startup-client-info", clientInfo, "--cloud-skills-snapshot", "/tmp/snapshot.json", "--print-startup-config"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, dir := t.TempDir(), t.TempDir()
			configureAppProxy(t, home, fakeCodex(t), 1, "5s")
			child := startProxy(t, home, "echo", dir, "", 0, test.args...)
			if code := childExit(t, child, 5*time.Second); code != 0 {
				t.Fatalf("exit=%d stderr=%s", code, child.stderr.String())
			}
			var output struct {
				Args []string
			}
			if err := json.Unmarshal(child.stdout.Bytes(), &output); err != nil {
				t.Fatalf("invalid stdout: %q: %v", child.stdout.String(), err)
			}
			if !reflect.DeepEqual(output.Args, test.args) {
				t.Fatalf("startup config arguments changed: %q, want %q", output.Args, test.args)
			}
		})
	}
}

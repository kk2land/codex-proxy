//go:build (darwin && !osusergo) || (linux && cgo && !osusergo)

package proxy

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"
)

func TestSystemHomeUsesAccountDatabaseDespiteEnvironment(t *testing.T) {
	account, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		t.Fatalf("lookup current UID: %v", err)
	}
	if !filepath.IsAbs(account.HomeDir) {
		t.Fatalf("system account home is not absolute: %q", account.HomeDir)
	}
	fakeHome := filepath.Join(t.TempDir(), "runtime-home")
	t.Setenv("HOME", fakeHome)
	t.Setenv("USER", "codex-proxy-not-the-current-user")
	t.Setenv("LOGNAME", "codex-proxy-not-the-current-user")
	got, err := SystemHome()
	if err != nil {
		t.Fatal(err)
	}
	if got != account.HomeDir {
		t.Fatalf("SystemHome = %q, want UID account home %q (runtime HOME=%q)", got, account.HomeDir, fakeHome)
	}
	if _, err := os.Stat(fakeHome); !os.IsNotExist(err) {
		t.Fatalf("SystemHome wrote to the runtime HOME directory: stat error %v", err)
	}
}

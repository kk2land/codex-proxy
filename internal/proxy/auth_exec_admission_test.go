package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// configureAuthProxySlots mirrors configureAuthProxy but pins the global slot
// count so a test can hold every slot from the parent process.
func configureAuthProxySlots(t *testing.T, home, authDir string, slots int) {
	t.Helper()
	configureProxy(t, home, fakeCodex(t), slots, "10m", false)
	path := filepath.Join(home, ".codex-proxy", "env")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "CODEX_PROXY_AUTH_DIR=%s\n", strconv.Quote(authDir)); err != nil {
		t.Fatal(err)
	}
}

// awaitLogEvent polls complete JSON Lines records until the named event shows
// up. A partially written trailing record is ignored instead of failing, so the
// helper is safe to use while another process is still appending.
func awaitLogEvent(t *testing.T, dir, event string) map[string]any {
	t.Helper()
	path := filepath.Join(dir, loggerFilename)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if end := bytes.LastIndexByte(data, '\n'); end >= 0 {
			for _, line := range bytes.Split(data[:end], []byte("\n")) {
				var record map[string]any
				if json.Unmarshal(line, &record) != nil {
					continue
				}
				if record["event"] == event {
					return record
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("event %q was not logged within deadline", event)
	return nil
}

// holdAllSlots reserves every global slot from the test process.
func holdAllSlots(t *testing.T, home string, slots int) *Lease {
	t.Helper()
	locksDir := filepath.Join(home, ".codex-proxy", "locks")
	if err := os.MkdirAll(locksDir, 0700); err != nil {
		t.Fatal(err)
	}
	lease, err := reserveSlot(context.Background(), locksDir, slots)
	if err != nil {
		t.Fatal(err)
	}
	if lease == nil {
		t.Fatalf("could not reserve %d slot(s)", slots)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Errorf("close test slot reservation: %v", err)
		}
	})
	return lease
}

// authHomeWritable reports whether no reader pins CODEX_HOME credentials.
func authHomeWritable(t *testing.T, p *authPool) bool {
	t.Helper()
	writer, err := p.homeLock()
	if err != nil {
		t.Fatal(err)
	}
	locked, err := tryAuthLock(context.Background(), writer, syscall.LOCK_EX)
	writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	return locked
}

// A process waiting for an exec slot has not started native Codex yet, so it
// must not pin CODEX_HOME credentials. Otherwise the exiting 429 owner cannot
// rotate to a healthy spare account and later retries keep failing with 75.
func TestAuthExecWaiterDoesNotPinAuthHome(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	home, nativeDir := t.TempDir(), t.TempDir()
	configureAuthProxySlots(t, home, p.dir, 1)
	p.home = filepath.Join(home, "sandbox-user", ".codex")
	p.store = newStateStore(filepath.Join(home, ".codex-proxy"), Config{}, 0, nil)
	// The exiting owner already cooled the active account, like a native 429.
	limit := defaultAuthLimit(time.Now())
	p.note("auth0_alice.json", limit)
	if err := p.record(context.Background(), "auth0_alice.json", limit); err != nil {
		t.Fatal(err)
	}
	if err := p.activate("auth0_alice.json"); err != nil {
		t.Fatal(err)
	}

	held := holdAllSlots(t, home, 1)
	child := startProxy(t, home, "echo", nativeDir, "", 0, "exec", "test")
	awaitLogEvent(t, filepath.Join(home, ".codex-proxy"), "waiting")
	if pids := startedPIDs(t, nativeDir); len(pids) != 0 {
		t.Fatalf("native started while queued: %v", pids)
	}
	if !authHomeWritable(t, p) {
		t.Fatal("queued exec still holds the authentication read lock")
	}
	if locked, name, err := p.rotate(); err != nil || !locked || name != "auth1_bob.json" {
		t.Fatalf("owner could not rotate past the queued exec: locked=%v name=%s err=%v", locked, name, err)
	}
	if linked, err := p.linkedAccount(); err != nil || linked != "auth1_bob.json" {
		t.Fatalf("rotation did not reach the healthy spare: %q %v", linked, err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if code := childExit(t, child, 5*time.Second); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, child.stderr.String())
	}
	if pids := startedPIDs(t, nativeDir); len(pids) != 1 {
		t.Fatalf("native starts=%v want 1", pids)
	}
	selected := awaitLogEvent(t, filepath.Join(home, ".codex-proxy"), "auth_selected")
	if selected["auth_file"] != "auth1_bob.json" {
		t.Fatalf("exec used %v, want the rotated account", selected["auth_file"])
	}
	if linked, err := p.linkedAccount(); err != nil || linked != "auth1_bob.json" {
		t.Fatalf("exec replaced the rotated account: %q %v", linked, err)
	}
}

// Cancelling a queued exec must exit with the signal status, leave native Codex
// unstarted, and leave no authentication reader behind.
func TestAuthExecWaiterCancelReleasesAuthHome(t *testing.T) {
	p := authFixture(t, "auth0_alice.json")
	home, nativeDir := t.TempDir(), t.TempDir()
	configureAuthProxySlots(t, home, p.dir, 1)
	p.home = filepath.Join(home, "sandbox-user", ".codex")

	held := holdAllSlots(t, home, 1)
	child := startProxy(t, home, "hold", nativeDir, "", 0, "exec", "test")
	awaitLogEvent(t, filepath.Join(home, ".codex-proxy"), "waiting")
	if !authHomeWritable(t, p) {
		t.Fatal("queued exec still holds the authentication read lock")
	}
	if err := child.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := childExit(t, child, 5*time.Second); code != 143 {
		t.Fatalf("exit=%d stderr=%s", code, child.stderr.String())
	}
	if pids := startedPIDs(t, nativeDir); len(pids) != 0 {
		t.Fatalf("canceled exec started native: %v", pids)
	}
	if !authHomeWritable(t, p) {
		t.Fatal("canceled exec left an authentication reader")
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	// The slot reservation must not have been leaked by the canceled waiter.
	next := holdAllSlots(t, home, 1)
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
}

package proxy

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func configNativeFixture(t *testing.T, dir, name string, magic uint32) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data := make([]byte, 64)
	binary.BigEndian.PutUint32(data, magic)
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func configSelf(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func writeConfigFixture(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigDefaultsAndFileOnly(t *testing.T) {
	dir := t.TempDir()
	real := configNativeFixture(t, dir, "native-codex", 0x7f454c46)
	// Every process setting is deliberately invalid: none may influence the
	// file's values or fill in an omitted setting.
	for _, key := range []string{
		"CODEX_PROXY_REAL_BIN", "CODEX_PROXY_MAX_CONCURRENCY", "CODEX_PROXY_WAIT_TIMEOUT",
		"CODEX_PROXY_LOG_MAX_SIZE_MIB", "CODEX_PROXY_LOG_MAX_BACKUPS", "CODEX_PROXY_APP_SERVER_ENABLED",
		"CODEX_PROXY_APP_SERVER_IDLE_TIMEOUT",
		"CODEX_PROXY_STATE_LOCK_PATH", "CODEX_PROXY_STATE_PATH",
	} {
		t.Setenv(key, "invalid-process-environment")
	}
	writeConfigFixture(t, dir, "CODEX_PROXY_REAL_BIN="+strconv.Quote(real)+"\n")
	cfg, err := LoadConfig(dir, configSelf(t))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		RealBin: real, MaxConcurrency: 1, WaitTimeout: 10 * time.Minute,
		AppServerIdleTimeout: 10 * time.Minute,
		LogMaxSize:           10 << 20, LogMaxBackups: 5,
	}
	if cfg != want {
		t.Fatalf("LoadConfig = %#v, want %#v", cfg, want)
	}
}

func TestLoadConfigQuotedExportAndComments(t *testing.T) {
	dir := t.TempDir()
	real := configNativeFixture(t, dir, "native #codex", 0xcffaedfe)
	writeConfigFixture(t, dir, "\n# Configuration\n"+
		" export\tCODEX_PROXY_REAL_BIN = "+strconv.Quote(real)+" # keep the hash in the path\n"+
		"export CODEX_PROXY_MAX_CONCURRENCY='3'\n"+
		"export CODEX_PROXY_APP_SERVER_ENABLED='true' # opt in\n"+
		"CODEX_PROXY_APP_SERVER_IDLE_TIMEOUT='0'\n"+
		"CODEX_PROXY_WAIT_TIMEOUT=\"0\"\n"+
		"CODEX_PROXY_LOG_MAX_SIZE_MIB=2 # MiB\n"+
		"CODEX_PROXY_LOG_MAX_BACKUPS = '0' # no backups\n")
	cfg, err := LoadConfig(dir, configSelf(t))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{RealBin: real, AppServerEnabled: true, MaxConcurrency: 3, WaitTimeout: 0, LogMaxSize: 2 << 20, LogMaxBackups: 0}
	if cfg != want {
		t.Fatalf("LoadConfig = %#v, want %#v", cfg, want)
	}
}

func TestLoadConfigReloadsFile(t *testing.T) {
	dir := t.TempDir()
	real := configNativeFixture(t, dir, "native-codex", 0x7f454c46)
	base := "CODEX_PROXY_REAL_BIN=" + strconv.Quote(real) + "\n"
	self := configSelf(t)
	writeConfigFixture(t, dir, base+"CODEX_PROXY_MAX_CONCURRENCY=1\nCODEX_PROXY_WAIT_TIMEOUT=2s\n")
	first, err := LoadConfig(dir, self)
	if err != nil {
		t.Fatal(err)
	}
	writeConfigFixture(t, dir, base+"CODEX_PROXY_MAX_CONCURRENCY=4\nCODEX_PROXY_WAIT_TIMEOUT=500ms\n")
	second, err := LoadConfig(dir, self)
	if err != nil {
		t.Fatal(err)
	}
	if first.MaxConcurrency != 1 || first.WaitTimeout != 2*time.Second ||
		second.MaxConcurrency != 4 || second.WaitTimeout != 500*time.Millisecond {
		t.Fatalf("file changes were not applied: first=%#v, second=%#v", first, second)
	}
}

func TestLoadConfigDoesNotExpandPath(t *testing.T) {
	dir := t.TempDir()
	real := configNativeFixture(t, dir, "native-$CODEX_PROXY_TEST_FILENAME", 0x7f454c46)
	t.Setenv("CODEX_PROXY_TEST_FILENAME", "expanded")
	writeConfigFixture(t, dir, "CODEX_PROXY_REAL_BIN="+strconv.Quote(real)+"\n")
	cfg, err := LoadConfig(dir, configSelf(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RealBin != real {
		t.Fatalf("path was expanded: got %q, want %q", cfg.RealBin, real)
	}
}

func TestLoadConfigStatePaths(t *testing.T) {
	dir := t.TempDir()
	real := configNativeFixture(t, dir, "native-codex", 0x7f454c46)
	lockPath := filepath.Join(dir, "custom lock")
	statePath := filepath.Join(dir, "custom state.json")
	for _, tc := range []struct {
		name, extra, lockPath, statePath string
	}{
		{"omitted", "", "", ""},
		{"empty", "CODEX_PROXY_STATE_LOCK_PATH=\nCODEX_PROXY_STATE_PATH=\"\"\n", "", ""},
		{"lock only", "CODEX_PROXY_STATE_LOCK_PATH=" + strconv.Quote(lockPath) + "\n", lockPath, ""},
		{"state only", "CODEX_PROXY_STATE_PATH=" + strconv.Quote(statePath) + "\n", "", statePath},
		{"both", "export CODEX_PROXY_STATE_LOCK_PATH=" + strconv.Quote(lockPath) + "\nCODEX_PROXY_STATE_PATH=" + strconv.Quote(statePath) + "\n", lockPath, statePath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeConfigFixture(t, dir, "CODEX_PROXY_REAL_BIN="+strconv.Quote(real)+"\n"+tc.extra)
			cfg, err := LoadConfig(dir, configSelf(t))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.StateLockPath != tc.lockPath || cfg.StatePath != tc.statePath {
				t.Fatalf("state paths = (%q, %q), want (%q, %q)", cfg.StateLockPath, cfg.StatePath, tc.lockPath, tc.statePath)
			}
		})
	}
}

func TestLoadConfigRejectsInvalidSettings(t *testing.T) {
	dir := t.TempDir()
	real := configNativeFixture(t, dir, "native-codex", 0x7f454c46)
	self := configSelf(t)
	base := "CODEX_PROXY_REAL_BIN=" + strconv.Quote(real) + "\n"
	tests := []struct {
		name, extra, wantError string
	}{
		{"unknown", "CODEX_PROXY_UNKNOWN=1\n", "unknown config key"},
		{"log format option removed", "CODEX_PROXY_LOG_FORMAT=json\n", "unknown config key"},
		{"relative state lock", "CODEX_PROXY_STATE_LOCK_PATH=state.lock\n", "absolute file path"},
		{"relative state file", "CODEX_PROXY_STATE_PATH=state.json\n", "absolute file path"},
		{"app server invalid boolean", "CODEX_PROXY_APP_SERVER_ENABLED=yes\n", "must be a boolean"},
		{"app server empty boolean", "CODEX_PROXY_APP_SERVER_ENABLED=\n", "must be a boolean"},
		{"duplicate", "CODEX_PROXY_MAX_CONCURRENCY=1\nCODEX_PROXY_MAX_CONCURRENCY=2\n", "duplicate key"},
		{"missing equals", "CODEX_PROXY_MAX_CONCURRENCY\n", "expected KEY=value"},
		{"empty key", "=3\n", "expected KEY=value"},
		{"bad key", "CODEX-PROXY-MAX-CONCURRENCY=3\n", "expected KEY=value"},
		{"unterminated double quote", "CODEX_PROXY_WAIT_TIMEOUT=\"1s\n", "unterminated quoted value"},
		{"unterminated single quote", "CODEX_PROXY_WAIT_TIMEOUT='1s\n", "unterminated quoted value"},
		{"trailing quoted text", "CODEX_PROXY_WAIT_TIMEOUT='1s' trailing\n", "unexpected text"},
		{"bad quoted escape", "CODEX_PROXY_WAIT_TIMEOUT=\"\\q\"\n", "invalid escape"},
		{"raw NUL", "CODEX_PROXY_WAIT_TIMEOUT=1s\x00\n", "NUL"},
		{"escaped NUL", "CODEX_PROXY_WAIT_TIMEOUT=\"1s\\x00\"\n", "NUL"},
		{"concurrency zero", "CODEX_PROXY_MAX_CONCURRENCY=0\n", "positive integer"},
		{"concurrency negative", "CODEX_PROXY_MAX_CONCURRENCY=-1\n", "positive integer"},
		{"concurrency fractional", "CODEX_PROXY_MAX_CONCURRENCY=1.5\n", "positive integer"},
		{"concurrency overflow", "CODEX_PROXY_MAX_CONCURRENCY=999999999999999999999999999\n", "positive integer"},
		{"timeout negative", "CODEX_PROXY_WAIT_TIMEOUT=-1s\n", "nonnegative duration"},
		{"timeout missing unit", "CODEX_PROXY_WAIT_TIMEOUT=10\n", "nonnegative duration"},
		{"timeout overflow", "CODEX_PROXY_WAIT_TIMEOUT=999999999999999999999h\n", "nonnegative duration"},
		{"idle negative", "CODEX_PROXY_APP_SERVER_IDLE_TIMEOUT=-1s\n", "nonnegative duration"},
		{"idle missing unit", "CODEX_PROXY_APP_SERVER_IDLE_TIMEOUT=10\n", "nonnegative duration"},
		{"idle overflow", "CODEX_PROXY_APP_SERVER_IDLE_TIMEOUT=999999999999999999999h\n", "nonnegative duration"},
		{"size zero", "CODEX_PROXY_LOG_MAX_SIZE_MIB=0\n", "positive integer MiB"},
		{"size negative", "CODEX_PROXY_LOG_MAX_SIZE_MIB=-1\n", "positive integer MiB"},
		{"size fractional", "CODEX_PROXY_LOG_MAX_SIZE_MIB=1.5\n", "positive integer MiB"},
		{"size shift overflow", "CODEX_PROXY_LOG_MAX_SIZE_MIB=9223372036854775807\n", "without overflow"},
		{"size parse overflow", "CODEX_PROXY_LOG_MAX_SIZE_MIB=999999999999999999999999999\n", "without overflow"},
		{"backups negative", "CODEX_PROXY_LOG_MAX_BACKUPS=-1\n", "nonnegative integer"},
		{"backups noninteger", "CODEX_PROXY_LOG_MAX_BACKUPS=five\n", "nonnegative integer"},
		{"backups overflow", "CODEX_PROXY_LOG_MAX_BACKUPS=999999999999999999999999999\n", "nonnegative integer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeConfigFixture(t, dir, base+tt.extra)
			_, err := LoadConfig(dir, self)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("LoadConfig error = %v, want text %q", err, tt.wantError)
			}
		})
	}
}

func TestLoadConfigRequiresEnvFileAndRealBinary(t *testing.T) {
	dir := t.TempDir()
	self := configSelf(t)
	t.Setenv("CODEX_PROXY_REAL_BIN", configNativeFixture(t, dir, "native-codex", 0x7f454c46))
	if _, err := LoadConfig(dir, self); err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("missing env error = %v", err)
	}
	for _, content := range []string{"# empty\n", "CODEX_PROXY_REAL_BIN=\n"} {
		writeConfigFixture(t, dir, content)
		if _, err := LoadConfig(dir, self); err == nil || !strings.Contains(err.Error(), "is required") {
			t.Fatalf("missing real binary error = %v", err)
		}
	}
}

func TestParseEnvValuesRemainLiteral(t *testing.T) {
	input := "export KEY='$(touch /not-created) $HOME ${USER} `id`'\n" +
		"DOUBLE=\"$HOME ${USER} $(id) `id`\" # comment\n" +
		"RAW=abc#hash # trimmed comment\n" +
		"ESCAPED=\"quote: \\\" slash: \\\\ newline: \\n\"\n" +
		"EMPTY= # comment\n"
	got, err := parseEnv(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"KEY":     "$(touch /not-created) $HOME ${USER} `id`",
		"DOUBLE":  "$HOME ${USER} $(id) `id`",
		"RAW":     "abc#hash",
		"ESCAPED": "quote: \" slash: \\ newline: \n",
		"EMPTY":   "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseEnv = %#v, want %#v", got, want)
	}
}

func TestParseEnvRejectsOversizedLine(t *testing.T) {
	if _, err := parseEnv(strings.NewReader("KEY=" + strings.Repeat("x", 70*1024))); err == nil {
		t.Fatal("oversized configuration line unexpectedly accepted")
	}
}

func TestLoadConfigAcceptsNativeBinaryHeaders(t *testing.T) {
	for _, format := range []struct {
		name  string
		magic uint32
	}{
		{"Linux ELF", 0x7f454c46},
		{"macOS Mach-O", 0xcffaedfe},
		{"macOS universal", 0xcafebabe},
	} {
		t.Run(format.name, func(t *testing.T) {
			dir := t.TempDir()
			real := configNativeFixture(t, dir, "native-codex", format.magic)
			writeConfigFixture(t, dir, "CODEX_PROXY_REAL_BIN="+strconv.Quote(real)+"\n")
			if _, err := LoadConfig(dir, configSelf(t)); err != nil {
				t.Fatalf("native %s binary rejected: %v", format.name, err)
			}
		})
	}
}

func TestLoadConfigRejectsInvalidBinaryPaths(t *testing.T) {
	dir := t.TempDir()
	self := configSelf(t)
	script := filepath.Join(dir, "launcher")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec codex \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	text := filepath.Join(dir, "launcher.js")
	if err := os.WriteFile(text, []byte("console.log('launcher');\n"), 0700); err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("ELF"), 0700); err != nil {
		t.Fatal(err)
	}
	nonExecutable := configNativeFixture(t, dir, "non-executable", 0x7f454c46)
	if err := os.Chmod(nonExecutable, 0600); err != nil {
		t.Fatal(err)
	}
	selfLink := filepath.Join(dir, "proxy-alias")
	if err := os.Symlink(self, selfLink); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, path, wantError string
	}{
		{"relative", "./codex", "absolute path"},
		{"missing", filepath.Join(dir, "missing"), "no such file"},
		{"directory", dir, "regular file"},
		{"script", script, "native ELF or Mach-O"},
		{"text launcher", text, "native ELF or Mach-O"},
		{"short header", short, "native ELF or Mach-O"},
		{"not executable", nonExecutable, "regular executable file"},
		{"self", self, "proxy itself"},
		{"symlink self", selfLink, "proxy itself"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeConfigFixture(t, dir, "CODEX_PROXY_REAL_BIN="+strconv.Quote(tt.path)+"\n")
			_, err := LoadConfig(dir, self)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("LoadConfig error = %v, want text %q", err, tt.wantError)
			}
		})
	}
}

func TestConfigFIFOProcessHelper(t *testing.T) {
	dir := os.Getenv("CODEX_PROXY_CONFIG_FIFO_TEST_DIR")
	if dir == "" {
		return
	}
	_, err := LoadConfig(dir, configSelf(t))
	if err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("LoadConfig FIFO error = %v, want regular-file rejection", err)
	}
}

func TestLoadConfigRejectsFIFOPaths(t *testing.T) {
	for _, target := range []string{"env", "real binary"} {
		t.Run(target, func(t *testing.T) {
			dir := t.TempDir()
			fifo := filepath.Join(dir, "env")
			if target == "real binary" {
				fifo = filepath.Join(dir, "native-codex")
				writeConfigFixture(t, dir, "CODEX_PROXY_REAL_BIN="+strconv.Quote(fifo)+"\n")
			}
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}

			// There is deliberately no FIFO writer. Run in a subprocess so a
			// regression to blocking open cannot leave a stuck test goroutine:
			// the deadline kills and reaps the entire blocked helper.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, configSelf(t), "-test.run=^TestConfigFIFOProcessHelper$")
			cmd.Env = append(os.Environ(), "CODEX_PROXY_CONFIG_FIFO_TEST_DIR="+dir)
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("LoadConfig blocked while opening FIFO %s: %v", target, ctx.Err())
			}
			if err != nil {
				t.Fatalf("FIFO helper failed: %v\n%s", err, output)
			}
		})
	}
}

package proxy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestRunLogsArgumentsOnConfigurationError(t *testing.T) {
	const prompt = "private exec prompt must stay out of logs"
	const input = "private stdin must stay out of logs"
	for _, command := range []string{"exec", "e", "login"} {
		t.Run(command, func(t *testing.T) {
			home := t.TempDir()
			args := []string{command, "--model", "test-model", "--json"}
			want := append([]string{}, args...)
			if command != "login" {
				args = append(args, prompt)
			}
			child := startProxy(t, home, "echo", "", input, 0, args...)
			if code := childExit(t, child, 5*time.Second); code != 2 {
				t.Fatalf("exit=%d stderr=%s", code, child.stderr.String())
			}
			dir := filepath.Join(home, ".codex-proxy")
			records := readLoggerRecords(t, dir, loggerDefaultMaxSize)
			if len(records) != 1 || records[0]["event"] != "error" {
				t.Fatalf("expected a configuration error record: %v", records)
			}
			if records[0]["user"] != "sandbox-user" {
				t.Errorf("logged user = %v, want sandbox-user", records[0]["user"])
			}
			encoded, err := json.Marshal(records[0]["args"])
			if err != nil {
				t.Fatal(err)
			}
			var logged []string
			if err := json.Unmarshal(encoded, &logged); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(logged, want) {
				t.Errorf("logged args = %q, want %q", logged, want)
			}
			data, err := os.ReadFile(filepath.Join(dir, loggerFilename))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte(prompt)) || bytes.Contains(data, []byte(input)) {
				t.Fatal("log contains prompt or stdin content")
			}
		})
	}
}

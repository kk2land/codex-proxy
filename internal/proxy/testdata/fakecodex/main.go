package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	dir := os.Getenv("FAKE_CODEX_DIR")
	pid := strconv.Itoa(os.Getpid())
	if dir != "" {
		if err := os.WriteFile(filepath.Join(dir, pid+".started"), []byte(pid), 0600); err != nil {
			panic(err)
		}
	}
	if os.Getenv("FAKE_CODEX_MODE") == "app-server" {
		appServer(dir)
	} else if os.Getenv("FAKE_CODEX_MODE") == "auth-background" {
		background := exec.Command("sleep", "30")
		background.Stdout, background.Stderr = os.Stdout, os.Stderr
		// An independently running tool keeps both pipes open even after the
		// native process and its process group have exited.
		background.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := background.Start(); err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "background.pid"), []byte(strconv.Itoa(background.Process.Pid)), 0600); err != nil {
			_ = background.Process.Kill()
			panic(err)
		}
		fmt.Fprintln(os.Stdout, "native stdout")
		fmt.Fprintln(os.Stderr, "native stderr")
		if os.Getenv("FAKE_CODEX_BACKGROUND_429") == "true" {
			// Also exercise the final partial error line when output draining is
			// stopped before the background tool closes the pipe.
			fmt.Fprint(os.Stdout, `{"type":"turn.failed","error":{"message":"HTTP status 429 Too Many Requests","retry_after":120}}`)
		}
	} else if os.Getenv("FAKE_CODEX_MODE") == "auth-nested" {
		cmd := exec.Command(os.Getenv("FAKE_CODEX_PROXY"), "-test.run=^TestRunHelperProcess$", "--", "exec", "inner")
		cmd.Env = append(os.Environ(), "FAKE_CODEX_MODE=echo")
		cmd.Stdin = strings.NewReader("")
		output, err := cmd.CombinedOutput()
		fmt.Fprintf(os.Stdout, "nested result=%v output=%s", err, output)
		if err != nil {
			os.Exit(1)
		}
	} else if os.Getenv("FAKE_CODEX_MODE") == "auth-limit-hold" {
		signal.Ignore(syscall.SIGTERM)
		fmt.Fprintln(os.Stdout, `{"type":"turn.failed","error":{"message":"HTTP status 429 Too Many Requests","retry_after":120}}`)
		for {
			time.Sleep(time.Second)
		}
	} else if os.Getenv("FAKE_CODEX_MODE") == "auth-limit" {
		fmt.Fprintln(os.Stdout, `{"type":"turn.failed","error":{"message":"HTTP status 429 Too Many Requests","retry_after":120}}`)
	} else if os.Getenv("FAKE_CODEX_MODE") == "auth-color-limit" {
		fmt.Fprint(os.Stdout, "\x1b[36mnative stdout\x1b[0m\n")
		fmt.Fprint(os.Stderr, "\x1b[31m\x1b[1mERROR:\x1b[0m You've hit your usage limit. Try again later.\n")
	} else if os.Getenv("FAKE_CODEX_MODE") == "sandbox-probe" {
		dir = os.Getenv("FAKE_SANDBOX_ROOT")
		own := filepath.Join(filepath.Dir(os.Getenv("CODEX_HOME")), "own-file")
		other := filepath.Join(dir, "other-user", "secret")
		_, ownRead := os.ReadFile(own)
		ownWrite := os.WriteFile(filepath.Join(filepath.Dir(own), "written"), nil, 0600)
		_, otherRead := os.ReadFile(other)
		otherWrite := os.WriteFile(filepath.Join(filepath.Dir(other), "written"), nil, 0600)
		_, otherStat := os.Stat(filepath.Dir(other))
		_, rootStat := os.Stat(dir)
		_, rootList := os.ReadDir(dir)
		_, linkRead := os.ReadFile(filepath.Join(filepath.Dir(own), "sibling-link"))
		checks := map[string]bool{
			"own_read": ownRead == nil, "own_write": ownWrite == nil,
			"other_read_denied": otherRead != nil, "other_write_denied": otherWrite != nil,
			"other_metadata_denied": otherStat != nil, "root_metadata": rootStat == nil,
			"root_listing_denied": rootList != nil, "symlink_denied": linkRead != nil,
		}
		if proxyDir := os.Getenv("FAKE_SANDBOX_PROXY_DIR"); proxyDir != "" {
			_, read := os.ReadFile(filepath.Join(proxyDir, "env"))
			_, envStat := os.Stat(filepath.Join(proxyDir, "env"))
			_, secretRead := os.ReadFile(filepath.Join(proxyDir, "secret"))
			_, secretStat := os.Stat(filepath.Join(proxyDir, "secret"))
			_, rulesRead := os.ReadFile(filepath.Join(proxyDir, "sandbox.sb"))
			write := os.WriteFile(filepath.Join(proxyDir, "env"), nil, 0600)
			create := os.WriteFile(filepath.Join(proxyDir, "new-file"), nil, 0600)
			unlink := os.Remove(filepath.Join(proxyDir, "secret"))
			_, list := os.ReadDir(proxyDir)
			_, stat := os.Stat(proxyDir)
			_, link := os.ReadFile(filepath.Join(filepath.Dir(own), "proxy-link"))
			_, envLink := os.ReadFile(filepath.Join(filepath.Dir(own), "proxy-env-link"))
			envUnlink := os.Remove(filepath.Join(proxyDir, "env"))
			private := filepath.Join(filepath.Dir(own), "private")
			_, privateRead := os.ReadFile(filepath.Join(private, "secret"))
			privateWrite := os.WriteFile(filepath.Join(private, "written"), nil, 0600)
			checks["proxy_env_read"], checks["proxy_env_metadata"] = read == nil, envStat == nil
			checks["proxy_env_symlink_read"], checks["proxy_env_unlink_denied"] = envLink == nil, envUnlink != nil
			checks["proxy_read_denied"], checks["proxy_secret_metadata_denied"] = secretRead != nil, secretStat != nil
			checks["proxy_rules_read_denied"] = rulesRead != nil
			checks["proxy_write_denied"], checks["proxy_create_denied"] = write != nil, create != nil
			checks["proxy_unlink_denied"], checks["proxy_listing_denied"] = unlink != nil, list != nil
			checks["proxy_metadata_denied"], checks["proxy_symlink_denied"] = stat != nil, link != nil
			checks["custom_read_denied"], checks["custom_write_denied"] = privateRead != nil, privateWrite != nil
		}
		_ = json.NewEncoder(os.Stdout).Encode(checks)
	} else if os.Getenv("FAKE_CODEX_MODE") == "hold" {
		for {
			if _, err := os.Stat(filepath.Join(dir, "release-"+pid)); err == nil {
				break
			}
			if _, err := os.Stat(filepath.Join(dir, "release-all")); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	} else {
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			panic(err)
		}
		cwd, _ := os.Getwd()
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"args": os.Args[1:], "stdin": string(input), "home": os.Getenv("HOME"),
			"user": os.Getenv("USER"), "cwd": cwd,
		})
		fmt.Fprintln(os.Stderr, "fake-codex-stderr")
	}
	if dir != "" && os.Getenv("FAKE_CODEX_MODE") != "sandbox-probe" {
		_ = os.WriteFile(filepath.Join(dir, pid+".finished"), []byte(pid), 0600)
	}
	code, _ := strconv.Atoi(os.Getenv("FAKE_CODEX_EXIT"))
	os.Exit(code)
}

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	} else if os.Getenv("FAKE_CODEX_MODE") == "error-output" {
		fmt.Fprint(os.Stdout, os.Getenv("FAKE_CODEX_STDOUT"))
		fmt.Fprint(os.Stderr, os.Getenv("FAKE_CODEX_STDERR"))
	} else if os.Getenv("FAKE_CODEX_MODE") == "nested-proxy" {
		cmd := exec.Command(os.Getenv("FAKE_CODEX_PROXY"), "-test.run=^TestNestedProxyHelperProcess$")
		cmd.Env = os.Environ()
		output, err := cmd.CombinedOutput()
		fmt.Fprintf(os.Stdout, "nested result=%v output=%s", err, output)
		if err != nil {
			os.Exit(1)
		}
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
			_, list := os.ReadDir(proxyDir)
			_, stat := os.Stat(proxyDir)
			_, link := os.ReadFile(filepath.Join(filepath.Dir(own), "proxy-link"))
			_, envLink := os.ReadFile(filepath.Join(filepath.Dir(own), "proxy-env-link"))
			unlink := os.Remove(filepath.Join(proxyDir, "secret"))
			envUnlink := os.Remove(filepath.Join(proxyDir, "env"))
			private := filepath.Join(filepath.Dir(own), "private")
			_, privateRead := os.ReadFile(filepath.Join(private, "secret"))
			privateWrite := os.WriteFile(filepath.Join(private, "written"), nil, 0600)
			checks["proxy_env_read"], checks["proxy_env_metadata"] = read == nil, envStat == nil
			checks["proxy_env_symlink_read"], checks["proxy_env_unlink_denied"] = envLink == nil, envUnlink != nil
			checks["proxy_read"], checks["proxy_secret_metadata"] = secretRead == nil, secretStat == nil
			checks["proxy_rules_read"] = rulesRead == nil
			checks["proxy_env_write_denied"], checks["proxy_create"] = write != nil, create == nil
			checks["proxy_unlink"], checks["proxy_listing"] = unlink == nil, list == nil
			checks["proxy_metadata"], checks["proxy_symlink_read"] = stat == nil, link == nil
			checks["custom_read_denied"], checks["custom_write_denied"] = privateRead != nil, privateWrite != nil
			checks["proxy_directory_mode"] = os.Chmod(proxyDir, 0700) == nil
			checks["proxy_env_mode_denied"] = os.Chmod(filepath.Join(proxyDir, "env"), 0600) != nil
			checks["proxy_rules_write_denied"] = os.WriteFile(filepath.Join(proxyDir, "sandbox.sb"), nil, 0600) != nil
			checks["proxy_rules_unlink_denied"] = os.Remove(filepath.Join(proxyDir, "sandbox.sb")) != nil
			checks["proxy_rules_mode_denied"] = os.Chmod(filepath.Join(proxyDir, "sandbox.sb"), 0600) != nil
			temporary, err := os.CreateTemp(proxyDir, ".state-*.json")
			checks["proxy_runtime_temp"] = err == nil
			checks["proxy_env_replace_denied"], checks["proxy_rules_replace_denied"] = false, false
			if err == nil {
				_ = temporary.Close()
				checks["proxy_env_replace_denied"] = os.Rename(temporary.Name(), filepath.Join(proxyDir, "env")) != nil
				checks["proxy_rules_replace_denied"] = os.Rename(temporary.Name(), filepath.Join(proxyDir, "sandbox.sb")) != nil
				_ = os.Remove(temporary.Name())
			}
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

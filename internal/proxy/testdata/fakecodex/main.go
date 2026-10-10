package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
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
	if os.Getenv("FAKE_CODEX_MODE") == "sandbox-probe" {
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
		_ = json.NewEncoder(os.Stdout).Encode(map[string]bool{
			"own_read": ownRead == nil, "own_write": ownWrite == nil,
			"other_read_denied": otherRead != nil, "other_write_denied": otherWrite != nil,
			"other_metadata_denied": otherStat != nil, "root_metadata": rootStat == nil,
			"root_listing_denied": rootList != nil, "symlink_denied": linkRead != nil,
		})
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

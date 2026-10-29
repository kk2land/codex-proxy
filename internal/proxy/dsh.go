package proxy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// RunDSH replaces this process with sandbox-exec on macOS, or the real dsh on
// Linux. It forwards every argument, the environment, cwd and inherited FDs.
func RunDSH(args []string) int {
	return runDSH(args, dshOptions{})
}

// Test injections are private and cannot disable the production sandbox via
// command-line arguments or environment variables.
type dshOptions struct {
	accountHome func() (string, error)
	stderr      io.Writer
	exec        func(string, []string, []string) error
	goos        string
}

func runDSH(args []string, opts dshOptions) int {
	if opts.stderr == nil {
		opts.stderr = os.Stderr
	}
	if opts.exec == nil {
		opts.exec = syscall.Exec
	}
	if opts.accountHome == nil {
		opts.accountHome = SystemHome
	}
	if opts.goos == "" {
		opts.goos = runtime.GOOS
	}
	fail := func(err error) int {
		fmt.Fprintln(opts.stderr, "dsh-proxy:", err)
		return 2
	}
	home, err := opts.accountHome()
	if err != nil {
		return fail(err)
	}
	if !filepath.IsAbs(home) {
		return fail(errors.New("account home must be absolute"))
	}
	dir := filepath.Join(home, ".codex-proxy")
	values, err := loadConfigValues(dir)
	if err != nil {
		return fail(err)
	}
	self, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	realBin, err := resolveDSHBinary(values["DSH_PROXY_REAL_BIN"], self)
	if err != nil {
		return fail(err)
	}
	path := realBin
	argv := append([]string{realBin}, args...)
	if opts.goos == "darwin" {
		profile, err := dshSandboxProfile(os.Getenv("DSH_HOME"), dir)
		if err != nil {
			return fail(err)
		}
		path = "/usr/bin/sandbox-exec"
		argv = append([]string{path, "-p", profile, realBin}, args...)
	}
	// No child process, protocol forwarding, admission lock or signal handler:
	// a successful exec never returns, and a failed sandbox never falls back.
	if err := opts.exec(path, argv, os.Environ()); err != nil {
		return fail(fmt.Errorf("exec %s: %w", path, err))
	}
	return 0
}

func dshSandboxProfile(home, proxyDir string) (string, error) {
	profile, err := sandboxProfileForHome(home, "DSH_HOME", "", "")
	if err != nil {
		return "", err
	}
	suffix, err := sandboxProfileSuffix(proxyDir)
	if err != nil {
		return "", err
	}
	return profile + suffix, nil
}

func resolveDSHBinary(configured, self string) (string, error) {
	if configured == "" {
		return "", errors.New("DSH_PROXY_REAL_BIN is required")
	}
	if !filepath.IsAbs(configured) {
		return "", errors.New("DSH_PROXY_REAL_BIN must be an absolute executable path")
	}
	info, err := os.Stat(configured)
	if err != nil {
		return "", fmt.Errorf("DSH_PROXY_REAL_BIN: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", errors.New("DSH_PROXY_REAL_BIN must be a regular executable file")
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return "", fmt.Errorf("identify dsh wrapper: %w", err)
	}
	if os.SameFile(info, selfInfo) {
		return "", errors.New("DSH_PROXY_REAL_BIN must not point to the dsh wrapper itself")
	}
	return configured, nil
}

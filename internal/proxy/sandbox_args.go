package proxy

import (
	"fmt"
	"strings"
)

// macOSExecArgs adjusts only the two options managed by the proxy. All other
// arguments, including options unknown to this version, go to Codex unchanged.
func macOSExecArgs(args []string) ([]string, error) {
	kind, index := classifyArgs(args)
	if kind != CommandExec {
		return nil, fmt.Errorf("macOS sandbox arguments require an exec command")
	}
	prefix, prefixSandbox, prefixSkip := rewriteMacOSExecOptions(args[:index], "top-level")
	tail, tailSandbox, tailSkip := rewriteMacOSExecOptions(args[index+1:], "exec")
	result := make([]string, 0, len(prefix)+len(tail)+4)
	result = append(result, prefix...)
	result = append(result, args[index])
	if !prefixSkip && !tailSkip {
		result = append(result, "--skip-git-repo-check")
	}
	if !prefixSandbox && !tailSandbox {
		result = append(result, "--sandbox", "danger-full-access")
	}
	return append(result, tail...), nil
}

func rewriteMacOSExecOptions(args []string, scope string) (result []string, sandbox, skip bool) {
	result = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(result, args[i:]...), sandbox, skip
		}
		if !isOption(arg) {
			result = append(result, arg)
			if scope == "exec" {
				switch arg {
				case "resume", "fork", "review":
					scope = arg
				case "help":
					return append(result, args[i+1:]...), sandbox, skip
				}
			}
			continue
		}
		name := arg
		attached := false
		if strings.HasPrefix(arg, "--") {
			name, _, attached = strings.Cut(arg, "=")
		} else {
			name = arg[:2]
			attached = len(arg) > 2
		}
		if name == "--sandbox" || name == "-s" {
			sandbox = true
			result = append(result, "--sandbox", "danger-full-access")
			if !attached && i+1 < len(args) && !isOption(args[i+1]) {
				i++
			}
			continue
		}
		if arg == "--skip-git-repo-check" {
			skip = true
		}
		last := i
		if value, variadic := macOSOptionValue(name, scope); value && !attached {
			// Recognize known option values without validating the caller's
			// syntax. Codex handles missing values and unknown options itself.
			if end, ok := optionValueEnd(args, i, variadic); ok {
				last = end
			}
		}
		result = append(result, args[i:last+1]...)
		i = last
	}
	return result, sandbox, skip
}

// macOSOptionValue describes known value boundaries, not an option whitelist.
func macOSOptionValue(name, scope string) (value, variadic bool) {
	switch name {
	case "--config", "-c", "--enable", "--disable", "--model", "-m",
		"--local-provider", "--profile", "-p", "--cd", "-C", "--add-dir",
		"--remote", "--remote-auth-token-env", "--ask-for-approval", "-a",
		"--thread-source", "--output-schema", "--output-last-message", "-o",
		"--color", "--base", "--commit", "--title":
		return true, false
	case "--image", "-i":
		return true, scope == "top-level" || scope == "exec"
	}
	return false, false
}

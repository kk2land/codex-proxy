package proxy

import (
	"fmt"
	"strings"
)

// macOSExecArgs enforces the proxy's execution flags and removes explicit
// sandbox modes, preserving variadic image boundaries when a mode is removed.
// Other arguments go to Codex unchanged.
func macOSExecArgs(args []string) ([]string, error) {
	kind, index := classifyArgs(args)
	if kind != CommandExec {
		return nil, fmt.Errorf("macOS sandbox arguments require an exec command")
	}
	prefix, prefixBypass, prefixSkip := rewriteMacOSExecOptions(args[:index], "top-level")
	tail, tailBypass, tailSkip := rewriteMacOSExecOptions(args[index+1:], "exec")
	result := make([]string, 0, len(prefix)+len(tail)+2)
	result = append(result, prefix...)
	result = append(result, args[index])
	if !prefixSkip && !tailSkip {
		result = append(result, "--skip-git-repo-check")
	}
	if !prefixBypass && !tailBypass {
		result = append(result, "--dangerously-bypass-approvals-and-sandbox")
	}
	return append(result, tail...), nil
}

func rewriteMacOSExecOptions(args []string, scope string) (result []string, bypass, skip bool) {
	result = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(result, args[i:]...), bypass, skip
		}
		if !isOption(arg) {
			result = append(result, arg)
			if scope == "exec" {
				switch arg {
				case "resume", "fork", "review":
					scope = arg
				case "help":
					return append(result, args[i+1:]...), bypass, skip
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
			if !attached && i+1 < len(args) && !isOption(args[i+1]) {
				i++
			}
			continue
		}
		if arg == "--dangerously-bypass-approvals-and-sandbox" {
			bypass = true
		}
		if arg == "--skip-git-repo-check" {
			skip = true
		}
		last := i
		if value, variadic := codexOptionValue(name, scope); value && !attached {
			// Recognize known option values without validating the caller's
			// syntax. Codex handles missing values and unknown options itself.
			if end, ok := optionValueEnd(args, i, variadic); ok {
				last = end
			}
			if variadic && last > i && last+1 < len(args) && isSandboxModeOption(args[last+1]) {
				// Removing the following sandbox option must not make a prompt
				// or subcommand look like another image path.
				for _, path := range args[i+1 : last+1] {
					result = append(result, "--image="+path)
				}
				i = last
				continue
			}
		}
		result = append(result, args[i:last+1]...)
		i = last
	}
	return result, bypass, skip
}

func isSandboxModeOption(arg string) bool {
	return arg == "--sandbox" || strings.HasPrefix(arg, "--sandbox=") || strings.HasPrefix(arg, "-s")
}

// codexOptionValue describes known value boundaries, not an option whitelist.
func codexOptionValue(name, scope string) (value, variadic bool) {
	switch name {
	case "--config", "-c", "--enable", "--disable", "--model", "-m",
		"--local-provider", "--profile", "-p", "--cd", "-C", "--add-dir",
		"--remote", "--remote-auth-token-env", "--ask-for-approval", "-a",
		"--thread-source", "--output-schema", "--output-last-message", "-o",
		"--color", "--base", "--commit", "--title", "--sandbox", "-s",
		"--cyber-access-program":
		return true, false
	case "--image", "-i":
		return true, scope == "top-level" || scope == "exec"
	}
	return false, false
}

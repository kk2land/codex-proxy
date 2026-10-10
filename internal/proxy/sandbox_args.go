package proxy

import (
	"fmt"
	"strings"
)

// macOSExecArgs applies the Codex policy used inside the OS sandbox. It parses
// options instead of deleting matching tokens: an option value or a prompt
// containing --sandbox must survive unchanged.
func macOSExecArgs(args []string) ([]string, error) {
	kind, err := ClassifyArgs(args)
	if err != nil {
		return nil, err
	}
	if kind != CommandExec {
		return nil, fmt.Errorf("macOS sandbox arguments require an exec command")
	}
	index := -1
	for i, arg := range args {
		if arg != "exec" && arg != "e" {
			continue
		}
		// The classifier knows which occurrences are option values, including
		// variadic top-level image values and a preceding positional prompt.
		if prefixKind, prefixErr := ClassifyArgs(args[:i+1]); prefixErr == nil && prefixKind == CommandExec {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, fmt.Errorf("cannot locate exec command for macOS sandbox")
	}
	prefix, err := filterMacOSExecOptions(args[:index], "top-level")
	if err != nil {
		return nil, err
	}
	tail, err := filterMacOSExecOptions(args[index+1:], "exec")
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(prefix)+len(tail)+4)
	result = append(result, prefix...)
	result = append(result, args[index], "--skip-git-repo-check", "--sandbox", "danger-full-access")
	return append(result, tail...), nil
}

type macOSOption struct {
	value    bool
	variadic bool
	remove   bool
	stop     bool
}

func filterMacOSExecOptions(args []string, scope string) ([]string, error) {
	result := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(result, args[i:]...), nil
		}
		if !isOption(arg) {
			result = append(result, arg)
			if scope == "exec" {
				switch arg {
				case "resume", "fork", "review":
					scope = arg
				case "help":
					// Help arguments name commands rather than execution options.
					return append(result, args[i+1:]...), nil
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
		spec, known := macOSExecOption(name, scope)
		if !known {
			return nil, fmt.Errorf("unsupported %s Codex option %q; update the proxy for this Codex version", scope, name)
		}
		if spec.stop {
			if strings.HasPrefix(arg, "--") && attached {
				return nil, fmt.Errorf("%s option %q does not take a value", scope, name)
			}
			// Clap short-circuits at help/version, including bundles such as
			// -hsread-only. Keeping this tail also preserves that behavior.
			return append(result, args[i:]...), nil
		}
		if !spec.value && attached {
			return nil, fmt.Errorf("%s option %q does not take a value", scope, name)
		}
		last := i
		if spec.value && !attached {
			var err error
			last, err = consumeOptionValue(args, i, name, spec.variadic)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", scope, err)
			}
		}
		if !spec.remove {
			if spec.variadic && !attached && last+1 < len(args) && macOSRemovableOption(args[last+1], scope) {
				// Removing the flag that ended --image/-i must not cause image
				// values to swallow the following prompt or command. Attached,
				// repeatable image values preserve the original boundary.
				for _, value := range args[i+1 : last+1] {
					result = append(result, "--image="+value)
				}
			} else {
				result = append(result, args[i:last+1]...)
			}
		}
		i = last
	}
	return result, nil
}

func macOSRemovableOption(arg, scope string) bool {
	if !isOption(arg) || arg == "--" {
		return false
	}
	name := arg
	if strings.HasPrefix(arg, "--") {
		name, _, _ = strings.Cut(arg, "=")
	} else {
		name = arg[:2]
	}
	spec, known := macOSExecOption(name, scope)
	return known && spec.remove
}

// macOSExecOption follows Codex CLI 0.155.0. Unknown options fail closed rather
// than guessing their arity and potentially removing a value-taking flag's
// argument. Child commands accept different options from the exec parent.
func macOSExecOption(name, scope string) (macOSOption, bool) {
	top := scope == "top-level"
	parent := scope == "exec"
	switch name {
	case "--config", "-c", "--enable", "--disable", "--model", "-m":
		return macOSOption{value: true}, true
	case "--strict-config", "--dangerously-bypass-hook-trust", "--worktree",
		"--dangerously-bypass-approvals-and-sandbox":
		return macOSOption{}, true
	case "--help", "-h":
		return macOSOption{stop: true}, true
	case "--version", "-V":
		return macOSOption{stop: true}, top || parent
	case "--sandbox", "-s":
		return macOSOption{value: true, remove: true}, top || parent
	case "--approve-for-me":
		return macOSOption{remove: true}, top || parent
	case "--image", "-i":
		return macOSOption{value: true, variadic: top || parent}, scope != "review"
	case "--local-provider", "--profile", "-p", "--cd", "-C", "--add-dir":
		return macOSOption{value: true}, top || parent
	case "--oss":
		return macOSOption{}, top || parent
	case "--remote", "--remote-auth-token-env", "--ask-for-approval", "-a":
		return macOSOption{value: true}, top
	case "--search", "--no-alt-screen":
		return macOSOption{}, top
	case "--skip-git-repo-check":
		return macOSOption{remove: true}, !top
	case "--thread-source", "--output-schema", "--output-last-message", "-o":
		return macOSOption{value: true}, !top
	case "--ephemeral", "--ignore-user-config", "--ignore-rules", "--json":
		return macOSOption{}, !top
	case "--color":
		return macOSOption{value: true}, parent
	case "--last", "--all":
		return macOSOption{}, scope == "resume"
	case "--uncommitted":
		return macOSOption{}, scope == "review"
	case "--base", "--commit", "--title":
		return macOSOption{value: true}, scope == "review"
	}
	return macOSOption{}, false
}

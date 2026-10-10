package proxy

import (
	"fmt"
	"strings"
)

// CommandKind describes which interception policy applies to a Codex invocation.
type CommandKind uint8

const (
	CommandPassThrough CommandKind = iota
	CommandExec
	CommandAppServer
)

// ClassifyArgs follows the top-level argument grammar of Codex CLI 0.155.0.
// Options and their values must be distinguished from commands: for example,
// --model exec does not invoke exec. Once a command is found, its remaining
// arguments belong to that command and do not affect interception.
//
// Unknown top-level options fail closed. This prevents an option added by a
// future Codex release from accidentally hiding an intercepted command in its
// values. The caller should report the error without starting the real binary.
func ClassifyArgs(args []string) (CommandKind, error) {
	seenPrompt := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			// Everything after -- is positional, including command-like text.
			return CommandPassThrough, nil
		}
		if strings.HasPrefix(arg, "--") {
			name, _, attached := strings.Cut(arg, "=")
			switch name {
			case "--help", "--version":
				if attached {
					return CommandPassThrough, fmt.Errorf("top-level option %q does not take a value", name)
				}
				return CommandPassThrough, nil
			case "--strict-config", "--oss", "--approve-for-me",
				"--dangerously-bypass-approvals-and-sandbox", "--dangerously-bypass-hook-trust",
				"--worktree", "--search", "--no-alt-screen":
				if attached {
					return CommandPassThrough, fmt.Errorf("top-level option %q does not take a value", name)
				}
			case "--config", "--enable", "--disable", "--remote", "--remote-auth-token-env",
				"--image", "--model", "--local-provider", "--profile", "--sandbox", "--cd",
				"--add-dir", "--ask-for-approval":
				if !attached {
					last, err := consumeOptionValue(args, i, name, name == "--image")
					if err != nil {
						return CommandPassThrough, err
					}
					i = last
				}
			default:
				return CommandPassThrough, fmt.Errorf("unsupported top-level Codex option %q; update the proxy for this Codex version", name)
			}
			continue
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			// h and V immediately print help/version even in a short-option
			// bundle. The other short options take the remaining suffix as
			// their value, or consume the next argument if there is no suffix.
			for j := 1; j < len(arg); j++ {
				switch arg[j] {
				case 'h', 'V':
					return CommandPassThrough, nil
				case 'c', 'i', 'm', 'p', 's', 'C', 'a':
					if j+1 == len(arg) {
						name := "-" + string(arg[j])
						last, err := consumeOptionValue(args, i, name, arg[j] == 'i')
						if err != nil {
							return CommandPassThrough, err
						}
						i = last
					}
					// An attached value ends this option, including a variadic
					// image option, as in -iimage.png exec.
					j = len(arg)
				default:
					return CommandPassThrough, fmt.Errorf("unsupported top-level Codex option %q; update the proxy for this Codex version", "-"+string(arg[j]))
				}
			}
			continue
		}

		switch arg {
		case "exec", "e":
			return CommandExec, nil
		case "app-server":
			return CommandAppServer, nil
		case "agents", "review", "login", "logout", "mcp", "plugin", "remote-control",
			"app", "completion", "update", "doctor", "sandbox", "debug", "apply", "a",
			"resume", "queue", "archive", "delete", "migrate-rollouts", "unarchive", "fork",
			"cloud", "exec-server", "features", "help":
			return CommandPassThrough, nil
		}
		if seenPrompt {
			return CommandPassThrough, fmt.Errorf("unexpected additional top-level argument")
		}
		// Clap can recognize a subcommand after the optional initial prompt,
		// for example: codex "initial prompt" exec --help.
		seenPrompt = true
	}
	return CommandPassThrough, nil
}

func consumeOptionValue(args []string, index int, name string, variadic bool) (int, error) {
	last := index + 1
	if last == len(args) || isOption(args[last]) {
		return index, fmt.Errorf("top-level option %q requires a value", name)
	}
	if variadic {
		// A separate --image/-i value consumes subsequent non-option
		// arguments, including words that otherwise name subcommands.
		for last+1 < len(args) && !isOption(args[last+1]) {
			last++
		}
	}
	return last, nil
}

func isOption(arg string) bool {
	return strings.HasPrefix(arg, "-") && arg != "-"
}

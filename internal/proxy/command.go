package proxy

import "strings"

// CommandKind describes which interception policy applies to a Codex invocation.
type CommandKind uint8

const (
	CommandPassThrough CommandKind = iota
	CommandExec
	CommandAppServer
)

// ClassifyArgs identifies commands while leaving argument validation to Codex.
// Known top-level options skip their values so --model exec is not a command.
// Unknown options are accepted without modifying the original arguments.
func ClassifyArgs(args []string) (CommandKind, error) {
	kind, _ := classifyArgs(args)
	return kind, nil
}

// classifyArgs also returns the intercepted command's position. Once a known
// command is found, its arguments do not affect classification.
func classifyArgs(args []string) (CommandKind, int) {
	seenPrompt := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			// Everything after -- is positional, including command-like text.
			return CommandPassThrough, -1
		}
		if strings.HasPrefix(arg, "--") {
			name, _, attached := strings.Cut(arg, "=")
			switch name {
			case "--help", "--version":
				return CommandPassThrough, -1
			case "--config", "--enable", "--disable", "--remote", "--remote-auth-token-env",
				"--image", "--model", "--local-provider", "--profile", "--sandbox", "--cd",
				"--add-dir", "--ask-for-approval":
				if !attached {
					last, ok := optionValueEnd(args, i, name == "--image")
					if !ok {
						return CommandPassThrough, -1
					}
					i = last
				}
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
					return CommandPassThrough, -1
				case 'c', 'i', 'm', 'p', 's', 'C', 'a':
					if j+1 == len(arg) {
						last, ok := optionValueEnd(args, i, arg[j] == 'i')
						if !ok {
							return CommandPassThrough, -1
						}
						i = last
					}
					// An attached value ends this option, including a variadic
					// image option, as in -iimage.png exec.
					j = len(arg)
				default:
					// Its suffix may be an attached value. Codex owns the
					// grammar of unknown options, so leave the token intact.
					j = len(arg)
				}
			}
			continue
		}

		switch arg {
		case "exec", "e":
			return CommandExec, i
		case "app-server":
			return CommandAppServer, i
		case "agents", "review", "login", "logout", "mcp", "plugin", "remote-control",
			"app", "completion", "update", "doctor", "sandbox", "debug", "apply", "a",
			"resume", "queue", "archive", "delete", "migrate-rollouts", "unarchive", "fork",
			"cloud", "exec-server", "features", "help":
			return CommandPassThrough, -1
		}
		if seenPrompt {
			return CommandPassThrough, -1
		}
		// Clap can recognize a subcommand after the optional initial prompt,
		// for example: codex "initial prompt" exec --help.
		seenPrompt = true
	}
	return CommandPassThrough, -1
}

func optionValueEnd(args []string, index int, variadic bool) (int, bool) {
	last := index + 1
	if last == len(args) || isOption(args[last]) {
		return index, false
	}
	if variadic {
		// A separate --image/-i value consumes subsequent non-option
		// arguments, including words that otherwise name subcommands.
		for last+1 < len(args) && !isOption(args[last+1]) {
			last++
		}
	}
	return last, true
}

func isOption(arg string) bool {
	return strings.HasPrefix(arg, "-") && arg != "-"
}

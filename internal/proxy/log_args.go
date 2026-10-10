package proxy

import "strings"

// logArgs copies command arguments for logging, omitting exec prompts without
// changing the arguments passed to Codex.
func logArgs(args []string) []string {
	kind, index := classifyArgs(args)
	if kind != CommandExec {
		return append([]string{}, args...)
	}
	result := make([]string, 0, len(args))
	result = append(result, execLogScopeArgs(args[:index], "top-level")...)
	result = append(result, args[index])
	return append(result, execLogScopeArgs(args[index+1:], "exec")...)
}

func execLogScopeArgs(args []string, scope string) []string {
	result := make([]string, 0, len(args))
	positionalOnly, last := false, false
	sessionIndex := -1
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !positionalOnly && arg == "--" {
			result = append(result, arg)
			positionalOnly = true
			continue
		}
		if !positionalOnly && isOption(arg) {
			name, attached := arg[:2], len(arg) > 2
			if strings.HasPrefix(arg, "--") {
				name, _, attached = strings.Cut(arg, "=")
			}
			if scope == "resume" && name == "--last" {
				last = true
			}
			end := i
			if value, variadic := codexOptionValue(name, scope); value && !attached {
				if valueEnd, ok := optionValueEnd(args, i, variadic); ok {
					end = valueEnd
				}
			}
			result = append(result, args[i:end+1]...)
			i = end
			continue
		}
		if scope == "exec" && !positionalOnly {
			switch arg {
			case "resume", "fork", "review", "help":
				scope = arg
				result = append(result, arg)
				continue
			}
		}
		if (scope == "resume" || scope == "fork") && sessionIndex == -1 {
			sessionIndex = len(result)
			result = append(result, arg)
		} else if scope == "help" {
			switch arg {
			case "resume", "fork", "review", "help":
				result = append(result, arg)
			}
		}
		// Other positional arguments may contain a prompt, including bare
		// values after unknown options, so they are omitted from the log.
	}
	if scope == "resume" && last && sessionIndex != -1 {
		result = append(result[:sessionIndex], result[sessionIndex+1:]...)
	}
	return result
}

package proxy

import (
	"errors"
	"strings"
)

type claudeCommand int

const (
	claudeTask claudeCommand = iota
	claudeStream
	claudePassThrough
)

// Unknown invocations count as tasks. Only positively identified management
// commands bypass admission; flags' values must never look like commands.
func classifyClaudeArgs(args []string) (claudeCommand, error) {
	var positionals []string
	var command string
	var inputFormat, outputFormat string
	printMode, help, jsonMode, unknown := false, false, false, false
	options := true
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if options && arg == "--" {
			options = false
			continue
		}
		if !options || !strings.HasPrefix(arg, "-") || arg == "-" {
			// Only options preceding the first positional can make its role
			// ambiguous. A subcommand's own flags must not change that decision.
			if len(positionals) == 0 && !printMode && !unknown {
				command = arg
			}
			positionals = append(positionals, arg)
			continue
		}
		name, value, attached := strings.Cut(arg, "=")
		switch name {
		case "--bg", "--background", "--cloud", "--remote", "--environment", "--desktop", "--teleport", "--remote-control", "--rc", "--sdk-url", "--channels", "--dangerously-load-development-channels":
			return claudeTask, errors.New("Claude detached, remote, channel, and daemon execution is not supported by this proxy")
		case "--help", "-h", "--version", "-v":
			help = true
		case "--print", "-p":
			printMode = true
		case "--json":
			jsonMode = true
		case "--input-format", "--output-format":
			if !attached {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return claudeTask, errors.New("Claude format option requires a value")
				}
				i++
				value = args[i]
			}
			if name == "--input-format" {
				inputFormat = value
			} else {
				outputFormat = value
			}
		case "--resume", "-r", "--from-pr", "--worktree", "-w", "--debug", "-d", "--prompt-suggestions":
			// Commander optional values consume the next non-option token.
			if !attached && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
		case "--tools", "--allowedTools", "--allowed-tools", "--disallowedTools", "--disallowed-tools", "--mcp-config", "--add-dir", "--betas", "--file":
			if !attached {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return claudeTask, errors.New("Claude option requires a value")
				}
				// Space-separated variadic values continue until the next option.
				// An attached value ends this occurrence, matching the CLI parser.
				for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i++
				}
			}
		case "--model", "--fallback-model", "--agent", "--agents", "--effort", "--settings", "--managed-settings", "--setting-sources", "--session-id", "--resume-session-at", "--permission-mode", "--permission-prompt-tool", "--permission-prompts", "--system-prompt", "--system-prompt-file", "--append-system-prompt", "--append-system-prompt-file", "--append-subagent-system-prompt", "--append-subagent-system-prompt-file", "--json-schema", "--max-turns", "--max-budget-usd", "--max-thinking-tokens", "--thinking", "--thinking-display", "--task-budget", "--debug-file", "--name", "-n", "--cwd", "--config", "--advisor", "--autocompact", "--plugin-dir", "--plugin-url":
			if !attached {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return claudeTask, errors.New("Claude option requires a value")
				}
				i++
			}
		case "--continue", "-c", "--verbose", "--assistant", "--session-mirror", "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions", "--fork-session", "--no-session-persistence", "--include-partial-messages", "--include-hook-events", "--forward-subagent-text", "--replay-user-messages", "--disable-slash-commands", "--strict-mcp-config", "--bare", "--safe-mode", "--restricted", "--init", "--init-only", "--maintenance", "--chrome", "--no-chrome", "--ide", "--exclude-dynamic-system-prompt-sections":
		default:
			// Unknown flags remain the CLI's responsibility, but cannot turn
			// their following value into an exempt management command.
			unknown = true
		}
	}
	if help {
		return claudePassThrough, nil
	}
	if !printMode {
		switch command {
		case "remote-control", "daemon", "attach", "respawn", "self-hosted-runner", "gateway", "ultrareview":
			return claudeTask, errors.New("Claude detached, remote, and daemon execution is not supported by this proxy")
		case "agents":
			if !jsonMode {
				return claudeTask, errors.New("interactive Claude agent dispatch is not supported; agents --json is allowed")
			}
			return claudePassThrough, nil
		case "auth", "mcp", "plugin", "plugins", "config", "doctor", "update", "upgrade", "install", "setup-token", "logs", "stop", "kill", "rm", "purge", "auto-mode":
			return claudePassThrough, nil
		}
	}
	if inputFormat == "stream-json" {
		if len(positionals) != 0 || outputFormat != "stream-json" {
			return claudeTask, errors.New("Claude stream proxy requires stream-json output and prompts sent only through stdin")
		}
		return claudeStream, nil
	}
	return claudeTask, nil
}

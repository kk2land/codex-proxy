package proxy

import (
	"errors"
	"strings"
)

const appServerLogFilename = "codex-app-server.log"
const maxAppServerRetainedBytes = 32 << 20

// Launch parameters captured by fake codex, never sent over client stdio.
type appServerStartup struct {
	Args []string
	Cwd  string
	Env  []string
}

type appServerLaunchMode int

const (
	appServerStdio appServerLaunchMode = iota
	appServerTooling
	appServerNative // Native subcommands are available only under bypass.
)

// No alternate listener may bypass the service's task admission. Help, schema
// generators, and startup discovery remain normal CLI tooling and do not
// create a server.
func appServerMode(args []string) (appServerLaunchMode, error) {
	kind, index := classifyArgs(args)
	if kind != CommandAppServer {
		return 0, errors.New("app-server launch request requires an app-server command")
	}
	for i := index + 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--help" || arg == "-h" {
			return appServerTooling, nil
		}
		name, value, attached := strings.Cut(arg, "=")
		// Startup discovery is a one-shot tooling command. It must reach the
		// native parser exactly as supplied, without a listener or policy flags.
		if name == "--print-startup-config" {
			return appServerTooling, nil
		}
		if name == "--listen" {
			if !attached {
				i++
				if i >= len(args) {
					return 0, errors.New("app-server --listen requires stdio://")
				}
				value = args[i]
			}
			if value != "stdio://" {
				return 0, errors.New("codex-proxy supports only app-server stdio:// transport")
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			if !attached && (name == "--config" || name == "-c" || name == "--enable" || name == "--disable" || name == "--code-mode-host" || name == "--startup-client-info" || name == "--cloud-skills-snapshot" || strings.HasPrefix(name, "--ws-")) {
				i++
			}
			continue
		}
		switch arg {
		case "generate-ts", "generate-json-schema", "help":
			return appServerTooling, nil
		case "daemon", "proxy":
			return appServerNative, errors.New("app-server daemon, proxy, and other subcommands are not supported by the stdio service")
		default:
			return 0, errors.New("app-server daemon, proxy, and other subcommands are not supported by the stdio service")
		}
	}
	return appServerStdio, nil
}

func appServerArgs(args []string) []string {
	result := append([]string(nil), args...)
	_, index := classifyArgs(args)
	mode, _ := appServerMode(args)
	if mode == appServerTooling {
		// Help, schema, and startup configuration are normal CLI tooling and
		// must not acquire server-only arguments.
		return result
	}
	hasListener := false
	for _, arg := range args[index+1:] {
		if arg == "--stdio" || arg == "--listen" || strings.HasPrefix(arg, "--listen=") {
			hasListener = true
		}
	}
	if !hasListener && mode != appServerNative {
		result = append(result, "--listen", "stdio://")
	}
	// Thread creation accepts SandboxMode rather than SandboxPolicy. The
	// service applies externalSandbox via thread/settings/update before tasks.
	// Explicit unsubscribe should unload idle runtimes promptly. Native
	// otherwise retains them for its inactivity grace period. Running work
	// is drained by the proxy.
	return append(result, "-c", `sandbox_mode="danger-full-access"`, "-c", `approval_policy="never"`, "-c", "thread_unload_delay_secs=0")
}

func environmentValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], key+"="); ok {
			return value
		}
	}
	return ""
}

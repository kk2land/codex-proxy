package proxy

import (
	"encoding/json"
	"errors"
	"strings"
)

const appServerLogFilename = "codex-app-server-proxy.log"
const maxAppServerMessage = 32 << 20

// The first line is a private launch envelope. It is consumed by the service,
// never forwarded to Codex or written to logs. Remaining lines are JSON-RPC.
type appServerStartup struct {
	Version   int      `json:"version"`
	Args      []string `json:"args"`
	Cwd       string   `json:"cwd"`
	Env       []string `json:"env"`
	CallerBin string   `json:"callerBin"`
}

func encodeAppServerStartup(request appServerStartup) ([]byte, error) {
	data, err := json.Marshal(request)
	if err != nil || len(data) >= maxAppServerMessage {
		return nil, errors.New("cannot encode app-server launch request")
	}
	return append(data, '\n'), nil
}

type appServerLaunchMode int

const (
	appServerStdio appServerLaunchMode = iota
	appServerTooling
)

// No alternate listener may bypass the service's task admission. Help and
// schema generators remain normal CLI tooling and do not create a server.
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
			if !attached && (name == "--config" || name == "-c" || name == "--enable" || name == "--disable" || name == "--code-mode-host" || strings.HasPrefix(name, "--ws-")) {
				i++
			}
			continue
		}
		switch arg {
		case "generate-ts", "generate-json-schema", "help":
			return appServerTooling, nil
		default:
			return 0, errors.New("app-server daemon, proxy, and other subcommands are not supported by the stdio service")
		}
	}
	return appServerStdio, nil
}

func appServerArgs(args []string) []string {
	result := append([]string(nil), args...)
	_, index := classifyArgs(args)
	hasListener := false
	for _, arg := range args[index+1:] {
		if arg == "--stdio" || arg == "--listen" || strings.HasPrefix(arg, "--listen=") {
			hasListener = true
		}
	}
	if !hasListener {
		result = append(result, "--listen", "stdio://")
	}
	// Thread creation accepts SandboxMode rather than SandboxPolicy. The
	// service applies externalSandbox via thread/settings/update before tasks.
	return append(result, "-c", `sandbox_mode="danger-full-access"`, "-c", `approval_policy="never"`)
}

func environmentValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], key+"="); ok {
			return value
		}
	}
	return ""
}

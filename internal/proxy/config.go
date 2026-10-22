package proxy

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	RealBin              string
	AppServerEnabled     bool
	AppServerIdleTimeout time.Duration
	MaxConcurrency       int
	WaitTimeout          time.Duration
	LogMaxSize           int64
	LogMaxBackups        int
}

type ConfigLoader func() (Config, error)

func DefaultConfig() Config {
	return Config{MaxConcurrency: 1, WaitTimeout: 10 * time.Minute, AppServerIdleTimeout: 10 * time.Minute, LogMaxSize: 10 << 20, LogMaxBackups: 5}
}

// LoadConfig reads only the account's env file, never process environment values.
func LoadConfig(dir, self string) (Config, error) {
	values, err := loadConfigValues(dir)
	if err != nil {
		return DefaultConfig(), err
	}
	return configFromValues(values, self)
}

// Disabled app-server calls retain the original refusal without requiring a
// configured native binary. No file or omitted flag means disabled. Once the
// flag is enabled, all normal configuration validation is required.
func loadAppServerConfig(dir, self string) (Config, error) {
	values, err := loadConfigValues(dir)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultConfig(), nil
	}
	if err != nil {
		return DefaultConfig(), err
	}
	enabled, err := appServerEnabledValue(values)
	if err != nil || !enabled {
		return DefaultConfig(), err
	}
	return configFromValues(values, self)
}

func loadConfigValues(dir string) (map[string]string, error) {
	f, err := openRegularRead(filepath.Join(dir, "env"))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	defer f.Close()
	return parseEnv(f)
}

func appServerEnabledValue(values map[string]string) (bool, error) {
	value, exists := values["CODEX_PROXY_APP_SERVER_ENABLED"]
	if !exists {
		return false, nil
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("CODEX_PROXY_APP_SERVER_ENABLED must be a boolean, e.g. true or false: %w", err)
	}
	return enabled, nil
}

func configFromValues(values map[string]string, self string) (Config, error) {
	cfg := DefaultConfig()
	var err error
	cfg.AppServerEnabled, err = appServerEnabledValue(values)
	if err != nil {
		return cfg, err
	}
	for key, value := range values {
		switch key {
		case "CODEX_PROXY_APP_SERVER_ENABLED":
			// Parsed before the remaining settings so errors are deterministic.
		case "CODEX_PROXY_REAL_BIN":
			cfg.RealBin = value
		case "CODEX_PROXY_APP_SERVER_IDLE_TIMEOUT":
			cfg.AppServerIdleTimeout, err = time.ParseDuration(value)
			if err != nil {
				return cfg, fmt.Errorf("%s must be a nonnegative duration, e.g. 30s, 10m, or 0: %w", key, err)
			}
			if cfg.AppServerIdleTimeout < 0 {
				return cfg, fmt.Errorf("%s must be a nonnegative duration, e.g. 30s, 10m, or 0: got %q", key, value)
			}
		case "CODEX_PROXY_MAX_CONCURRENCY":
			cfg.MaxConcurrency, err = strconv.Atoi(value)
			if err != nil {
				return cfg, fmt.Errorf("%s must be a positive integer: %w", key, err)
			}
			if cfg.MaxConcurrency < 1 {
				return cfg, fmt.Errorf("%s must be a positive integer: got %q", key, value)
			}
		case "CODEX_PROXY_WAIT_TIMEOUT":
			cfg.WaitTimeout, err = time.ParseDuration(value)
			if err != nil {
				return cfg, fmt.Errorf("%s must be a nonnegative duration, e.g. 30s, 10m, or 0: %w", key, err)
			}
			if cfg.WaitTimeout < 0 {
				return cfg, fmt.Errorf("%s must be a nonnegative duration, e.g. 30s, 10m, or 0: got %q", key, value)
			}
		case "CODEX_PROXY_LOG_MAX_SIZE_MIB":
			size, e := strconv.ParseInt(value, 10, 64)
			if e != nil {
				return cfg, fmt.Errorf("%s must be a positive integer MiB size without overflow: %w", key, e)
			}
			if size < 1 || size > math.MaxInt64/(1<<20) {
				return cfg, fmt.Errorf("%s must be a positive integer MiB size without overflow: got %q", key, value)
			}
			cfg.LogMaxSize = size << 20
		case "CODEX_PROXY_LOG_MAX_BACKUPS":
			cfg.LogMaxBackups, err = strconv.Atoi(value)
			if err != nil {
				return cfg, fmt.Errorf("%s must be a nonnegative integer: %w", key, err)
			}
			if cfg.LogMaxBackups < 0 {
				return cfg, fmt.Errorf("%s must be a nonnegative integer: got %q", key, value)
			}
		default:
			return cfg, fmt.Errorf("unknown config key %q", key)
		}
	}
	if cfg.RealBin == "" {
		return cfg, errors.New("CODEX_PROXY_REAL_BIN is required")
	}
	if err := validateNativeBinary(cfg.RealBin, self); err != nil {
		return cfg, fmt.Errorf("CODEX_PROXY_REAL_BIN: %w", err)
	}
	return cfg, nil
}

func parseEnv(r io.Reader) (map[string]string, error) {
	values := make(map[string]string)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 64*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if strings.HasPrefix(text, "export ") || strings.HasPrefix(text, "export\t") {
			text = strings.TrimSpace(text[len("export"):])
		}
		key, value, found := strings.Cut(text, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" || strings.IndexFunc(key, func(r rune) bool {
			return !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
		}) >= 0 {
			return nil, fmt.Errorf("config line %d: expected KEY=value", line)
		}
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("config line %d: duplicate key %q", line, key)
		}
		parsed, err := parseEnvValue(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("config line %d: %w", line, err)
		}
		if strings.ContainsRune(parsed, '\x00') {
			return nil, fmt.Errorf("config line %d: NUL is not allowed", line)
		}
		values[key] = parsed
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return values, nil
}

func parseEnvValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if value[0] != '\'' && value[0] != '"' {
		for i := 0; i < len(value); i++ {
			if value[i] == '#' && (i == 0 || value[i-1] == ' ' || value[i-1] == '\t') {
				return strings.TrimSpace(value[:i]), nil
			}
		}
		return value, nil
	}
	quote := value[0]
	end := -1
	for i := 1; i < len(value); i++ {
		if quote == '"' && value[i] == '\\' {
			i++
			continue
		}
		if value[i] == quote {
			end = i
			break
		}
	}
	if end < 0 {
		return "", errors.New("unterminated quoted value")
	}
	tail := strings.TrimSpace(value[end+1:])
	if tail != "" && !strings.HasPrefix(tail, "#") {
		return "", errors.New("unexpected text after quoted value")
	}
	if quote == '\'' {
		return value[1:end], nil
	}
	parsed, err := strconv.Unquote(value[:end+1])
	if err != nil {
		return "", fmt.Errorf("invalid escape in double-quoted value: %w", err)
	}
	return parsed, nil
}

func validateNativeBinary(path, self string) error {
	if !filepath.IsAbs(path) {
		return errors.New("must be an absolute path")
	}
	file, err := openRegularRead(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return errors.New("must be a regular executable file")
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return fmt.Errorf("identify proxy executable: %w", err)
	}
	if os.SameFile(info, selfInfo) {
		return errors.New("must not point to the proxy itself")
	}
	var header [4]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return fmt.Errorf("must be a native ELF or Mach-O binary: %w", err)
	}
	magic := binary.BigEndian.Uint32(header[:])
	switch magic {
	case 0x7f454c46, 0xfeedface, 0xcefaedfe, 0xfeedfacf, 0xcffaedfe,
		0xcafebabe, 0xbebafeca, 0xcafebabf, 0xbfbafeca:
		return nil
	default:
		return errors.New("must be a native ELF or Mach-O binary; shell and npm launch scripts are not supported")
	}
}

// Nonblocking open allows rejecting FIFOs/devices instead of hanging before
// the file type can be checked. Symlinks to real regular files remain allowed.
func openRegularRead(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("must be a regular file")
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

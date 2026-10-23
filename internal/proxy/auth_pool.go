package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const codexAccountsKey = "codex_accounts"

// Process exit statuses must fit in one byte; the protocol and diagnostic
// retain HTTP 429 while 75 (temporary failure) identifies a retryable exit.
const authLimitExitCode = 75

var authFilename = regexp.MustCompile(`^auth[0-9]+_.+\.json$`)
var errNoAuthAccount = errors.New("429: no available Codex accounts")

type authLimit struct {
	At    int64
	Until int64
}

// The cursor belongs to this process; only cooldowns are shared on disk.
type authPool struct {
	dir, home string
	store     *stateStore
	mu        sync.Mutex
	current   string
	advance   bool
	local     map[string]authLimit
	recordErr error
}

func newAuthPool(cfg Config, home string, store *stateStore) (*authPool, error) {
	if cfg.AuthDir == "" {
		return nil, nil
	}
	if !filepath.IsAbs(home) || !safeSandboxPath(home) {
		return nil, errors.New("multi-account mode requires an absolute CODEX_HOME")
	}
	dir, err := filepath.EvalSymlinks(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("open Codex auth directory: %w", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, errors.New("CODEX_PROXY_AUTH_DIR must name an accessible directory")
	}
	return &authPool{dir: dir, home: filepath.Clean(home), store: store, local: make(map[string]authLimit)}, nil
}

func decodeAccountStates(state proxyState) (map[string]json.RawMessage, error) {
	raw, exists := state[codexAccountsKey]
	if !exists {
		return make(map[string]json.RawMessage), nil
	}
	var accounts map[string]json.RawMessage
	if err := json.Unmarshal(raw, &accounts); err != nil || accounts == nil {
		return nil, errors.New("invalid codex_accounts in state: object is required")
	}
	return accounts, nil
}

func decodeAccountLimit(raw json.RawMessage) (map[string]json.RawMessage, authLimit, error) {
	fields := make(map[string]json.RawMessage)
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return nil, authLimit{}, errors.New("invalid Codex account state: object is required")
		}
	}
	var limit authLimit
	for key, target := range map[string]*int64{"cooldown_until": &limit.Until, "last_429_at": &limit.At} {
		if value, ok := fields[key]; ok {
			if string(value) == "null" || json.Unmarshal(value, target) != nil || *target < 0 {
				return nil, authLimit{}, fmt.Errorf("invalid %s in Codex account state", key)
			}
		}
	}
	return fields, limit, nil
}

func (p *authPool) choose() (string, error) {
	state, err := p.store.readState()
	if err != nil {
		return "", err
	}
	return p.chooseFrom(state)
}

func (p *authPool) chooseFrom(state proxyState) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.recordErr != nil {
		return "", p.recordErr
	}
	entries, err := os.ReadDir(p.dir) // ReadDir guarantees filename order.
	if err != nil {
		return "", fmt.Errorf("scan Codex auth directory: %w", err)
	}
	var names []string
	for _, entry := range entries {
		// Only real regular files: a source symlink can otherwise lead back to
		// CODEX_HOME/auth.json and create a cycle when that link is replaced.
		if authFilename.MatchString(entry.Name()) && entry.Type().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	accounts, err := decodeAccountStates(state)
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", errNoAuthAccount
	}
	start := sort.SearchStrings(names, p.current)
	if start < len(names) && names[start] == p.current && p.advance {
		start++
	}
	now := p.store.now().Unix()
	for n := 0; n < len(names); n++ {
		name := names[(start+n)%len(names)]
		_, limit, err := decodeAccountLimit(accounts[name])
		if err != nil {
			return "", fmt.Errorf("account %s: %w", name, err)
		}
		limit.Until = max(limit.Until, p.local[name].Until)
		if now >= limit.Until {
			return name, nil
		}
	}
	return "", errNoAuthAccount
}

func (p *authPool) active() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current
}

// Different proxy connections sharing CODEX_HOME share one mutable auth.json.
// Keep it pinned for the entire native process lifetime; other CODEX_HOMEs and account
// pool readers remain independent, so this does not prevent duplicate 429s.
func (p *authPool) homeLock() (*os.File, error) {
	if err := os.MkdirAll(p.home, 0700); err != nil {
		return nil, err
	}
	return openStateFile(filepath.Join(p.home, ".codex-proxy-auth.lock"), os.O_CREATE|os.O_RDWR)
}

func releaseAuthHome(file *os.File) error {
	// Normal completion releases copies inherited by native/background tools.
	// On proxy death, inherited copies instead keep the home pinned until the
	// task guardian has stopped native Codex.
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (p *authPool) acquireHome(ctx context.Context, timeout time.Duration) (*os.File, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	file, err := p.homeLock()
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			file.Close()
		}
	}()
	for {
		locked, err := tryAuthLock(ctx, file, syscall.LOCK_SH)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, ErrWaitTimeout
			}
			return nil, err
		}
		if locked {
			name, err := p.linkedAccount()
			if err != nil {
				return nil, err
			}
			state, err := p.store.readState()
			if err != nil {
				return nil, err
			}
			if name != "" {
				available, err := p.available(name, state)
				if err != nil {
					return nil, err
				}
				if available {
					p.mu.Lock()
					p.current, p.advance = name, false
					p.mu.Unlock()
					ok = true
					return file, nil
				}
			}
			// No native child exists yet. Explicitly drop our reader before
			// trying a writer; never depend on flock's upgrade semantics.
			if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
				return nil, err
			}
			writerAcquired, _, err := p.rotateFile(ctx, file)
			if err != nil {
				return nil, err
			}
			if !writerAcquired {
				return nil, fmt.Errorf("%w: another Codex process still pins the current credentials", errNoAuthAccount)
			}
			// Reacquire a reader and validate the actual link. Another writer
			// may have won the gap after our writer was released.
			continue
		}
		if err := sleepTaskInterval(ctx, p.store.poll); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, ErrWaitTimeout
			}
			return nil, err
		}
	}
}

func tryAuthLock(ctx context.Context, file *os.File, mode int) (bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		err := syscall.Flock(int(file.Fd()), mode|syscall.LOCK_NB)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, nil
		}
		return err == nil, err
	}
}

func (p *authPool) linkedAccount() (string, error) {
	link, err := filepath.EvalSymlinks(filepath.Join(p.home, "auth.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if filepath.Dir(link) != p.dir || !authFilename.MatchString(filepath.Base(link)) {
		return "", nil // A writer may replace an ordinary pre-pool login.
	}
	return filepath.Base(link), nil
}

func (p *authPool) available(name string, state proxyState) (bool, error) {
	info, err := os.Lstat(filepath.Join(p.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if name == "" || !authFilename.MatchString(name) || !info.Mode().IsRegular() {
		return false, nil
	}
	accounts, err := decodeAccountStates(state)
	if err != nil {
		return false, err
	}
	_, limit, err := decodeAccountLimit(accounts[name])
	if err != nil {
		return false, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.recordErr != nil {
		return false, p.recordErr
	}
	return p.store.now().Unix() >= max(limit.Until, p.local[name].Until), nil
}

func (p *authPool) checkCurrent(state proxyState) error {
	available, err := p.available(p.active(), state)
	if err != nil {
		return err
	}
	if !available {
		return errNoAuthAccount
	}
	return nil
}

// rotate is called only after the native child is gone and our reader is
// released. A failed nonblocking writer simply leaves rotation to a retry.
func (p *authPool) rotate() (bool, string, error) {
	file, err := p.homeLock()
	if err != nil {
		return false, "", err
	}
	defer file.Close()
	return p.rotateFile(context.Background(), file)
}

func (p *authPool) rotateFile(ctx context.Context, file *os.File) (bool, string, error) {
	locked, err := tryAuthLock(ctx, file, syscall.LOCK_EX)
	if err != nil || !locked {
		return false, "", err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	state, err := p.store.readState()
	if err != nil {
		return true, "", err
	}
	linked, err := p.linkedAccount()
	if err != nil {
		return true, "", err
	}
	if linked != "" {
		available, err := p.available(linked, state)
		if err != nil {
			return true, "", err
		}
		if available {
			return true, linked, nil // Someone else already selected a healthy account.
		}
		p.mu.Lock()
		p.current, p.advance = linked, true
		p.mu.Unlock()
	}
	name, err := p.chooseFrom(state)
	if err != nil {
		return true, "", err
	}
	return true, name, p.activate(name)
}

func (p *authPool) activate(name string) error {
	if !authFilename.MatchString(name) || filepath.Base(name) != name {
		return errors.New("invalid Codex auth filename")
	}
	source := filepath.Join(p.dir, name)
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("selected Codex auth must be a regular file")
	}
	if err := os.MkdirAll(p.home, 0700); err != nil {
		return fmt.Errorf("create CODEX_HOME: %w", err)
	}
	target := filepath.Join(p.home, "auth.json")
	// Preserve an existing ordinary login file before the first replacement.
	if info, err := os.Lstat(target); err == nil {
		if info.Mode().IsRegular() {
			backup := target + ".codex-proxy-backup"
			if err := os.Link(target, backup); err != nil && !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("back up existing auth.json: %w", err)
			}
		} else if info.Mode()&os.ModeSymlink == 0 {
			return errors.New("CODEX_HOME/auth.json must be a regular file or symlink")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect auth.json: %w", err)
	}
	tmp := filepath.Join(p.home, ".codex-proxy-auth-"+newRequestID())
	if err := os.Symlink(source, tmp); err != nil {
		return fmt.Errorf("create auth.json symlink: %w", err)
	}
	defer os.Remove(tmp)
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("replace auth.json symlink: %w", err)
	}
	p.mu.Lock()
	p.current, p.advance = name, false
	p.mu.Unlock()
	return nil
}

// note is immediate and local, so another task cannot reuse this account while
// the asynchronous cross-process transaction is waiting for state.lock.
func (p *authPool) note(name string, limit authLimit) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.local[name]
	p.local[name] = authLimit{At: max(old.At, limit.At), Until: max(old.Until, limit.Until)}
	if name == p.current {
		p.advance = true
	}
}

func (p *authPool) record(ctx context.Context, name string, limit authLimit) (err error) {
	defer func() {
		if err != nil {
			p.mu.Lock()
			p.recordErr = fmt.Errorf("cannot persist Codex account cooldown: %w", err)
			p.mu.Unlock()
		}
	}()
	lease, err := p.store.lock(ctx)
	if err != nil {
		return err
	}
	defer lease.Close()
	accounts, err := decodeAccountStates(lease.state)
	if err != nil {
		return err
	}
	fields, old, err := decodeAccountLimit(accounts[name])
	if err != nil {
		return err
	}
	if old.At >= limit.At && old.Until >= limit.Until {
		return nil
	}
	fields["cooldown_until"], _ = json.Marshal(max(old.Until, limit.Until))
	fields["last_429_at"], _ = json.Marshal(max(old.At, limit.At))
	accounts[name], _ = json.Marshal(fields)
	lease.state[codexAccountsKey], _ = json.Marshal(accounts)
	return lease.write(ctx)
}

func authFileArgs(args []string) []string {
	result := []string{"-c", `cli_auth_credentials_store="file"`}
	// Remove contradictory explicit CLI overrides, preserving literal prompts
	// after -- and values belonging to other known options.
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(result, args[i:]...)
		}
		if arg == "-c" || arg == "--config" {
			if i+1 < len(args) {
				if key, _, ok := strings.Cut(args[i+1], "="); ok && strings.TrimSpace(key) == "cli_auth_credentials_store" {
					i++
					continue
				}
				result = append(result, arg, args[i+1])
				i++
				continue
			}
		}
		value := ""
		if strings.HasPrefix(arg, "--config=") {
			value = strings.TrimPrefix(arg, "--config=")
		} else if strings.HasPrefix(arg, "-c") {
			value = strings.TrimPrefix(strings.TrimPrefix(arg, "-c"), "=")
		}
		if key, _, ok := strings.Cut(value, "="); ok && strings.TrimSpace(key) == "cli_auth_credentials_store" {
			continue
		}
		result = append(result, arg)
	}
	return result
}

func defaultAuthLimit(now time.Time) authLimit {
	return authLimit{At: now.Unix(), Until: now.Add(time.Minute).Unix()}
}

func authLaunch(spec launchSpec, pool *authPool) (launchSpec, error) {
	if spec.profile == "" || pool == nil || pool.active() == "" {
		return spec, nil
	}
	profile, err := sandboxProfileWithAuth(pool.home, filepath.Join(pool.dir, pool.active()))
	if err != nil {
		return spec, err
	}
	spec.profile = profile
	return spec, nil
}

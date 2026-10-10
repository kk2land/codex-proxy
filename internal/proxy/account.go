//go:build (darwin && !osusergo) || (linux && cgo && !osusergo)

package proxy

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

// SystemHome uses the system account database; HOME and USER are irrelevant.
func SystemHome() (string, error) {
	account, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		return "", fmt.Errorf("look up current UID: %w", err)
	}
	if !filepath.IsAbs(account.HomeDir) {
		return "", fmt.Errorf("system account has no absolute home directory")
	}
	return account.HomeDir, nil
}

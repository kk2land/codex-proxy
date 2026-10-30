//go:build darwin && !cgo

package proxy

import "errors"

func processSandboxed() (bool, error) {
	return false, errors.New("macOS sandbox detection requires CGO_ENABLED=1")
}

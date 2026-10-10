//go:build (linux && (!cgo || osusergo)) || (darwin && osusergo)

package proxy

import "errors"

func SystemHome() (string, error) {
	return "", errors.New("system account lookup requires Linux CGO_ENABLED=1 and no osusergo build tag (macOS also requires no osusergo tag)")
}

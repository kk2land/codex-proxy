//go:build darwin && cgo

package proxy

/*
#cgo LDFLAGS: -lsandbox
#include <stddef.h>
#include <unistd.h>

// This SPI is also used by Chromium's Seatbelt::IsSandboxed(). It checks
// the kernel's sandbox state, including a sandbox inherited across exec.
extern int sandbox_check(pid_t pid, const char *operation, int type, ...);

static int proxy_sandbox_check(void) {
    return sandbox_check(getpid(), NULL, 0);
}
*/
import "C"

import "fmt"

func processSandboxed() (bool, error) {
	result, err := C.proxy_sandbox_check()
	switch result {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		if err != nil {
			return false, fmt.Errorf("check process sandbox: %w", err)
		}
		return false, fmt.Errorf("check process sandbox: unexpected result %d", result)
	}
}

package proxy

import (
	"strings"
	"testing"
)

func TestAppServerLaunchTransportAndTooling(t *testing.T) {
	for _, test := range []struct {
		args     []string
		mode     appServerLaunchMode
		rejected bool
	}{
		{[]string{"app-server"}, appServerStdio, false},
		{[]string{"app-server", "--stdio"}, appServerStdio, false},
		{[]string{"app-server", "--listen=stdio://"}, appServerStdio, false},
		{[]string{"app-server", "--ws-issuer", "--help"}, appServerStdio, false},
		{[]string{"app-server", "--help"}, appServerTooling, false},
		{[]string{"app-server", "generate-ts", "--out", "/tmp/types"}, appServerTooling, false},
		{[]string{"app-server", "--listen", "unix:///tmp/server.sock"}, appServerStdio, true},
		{[]string{"app-server", "--listen", "off"}, appServerStdio, true},
		{[]string{"app-server", "--listen"}, appServerStdio, true},
		{[]string{"app-server", "-c", "setting=value", "daemon"}, appServerStdio, true},
	} {
		mode, err := appServerMode(test.args)
		if (err != nil) != test.rejected || (err == nil && mode != test.mode) {
			t.Errorf("appServerMode(%q) = %v, %v", test.args, mode, err)
		}
		if !test.rejected && mode == appServerStdio {
			args := appServerArgs(test.args)
			listeners := 0
			for _, arg := range args {
				if arg == "--stdio" || arg == "--listen" || strings.HasPrefix(arg, "--listen=") {
					listeners++
				}
			}
			if listeners != 1 {
				t.Errorf("duplicate or missing listener: %q", args)
			}
		}
	}
}

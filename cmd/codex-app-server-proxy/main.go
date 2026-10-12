package main

import (
	"os"
	"strconv"

	"codex-proxy/internal/proxy"
)

func main() {
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case "--daemon":
			os.Exit(proxy.RunAppServerDaemon(proxy.Options{}))
		case "--connect":
			os.Exit(proxy.RunAppServerClient(proxy.Options{}))
		}
	}
	if len(os.Args) == 3 && os.Args[1] == "--guard" {
		pid, err := strconv.Atoi(os.Args[2])
		if err != nil {
			os.Exit(2)
		}
		os.Exit(proxy.RunAppServerGuard(pid))
	}
	if len(os.Args) != 1 {
		os.Exit(2)
	}
	os.Exit(proxy.RunAppServer(proxy.Options{}))
}

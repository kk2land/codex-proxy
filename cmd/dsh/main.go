package main

import (
	"os"

	"codex-proxy/internal/proxy"
)

func main() {
	os.Exit(proxy.RunDSH(os.Args[1:]))
}

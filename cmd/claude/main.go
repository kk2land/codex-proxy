package main

import (
	"os"

	"codex-proxy/internal/proxy"
)

func main() {
	os.Exit(proxy.RunClaude(os.Args[1:], proxy.Options{}))
}

package main

import (
	"os"

	"codex-proxy/internal/proxy"
)

func main() {
	os.Exit(proxy.RunPI(os.Args[1:]))
}

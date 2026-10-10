package main

import (
	"os"

	"codex-proxy/internal/proxy"
)

func main() {
	os.Exit(proxy.Run(os.Args[1:], proxy.Options{}))
}

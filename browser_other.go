//go:build !windows

package main

import (
	"os/exec"
	"runtime"
)

func openBrowser(url string) error {
	bin := "xdg-open"
	if runtime.GOOS == "darwin" {
		bin = "open"
	}
	return exec.Command(bin, url).Start()
}

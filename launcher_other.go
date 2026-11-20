//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

// On Linux/macOS there's no launcher: run the backend here and print the
// address. Handy for development and for testing with loop devices.
func launch() {
	err := runBackend(func(url string) {
		fmt.Println("DragonClone is running at", url)
		if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
			exec.Command("xdg-open", url).Start()
		}
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

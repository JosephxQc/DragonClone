// DragonClone - sector-by-sector disk cloning, imaging and restoring for Windows.
package main

import (
	"os"

	"dragonclone/core"
)

var version = "1.0.0"

func main() {
	core.AppVersion = version
	backend := false
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--backend":
			backend = true
		case "--data":
			if i+1 < len(args) {
				i++
				core.SetDataDir(args[i])
			}
		}
	}
	// Two halves: the elevated backend that does the disk work, and the
	// launcher the user double-clicks, which starts the backend (one UAC
	// prompt) and opens the window.
	if backend {
		if err := runBackend(nil); err != nil {
			os.Exit(1)
		}
		return
	}
	launch()
}

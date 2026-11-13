// Command gen writes the generated icon files into the directory given as
// its only argument. Run it through `go generate ./internal/brand`.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/mrRedSun/redrop/internal/brand/iconsrc"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gen <output dir>")
		os.Exit(2)
	}
	for name, data := range iconsrc.Files() {
		if err := os.WriteFile(filepath.Join(os.Args[1], name), data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

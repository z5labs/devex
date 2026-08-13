// Command payload is the tree-shaped-payload fixture: an executable that is
// useless without the files beside it in the image.
//
// It exists so a test can assert that an App assembled from a prebuilt
// executable plus the tree it needs really composes — the entry alone would
// pass an exec check while the files it depends on were missing, which is the
// failure a single-file payload can never surface.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// payloadDir is where the test contributes the tree. It is a constant in the
// program rather than an environment variable because a layout fact for a
// program you do control belongs in the program — the archetype's package doc
// makes the same point from the other side.
const payloadDir = "/srv/payload"

func main() {
	var names []string
	err := filepath.WalkDir(payloadDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(payloadDir, path)
		if err != nil {
			return err
		}
		names = append(names, rel)
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "payload:", err)
		os.Exit(1)
	}
	greeting, err := os.ReadFile(filepath.Join(payloadDir, "greeting.txt"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "payload:", err)
		os.Exit(1)
	}
	sort.Strings(names)
	fmt.Printf("%s %s\n", strings.TrimSpace(string(greeting)), strings.Join(names, ","))
}

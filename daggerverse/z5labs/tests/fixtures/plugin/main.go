// Command plugin is the composition fixture: an executable that exists to be
// composed into another application's image.
//
// It prints something no other fixture prints, so a test can tell "the plugin
// ran" from "the base's own binary ran" — the two land in one image and an
// assertion that could not distinguish them would pass for a composition that
// copied nothing at all.
package main

import "fmt"

func main() {
	fmt.Println("plugin ok")
}

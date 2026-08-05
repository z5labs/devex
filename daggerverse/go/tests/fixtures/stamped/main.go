package main

import "fmt"

// version and commit are package-level string vars so `-ldflags -X` can
// assign them at link time. The defaults are what an unstamped build
// reports.
var (
	version = "dev"
	commit  = "none"
)

func main() { fmt.Printf("version=%s commit=%s flavor=%s\n", version, commit, flavor) }

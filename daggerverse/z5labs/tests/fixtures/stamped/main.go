package main

import "fmt"

// version and commit are the two package-level vars GoApp stamps at link
// time. The defaults are what an unstamped build would report.
var (
	version = "dev"
	commit  = "none"
)

func main() { fmt.Printf("version=%s commit=%s\n", version, commit) }

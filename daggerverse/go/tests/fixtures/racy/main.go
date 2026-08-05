package main

import "fmt"

// The race detector is not something a program can query at runtime, so this
// fixture reports it at build time instead: `go build -race` implies the
// `race` build tag, which selects detector_on.go over detector_off.go.
func main() { fmt.Printf("race=%s\n", raceDetector) }

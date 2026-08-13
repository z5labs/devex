// Package racelib is a fixture library whose test carries a deliberate
// data race. It compiles, vets and lints clean, and `go test` passes; only
// `go test -race` fails it. That is what makes it able to tell the two
// apart.
package racelib

// Add returns a + b.
func Add(a, b int) int {
	return a + b
}

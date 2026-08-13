// Package main is the build-tag fixture: a program whose output says which
// build tags it was compiled with.
//
// It exists so that GoChain.WithBuild can be asserted to reach the compiler
// rather than merely to be accepted. Two files below are selected by a
// `//go:build` constraint and exactly one of them is compiled into any given
// binary, so the program's own output is the evidence.
package main

import "fmt"

func main() {
	fmt.Println(variant())
}

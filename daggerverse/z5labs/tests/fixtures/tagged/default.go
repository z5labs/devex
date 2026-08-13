//go:build !integration

package main

// variant reports the build with no tags, which is what a caller who never
// calls WithBuild must get.
func variant() string { return "variant=default" }

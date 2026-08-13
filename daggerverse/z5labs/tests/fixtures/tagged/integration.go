//go:build integration

package main

// variant reports the build with `-tags integration`, which is only
// reachable if WithBuild's tags reached the Go toolchain.
func variant() string { return "variant=integration" }

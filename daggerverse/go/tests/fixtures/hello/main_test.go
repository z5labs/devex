package main

import "testing"

func TestGreet(t *testing.T) {
	if got := Greet(); got != "hello" {
		t.Errorf("Greet() = %q, want %q", got, "hello")
	}
}

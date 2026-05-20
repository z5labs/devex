package main

import "testing"

func TestGreet(t *testing.T) {
	if got, want := Greet(), "hello"; got != want {
		t.Errorf("Greet() = %q, want %q", got, want)
	}
}

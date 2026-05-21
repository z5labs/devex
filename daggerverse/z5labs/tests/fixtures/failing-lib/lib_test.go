package failinglib

import "testing"

func TestGreetFails(t *testing.T) {
	if got, want := Greet(), "goodbye"; got != want {
		t.Errorf("Greet() = %q, want %q", got, want)
	}
}

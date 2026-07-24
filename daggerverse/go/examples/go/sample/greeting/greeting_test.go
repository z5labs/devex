package greeting

import "testing"

func TestFor(t *testing.T) {
	if got, want := For("world"), "hello, world!"; got != want {
		t.Errorf("For(%q) = %q, want %q", "world", got, want)
	}
}

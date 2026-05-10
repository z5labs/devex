package foo

import "testing"

func TestBar(t *testing.T) {
	if got := Bar(); got != "bar" {
		t.Errorf("Bar() = %q, want %q", got, "bar")
	}
}

// Package greeting is a second package in the sample module, so recipes that
// pass `./...` visibly cover more than the main package.
package greeting

import "fmt"

// For returns the greeting for name.
func For(name string) string {
	return fmt.Sprintf("hello, %s!", name)
}

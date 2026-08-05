// A main package whose only purpose is to be compiled with
// -buildmode=c-archive or -buildmode=c-shared: those modes export the
// functions carrying a cgo //export comment and ignore main, which must
// nonetheless exist for the package to be a main package.
package main

// #include <stdlib.h>
import "C"

//export Greet
func Greet() *C.char {
	return C.CString("hello from go")
}

func main() {}

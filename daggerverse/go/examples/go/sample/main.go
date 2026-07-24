// Command greeter is the sample program the cookbook recipes build, test and
// tidy. It is deliberately tiny and depends only on the standard library, so
// every recipe runs without network access to a module proxy.
package main

import (
	"fmt"

	"example.com/greeter/greeting"
)

func main() {
	fmt.Println(greeting.For("world"))
}

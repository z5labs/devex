// Package main is the SBOM fixture: a main package with one external
// dependency, so a document generated from the built binary has a
// component to describe and a licence file to classify. The dependency
// is deliberately tiny and permissively licensed — the point is that it
// is *there*, not what it does.
package main

import (
	"fmt"

	"github.com/google/uuid"
)

func main() {
	fmt.Println(uuid.NameSpaceDNS.String())
}

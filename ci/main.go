// Ci is the root module that aggregates each daggerverse module's tests
// suite as a toolchain. With toolchains, `dagger check -l` enumerates
// every dep's +check functions directly (e.g. kafka-tests:all), so no
// wrapper methods are needed here.
package main

type Ci struct{}

package main

//go:generate sh -c "echo generated > out.txt"

import "fmt"

func main() { fmt.Println(Greet()) }

func Greet() string { return "hello" }

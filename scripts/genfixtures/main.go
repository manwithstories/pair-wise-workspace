package main

import (
	"fmt"
	"os"

	"loudness/internal/gen"
)

func main() {
	dir := "fixtures"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	files, err := gen.WriteSuite(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, name := range files {
		fmt.Println(dir + "/" + name)
	}
}

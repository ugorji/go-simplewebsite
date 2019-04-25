package main

import (
	"log"
	"os"

	"github.com/ugorji/go-simplewebsite/simplewebsite"
)

func main() {
	// runtime.GOMAXPROCS(runtime.NumCPU()) // done by default
	if err := simplewebsite.Main(os.Args[1:]); err != nil {
		log.Fatalf("Main Error: %v\n", err)
	}
}

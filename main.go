package main

import (
	"log"
	"os"

	"github.com/ziinaio/ziina/cmd/ziina"
)

func main() {
	if err := ziina.App.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}


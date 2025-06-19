package main

import (
	"log"
	"os"

	"github.com/ziinaio/zmate/cmd/zmate"
)

func main() {
	if err := zmate.App.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

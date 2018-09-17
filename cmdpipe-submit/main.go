package main

import (
	"os"

	"github.com/washtubs/cmdpipe"
)

func main() {
	os.Exit(cmdpipe.Send())
}

package main

import (
	"fmt"

	"github.com/washtubs/cmdpipe"
)

func main() {
	fmt.Println("server")
	cmdpipe.Receive()
}

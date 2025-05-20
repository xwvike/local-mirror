package main

import (
	"flag"
	"fmt"
	"local-mirror/internal/app"
)

func main() {
	fmt.Println("main init")
	flag.Parse()
	app.App()
}

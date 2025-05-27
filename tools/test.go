package main

import (
	"fmt"
	"strings"
)

func main() {
	var path = "./dist/local-mirror"

	fmt.Print(strings.Replace(path, "./dist", ".", 1))
}

package config

import (
	"flag"
)

var (
	Mode = flag.String("mode", "real", "运行模式: real 或 mirror")
)

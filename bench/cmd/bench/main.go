// bench 对 local-mirror 与其他同步程序做可复现的对比测试。
//
//	bench gen   -ws <dir> [-datasets small,mixed,large]
//	bench run   -ws <dir> -out <file.jsonl> -local-mirror <bin> [-rsync <bin>] ...
//	bench chart -out <file.md> <file.jsonl>...
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"local-mirror/bench/internal/dataset"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: bench gen|run|chart [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = cmdGen(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "chart":
		err = cmdChart(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func cmdGen(args []string) error {
	fl := flag.NewFlagSet("gen", flag.ExitOnError)
	ws := fl.String("ws", "", "workspace directory")
	datasets := fl.String("datasets", "small,mixed,large", "datasets to generate")
	fl.Parse(args)
	if *ws == "" {
		return errors.New("-ws is required")
	}
	for _, name := range strings.Split(*datasets, ",") {
		spec, err := dataset.Get(name)
		if err != nil {
			return err
		}
		start := time.Now()
		entries, err := dataset.Generate(spec, filepath.Join(*ws, "data", name))
		if err != nil {
			return err
		}
		var total int64
		for _, e := range entries {
			total += e.Size
		}
		logf("%s: %d files, %.2f GiB (%s)", name, len(entries), float64(total)/(1<<30), time.Since(start).Round(time.Second))
	}
	return nil
}

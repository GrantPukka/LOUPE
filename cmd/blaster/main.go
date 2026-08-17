// Command blaster generates realistic, messy, multi-format log files for
// testing and demoing loupe.
//
//	go run ./cmd/blaster -out ./demo -scenario incident
//	go run ./cmd/blaster -out ./demo -follow -rate 40
//	go run ./cmd/blaster -out ./testdata/mixed -seed 7 -duration 5m -malform 0.02
//
// The generator itself lives in internal/blaster so that `loupe demo` can use
// it too. This file is only the flag parsing.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/GrantPukka/loupe/internal/blaster"
)

func main() {
	c := blaster.Defaults()
	flag.StringVar(&c.Out, "out", c.Out, "output directory")
	flag.StringVar(&c.Scenario, "scenario", c.Scenario,
		"steady | incident | deploy-regression | quiet")
	flag.DurationVar(&c.Duration, "duration", c.Duration, "span of simulated time")
	flag.Float64Var(&c.Rate, "rate", c.Rate, "baseline requests per simulated second")
	flag.Int64Var(&c.Seed, "seed", c.Seed, "rng seed; same seed gives byte-identical output")
	flag.Float64Var(&c.Malform, "malform", c.Malform,
		"fraction of lines that are truncated, invalid, or otherwise broken")
	flag.BoolVar(&c.Follow, "follow", c.Follow, "write in real time instead of all at once")
	flag.BoolVar(&c.Rotate, "rotate", c.Rotate, "also emit rotated .log.1 and .log.2.gz files")
	flag.Parse()

	c.Report = os.Stdout
	if err := blaster.Run(c); err != nil {
		fmt.Fprintln(os.Stderr, "blaster:", err)
		os.Exit(1)
	}
	if !c.Follow {
		fmt.Printf("\n  loupe %s --ui\n", c.Out)
	}
}

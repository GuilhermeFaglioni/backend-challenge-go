package main

import (
	"fmt"
	"os"

	"go.uber.org/fx"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/app"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration:\n%v\n", err)
		os.Exit(1)
	}
	// Run blocks until SIGINT/SIGTERM and then runs the OnStop hooks.
	fx.New(app.New(cfg)).Run()
}

package main

import (
	"fmt"
	"os"

	"github.com/LXXero/xerotty/internal/app"
	"github.com/LXXero/xerotty/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "xerotty: config error: %v\n", err)
		os.Exit(1)
	}

	a := app.New(cfg)
	if err := a.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "xerotty: %v\n", err)
		os.Exit(1)
	}
}

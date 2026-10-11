package main

import (
	"context"
	"flag"
	"fmt"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/config"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	mode := flag.String("mode", "", "runtime mode: all, api or worker")
	runFor := flag.Duration("run-for", 0, "optional bounded local runtime duration")
	flag.Parse()
	lookup := func(key string) (string, bool) {
		if key == "LOREVIVA_MODE" && *mode != "" {
			return *mode, true
		}
		return os.LookupEnv(key)
	}
	cfg, err := config.LoadConfig(lookup)
	if err == nil {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if flag.NArg() > 0 {
			err = command(ctx, cfg, flag.Args(), os.Stdout)
		} else {
			if *runFor > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, *runFor)
				defer cancel()
			}
			var r *app.Runtime
			var closeRuntime func()
			r, closeRuntime, err = runtime(cfg)
			if err == nil {
				defer closeRuntime()
				err = app.Run(ctx, cfg, r.Dependencies(cfg))
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

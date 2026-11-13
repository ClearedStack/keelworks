// Command keelworks is the Keelworks controller.
//
// Today it runs only against in-memory stand-ins (-standin FILE). The
// launcher for a real Kubernetes cluster is not built yet, and without
// -standin the command refuses to start rather than pretend.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/clearedstack/keelworks/internal/controller"
	"github.com/clearedstack/keelworks/internal/standin"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "keelworks:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("keelworks", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "address to listen on")
	script := fs.String("standin", "", "run against the in-memory stand-in cluster, playing back this script `file`")
	var cfg controller.Config
	fs.StringVar(&cfg.TestImage, "test-image", "", "image that fetches a commit and runs its tests (required)")
	fs.StringVar(&cfg.PriorityClass, "priority-class", "", "PriorityClass for test Jobs (required)")
	fs.DurationVar(&cfg.JobTimeout, "job-timeout", 0, "how long a test Job may take, for example 10m (required)")
	fs.IntVar(&cfg.MaxRuns, "max-runs", 256, "how many runs are remembered")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if *script == "" {
		return errors.New("the launcher for a real cluster is not built yet; run with -standin FILE")
	}
	data, err := os.ReadFile(*script)
	if err != nil {
		return errors.New("the stand-in script could not be read")
	}
	parsed, err := standin.ParseScript(data)
	if err != nil {
		return err
	}
	ctl, err := controller.New(cfg, standin.NewCluster(parsed))
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           ctl,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	failed := make(chan error, 1)
	go func() { failed <- srv.ListenAndServe() }()
	fmt.Fprintln(os.Stderr, "keelworks: listening with the in-memory stand-in cluster")

	select {
	case <-failed:
		ctl.Close()
		return errors.New("the listener stopped")
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	ctl.Close()
	return nil
}

// Command toll is a stateless egress gateway that meters LLM token usage
// per user, API key and model.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"syscall"
	"time"

	"github.com/sokingdom/toll/internal/config"
	"github.com/sokingdom/toll/internal/events"
	"github.com/sokingdom/toll/internal/metrics"
	"github.com/sokingdom/toll/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "toll:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", os.Getenv("TOLL_CONFIG"), "path to the YAML config file (env TOLL_CONFIG)")
	flag.Parse()

	// toll's own logs go to stderr; stdout is reserved for events.
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Without this, Go exits the process when a write to a closed stdout pipe fails.
	signal.Ignore(syscall.SIGPIPE)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	var writer events.Writer
	switch cfg.Events.Output {
	case config.OutputStdout:
		writer = events.NewStreamWriter(os.Stdout)
	default:
		writer, err = events.NewFileWriter(cfg.Events.Dir, cfg.Events.Filename, cfg.Server.InstanceID, cfg.Events.Rotate)
		if err != nil {
			return fmt.Errorf("events: %w", err)
		}
	}
	defer writer.Close()

	srv, err := server.New(server.Options{
		Config:  cfg,
		Events:  writer,
		Metrics: metrics.New(cfg.Metering.MetricTags),
		Logger:  log,
	})
	if err != nil {
		return err
	}

	baseCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	httpServer := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           srv,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("toll listening", "addr", cfg.Server.Listen, "events", cfg.Events.Output, "instance", cfg.Server.InstanceID)
		serveErr <- httpServer.ListenAndServe()
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	for {
		select {
		case err := <-serveErr:
			return err
		case sig := <-signals:
			if sig == syscall.SIGHUP {
				cfg = reload(log, srv, *configPath, cfg)
				continue
			}
			return shutdown(log, httpServer, srv, cancelRequests, cfg.Server.ShutdownGrace)
		}
	}
}

func reload(log *slog.Logger, srv *server.Server, path string, current *config.Config) *config.Config {
	next, err := config.Load(path)
	if err != nil {
		log.Error("config reload failed; keeping current config", "err", err)
		return current
	}
	if err := srv.Reload(next); err != nil {
		log.Error("config reload failed; keeping current config", "err", err)
		return current
	}
	// These settings are bound at startup.
	if next.Server.Listen != current.Server.Listen ||
		next.Server.InstanceID != current.Server.InstanceID ||
		next.Server.UpstreamResponseHeaderTimeout != current.Server.UpstreamResponseHeaderTimeout ||
		next.Events.Output != current.Events.Output || next.Events.Dir != current.Events.Dir ||
		next.Events.Filename != current.Events.Filename || next.Events.Rotate != current.Events.Rotate ||
		!reflect.DeepEqual(next.Metering.MetricTags, current.Metering.MetricTags) {
		log.Warn("config reloaded; listen, instance_id, upstream timeout, events output and metric_tags changes need a restart")
	} else {
		log.Info("config reloaded")
	}
	return next
}

// shutdown stops accepting connections and waits up to grace for in-flight
// streams. Streams still running after that are cancelled and recorded as
// partial before the process exits.
func shutdown(log *slog.Logger, httpServer *http.Server, srv *server.Server, cancelRequests context.CancelFunc, grace time.Duration) error {
	log.Info("shutting down", "grace", grace.String())
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	err := httpServer.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		log.Warn("grace period over; cancelling in-flight requests")
		cancelRequests()
		done := make(chan struct{})
		go func() { srv.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			log.Error("in-flight requests did not finish after cancellation")
		}
		return nil
	}
	srv.Wait()
	return err
}

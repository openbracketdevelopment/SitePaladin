package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/openbracketdevelopment/bytedown/internal/api"
	"github.com/openbracketdevelopment/bytedown/internal/config"
	"github.com/openbracketdevelopment/bytedown/internal/database"
	"github.com/openbracketdevelopment/bytedown/internal/jobs"
	"github.com/openbracketdevelopment/bytedown/internal/processing"
	"github.com/openbracketdevelopment/bytedown/internal/storage"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		// Detailed native/database errors may contain paths or credentials.
		slog.Error("command failed; verify configuration, database, storage, and codec availability")
		os.Exit(1)
	}
}

func run() error {
	if handled, err := processing.HandleCommand(os.Args[1:]); handled {
		return err
	}
	if len(os.Args) < 2 {
		return errors.New("usage: bytedown api|worker|migrate|probe|health|version")
	}
	if os.Args[1] == "version" {
		fmt.Printf("ByteDown %s pipeline %s\n", api.Version, processing.PipelineVersion)
		return nil
	}
	if os.Args[1] == "health" {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if len(os.Args) > 2 && os.Args[2] == "worker" {
			c, err := config.Load()
			if err != nil {
				return err
			}
			db, err := database.Open(ctx, c.DatabaseURL)
			if err != nil {
				return err
			}
			defer db.Close()
			files, err := storage.New(c.StorageDir)
			if err != nil {
				return err
			}
			if err := files.Ready(); err != nil {
				return err
			}
			return processing.Probe(ctx)
		}
		address := os.Getenv("LISTEN")
		if address == "" {
			address = ":8080"
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/health/ready", nil)
		if err != nil {
			return err
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return errors.New("not ready")
		}
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if os.Args[1] == "probe" {
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := processing.Probe(probeCtx); err != nil {
			slog.Error("required JPEG/PNG/WebP/AVIF encode/decode capability probe failed")
			return err
		}
		fmt.Println("JPEG PNG WebP AVIF encode/decode available")
		return nil
	}
	if os.Args[1] != "api" && os.Args[1] != "worker" && os.Args[1] != "migrate" {
		return errors.New("unknown command")
	}
	c, err := config.Load()
	if err != nil {
		slog.Error("invalid environment configuration", "reason", err.Error())
		return err
	}
	db, err := database.Open(ctx, c.DatabaseURL)
	if err != nil {
		slog.Error("database connection unavailable")
		return err
	}
	defer db.Close()
	if os.Args[1] == "migrate" {
		return db.Migrate(ctx)
	}
	files, err := storage.New(c.StorageDir)
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = processing.Probe(probeCtx)
	cancel()
	if err != nil {
		slog.Error("required JPEG/PNG/WebP/AVIF encode/decode capability probe failed")
		return err
	}
	if os.Args[1] == "worker" {
		w := jobs.Worker{Config: c, DB: db, Files: files}
		return w.Run(ctx)
	}
	server := &http.Server{
		Addr: c.Listen, Handler: api.New(c, db, files).Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 60 * time.Second,
		WriteTimeout: time.Duration(c.Limits.ProcessingSeconds+65) * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				_ = server.Close()
			}
		case <-done:
		}
	}()
	slog.Info("API listening", "address", c.Listen, "version", api.Version)
	err = server.ListenAndServe()
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

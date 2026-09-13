package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("load prompt-audit config: %v", err)
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := OpenStore(rootCtx, cfg.AuditDatabaseURL)
	if err != nil {
		log.Fatalf("open prompt-audit store: %v", err)
	}
	defer store.Close()

	resolver, err := NewIdentityResolver(rootCtx, cfg)
	if err != nil {
		log.Fatalf("open prompt-audit identity resolver: %v", err)
	}
	defer resolver.Close()
	resolver.Start(rootCtx)

	queue := NewRecordQueue(cfg.QueueSize)
	workerCtx, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		queue.Run(workerCtx, store, resolver, cfg)
	}()
	if cfg.AuditRetentionDays > 0 {
		go runRetention(rootCtx, store, cfg.AuditRetentionDays)
	}

	proxy, err := NewProxyServer(cfg, resolver, store, queue)
	if err != nil {
		log.Fatalf("create prompt-audit proxy: %v", err)
	}
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("prompt-audit server shutdown: %v", err)
		}
		queue.Close()
		select {
		case <-workerDone:
		case <-shutdownCtx.Done():
			stopWorker()
		}
		stopWorker()
	}()

	log.Printf("prompt-audit sidecar listening on %s, upstream=%s", cfg.ListenAddr, cfg.UpstreamURL.Redacted())
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("prompt-audit server stopped: %v", err)
		os.Exit(1)
	}
	if rootCtx.Err() != nil {
		waitCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		select {
		case <-workerDone:
		case <-waitCtx.Done():
			stopWorker()
		}
		cancel()
	}
}

func runRetention(ctx context.Context, store *Store, retentionDays int) {
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := store.DeleteExpired(cleanupCtx, retentionDays); err != nil {
			log.Printf("prompt-audit retention cleanup failed: %v", err)
		}
	}
	cleanup()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanup()
		}
	}
}

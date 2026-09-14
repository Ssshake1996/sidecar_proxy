package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
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

	bootstrapPassword := cfg.AdminPassword
	if bootstrapPassword == "" {
		bootstrapPassword, err = generateAdminPassword()
		if err != nil {
			log.Fatalf("prepare prompt-audit admin password: %v", err)
		}
	}
	bootstrapPasswordHash, err := hashAdminPassword(bootstrapPassword)
	if err != nil {
		log.Fatalf("prepare prompt-audit admin password hash: %v", err)
	}
	go runAdminBootstrap(rootCtx, store, cfg.AdminUsername, bootstrapPassword, bootstrapPasswordHash)

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
	go runRetention(rootCtx, store, cfg.AuditRetentionDays)

	proxy, err := NewProxyServer(cfg, resolver, store, queue)
	if err != nil {
		log.Fatalf("create prompt-audit proxy: %v", err)
	}

	servers := make([]*http.Server, 0, 2)
	serverErrors := make(chan error, 2)
	newServer := func(addr string) *http.Server {
		return &http.Server{
			Addr:              addr,
			Handler:           proxy,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
	}
	if cfg.HTTPListenAddr != "" {
		server := newServer(cfg.HTTPListenAddr)
		servers = append(servers, server)
		go func() {
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErrors <- fmt.Errorf("HTTP listener %s: %w", server.Addr, err)
			}
		}()
	}
	if cfg.HTTPSListenAddr != "" {
		server := newServer(cfg.HTTPSListenAddr)
		servers = append(servers, server)
		go func() {
			if err := server.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErrors <- fmt.Errorf("HTTPS listener %s: %w", server.Addr, err)
			}
		}()
	}

	if cfg.HTTPListenAddr != "" {
		log.Printf("prompt-audit HTTP listening on %s", cfg.HTTPListenAddr)
	}
	if cfg.HTTPSListenAddr != "" {
		log.Printf("prompt-audit HTTPS listening on %s", cfg.HTTPSListenAddr)
	}
	log.Printf("prompt-audit upstream=%s", cfg.UpstreamURL.Redacted())

	var serveErr error
	select {
	case <-rootCtx.Done():
	case serveErr = <-serverErrors:
		log.Printf("prompt-audit listener stopped: %v", serveErr)
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("prompt-audit server shutdown (%s): %v", server.Addr, err)
		}
	}
	queue.Close()
	select {
	case <-workerDone:
	case <-shutdownCtx.Done():
		stopWorker()
	}
	stopWorker()
	if serveErr != nil {
		log.Printf("prompt-audit exited with listener error: %v", serveErr)
	}
}

func runAdminBootstrap(ctx context.Context, store *Store, username, password, passwordHash string) {
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		created, err := store.EnsureAdminHash(attemptCtx, username, passwordHash)
		cancel()
		if err == nil {
			if created {
				log.Printf("PROMPT_AUDIT_ADMIN_INITIAL_CREDENTIALS username=%s password=%s", username, password)
				log.Printf("prompt-audit admin account was created; store these credentials securely")
			}
			return
		}
		log.Printf("prompt-audit admin bootstrap waiting for database: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func runRetention(ctx context.Context, store *Store, retentionDays int) {
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if retentionDays > 0 {
			if err := store.DeleteExpired(cleanupCtx, retentionDays); err != nil {
				log.Printf("prompt-audit retention cleanup failed: %v", err)
			}
		}
		if err := store.DeleteExpiredAdminSessions(cleanupCtx); err != nil {
			log.Printf("prompt-audit session cleanup failed: %v", err)
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

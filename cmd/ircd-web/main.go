// Command ircd-web is a self-hosted web IRC client: a bouncer core
// (persistent IRC connections, scrollback persistence) plus an embedded
// web frontend, shipped as a single static binary.
//
// The only runtime file dependencies are the JSON config file and the
// SQLite database it names.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"ircthing/internal/api"
	"ircthing/internal/hub"
	"ircthing/internal/irc"
	"ircthing/internal/store"
	"ircthing/web"
)

func main() {
	configPath := flag.String("config", "config.json", "path to the JSON config file")
	hashPassword := flag.Bool("hash-password", false,
		"read a password from stdin, print its bcrypt hash for user.password_hash, and exit")
	flag.Parse()

	if *hashPassword {
		if err := runHashPassword(); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := run(cfg); err != nil {
		log.Fatal(err)
	}
}

func run(cfg *config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(cfg.Database, store.Options{RingSize: cfg.RingSize})
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer st.Close()

	h := hub.New(st)

	// Per network: one manager (its Run is the read loop) plus one hub
	// consumer for its events.
	var wg sync.WaitGroup
	for _, nc := range cfg.Networks {
		m, err := irc.NewManager(nc.ircConfig())
		if err != nil {
			return fmt.Errorf("network %q: %w", nc.effectiveName(), err)
		}
		wg.Add(2)
		go func() {
			defer wg.Done()
			m.Run(ctx)
		}()
		go func() {
			defer wg.Done()
			h.Run(ctx, m)
		}()
	}

	assets, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		return fmt.Errorf("embedded assets: %w", err)
	}
	handler, err := api.New(api.Config{
		Username:     cfg.User.Username,
		PasswordHash: cfg.User.PasswordHash,
		SessionTTL:   cfg.sessionTTL(),
	}, h, assets)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("listening on http://%s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		stop()
		wg.Wait()
		return err
	case <-ctx.Done():
	}

	log.Print("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	wg.Wait()
	return nil
}

func runHashPassword() error {
	fmt.Fprint(os.Stderr, "password (echoed): ")
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return err
		}
		return fmt.Errorf("no password read")
	}
	pw := strings.TrimRight(sc.Text(), "\r\n")
	if pw == "" {
		return fmt.Errorf("empty password")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	fmt.Println(string(hash))
	return nil
}

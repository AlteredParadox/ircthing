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
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"ircthing/internal/api"
	"ircthing/internal/hub"
	"ircthing/internal/netconf"
	"ircthing/internal/store"
	"ircthing/web"
)

func main() {
	configFlag := flag.String("config", "config.json", "path to the JSON config file")
	hashPassword := flag.Bool("hash-password", false,
		"read a password from stdin, print its bcrypt hash for user.password_hash, and exit")
	flag.Parse()

	if *hashPassword {
		if err := runHashPassword(); err != nil {
			log.Fatal(err)
		}
		return
	}

	configPath := resolveConfigPath(*configFlag, flagPassed("config"), os.Getenv("CREDENTIALS_DIRECTORY"))
	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := run(cfg); err != nil {
		log.Fatal(err)
	}
}

// resolveConfigPath picks the config file location. Under systemd
// DynamicUser with `LoadCredential=config.json:/etc/ircthing/config.json`,
// systemd copies the file into a private, service-only directory and
// exports its path as $CREDENTIALS_DIRECTORY (passed here as credDir); the
// binary reads it from there so the unit needs no world-readable /etc file
// and no -config flag. An explicit -config always wins.
func resolveConfigPath(flagVal string, flagSet bool, credDir string) string {
	if credDir != "" && !flagSet {
		return filepath.Join(credDir, "config.json")
	}
	return flagVal
}

// flagPassed reports whether the named flag was set explicitly on the
// command line (as opposed to left at its default).
func flagPassed(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
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

	// Networks live in the database and are managed from the web UI. The
	// config file's networks[] seeds the table on first run only; once
	// rows exist the file list is ignored.
	var wg sync.WaitGroup
	h.UseRoot(ctx, &wg)
	if err := startNetworks(ctx, st, h, cfg.Networks); err != nil {
		return fmt.Errorf("networks: %w", err)
	}

	assets, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		return fmt.Errorf("embedded assets: %w", err)
	}
	handler, err := api.New(api.Config{
		Username:        cfg.User.Username,
		PasswordHash:    cfg.User.PasswordHash,
		SessionTTL:      cfg.sessionTTL(),
		SecureCookies:   cfg.SecureCookies,
		PreviewsDefault: cfg.previewsDefault(),
	}, h, assets)
	if err != nil {
		return err
	}
	// Full server timeouts; the WebSocket endpoint hijacks its
	// connection at upgrade, after which the library manages deadlines,
	// so long-lived sessions are unaffected.
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second, // media proxy fetches cap at 15s
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
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

// startNetworks seeds the network_configs table from the config file on
// first run, then starts every stored definition. A bad stored row is
// skipped with a log line, not fatal.
// seedNetworks validates the config-file definitions and imports them on
// first run (empty database). Seeds get the same full validation as web
// edits (TLS/SASL/proxy/certificate checks) BEFORE anything is
// persisted — once a bad definition lands in the database, fixing
// config.json no longer helps because the database wins, so refusing
// startup here leaves the database untouched. Later runs ignore the file
// list and never reach this (it may reference retired cert paths).
func seedNetworks(ctx context.Context, st *store.Store, fileNetworks []netconf.Network) ([]store.NetworkConfig, error) {
	for i := range fileNetworks {
		if err := hub.ValidateNetwork(&fileNetworks[i]); err != nil {
			return nil, fmt.Errorf("config networks[%d] (%s): %w", i, fileNetworks[i].EffectiveName(), err)
		}
	}
	seedRows, err := hub.SeedRows(fileNetworks)
	if err != nil {
		return nil, err
	}
	if _, err := st.SeedNetworkConfigs(ctx, seedRows); err != nil {
		return nil, err
	}
	log.Printf("networks: imported %d definitions from the config file", len(seedRows))
	return st.NetworkConfigs(ctx)
}

func startNetworks(ctx context.Context, st *store.Store, h *hub.Hub, fileNetworks []netconf.Network) error {
	stored, err := st.NetworkConfigs(ctx)
	if err != nil {
		return err
	}
	if len(stored) == 0 && len(fileNetworks) > 0 {
		if stored, err = seedNetworks(ctx, st, fileNetworks); err != nil {
			return err
		}
	} else if len(fileNetworks) > 0 {
		log.Printf("networks: %d definitions in database; config file networks[] is ignored (manage networks in the web UI)", len(stored))
	}
	for _, row := range stored {
		nc, err := netconf.Parse([]byte(row.Config))
		if err != nil {
			log.Printf("networks: skipping %q: %v", row.Name, err)
			continue
		}
		if err := h.StartNetwork(nc); err != nil {
			log.Printf("networks: starting %q: %v", row.Name, err)
		}
	}
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

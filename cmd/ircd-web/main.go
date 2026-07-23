// ircthing — a self-hosted, always-connected web IRC client.
// Copyright (C) 2026 AlteredParadox
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
// for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

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
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"ircthing/internal/api"
	"ircthing/internal/hub"
	"ircthing/internal/netconf"
	"ircthing/internal/store"
	"ircthing/web"
)

func main() {
	// Drop the log package's own date/time prefix: the service runs under
	// systemd/journald (or another supervisor) which timestamps every
	// line, so the Go prefix is redundant — and its absence lets the
	// fail2ban filter (deploy/fail2ban/) anchor cleanly on "^login:"
	// instead of a fragile leading-timestamp pattern.
	log.SetFlags(0)

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

// version is stamped by the Makefile (-X main.version=git describe);
// empty when built without make (go install, go build).
var version string

// effectiveVersion prefers the Makefile stamp and falls back to the
// buildinfo VCS revision — the same source /source pins to — so the
// settings About line always shows SOMETHING attributable. A dirty
// unstamped build says so rather than claiming a commit.
func effectiveVersion() string {
	if version != "" {
		return version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}

// logStartupWarnings surfaces misconfiguration that is worth a log line
// but not a refusal to start. Retention warns on the EFFECTIVE value,
// not the config file: the settings table (runtime-set via the UI) is
// authoritative once seeded, so cfg.* can be stale in both directions
// after any UI change.
func logStartupWarnings(cfg *config, st *store.Store) {
	if days, maxPer := st.Retention(); days == 0 && maxPer == 0 {
		log.Print("retention: disabled (retention_days and retention_max_messages both 0) — stored history grows without bound; set a limit or place the database on a quota'd filesystem")
	}
	if w := cfg.proxyConfigWarning(); w != "" {
		log.Print("config: " + w)
	}
	if w := cfg.cookieConfigWarning(); w != "" {
		log.Print("config: " + w)
	}
	if w := api.MediaDebugURLsWarning(); w != "" {
		log.Print("env: " + w)
	}
}

func run(cfg *config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(cfg.Database, store.Options{
		RingSize:             cfg.RingSize,
		RetentionDays:        cfg.RetentionDays,
		RetentionMaxMessages: cfg.RetentionMaxMessages,
	})
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer st.Close()

	logStartupWarnings(cfg, st)

	h := hub.New(st)

	// Construct (and thereby VALIDATE) the API before any network goroutine
	// starts: api.New checks the bcrypt hash and the stored password
	// override, and failing after networks launched meant outbound IRC
	// connections for a process that was about to exit — with the deferred
	// st.Close() running (LIFO) before the signal context's stop(), i.e.
	// closing the store under goroutines still using it.
	assets, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		return fmt.Errorf("embedded assets: %w", err)
	}
	handler, err := api.New(api.Config{
		Username:            cfg.User.Username,
		PasswordHash:        cfg.User.PasswordHash,
		SessionTTL:          cfg.sessionTTL(),
		SecureCookies:       cfg.SecureCookies,
		PreviewsDefault:     cfg.previewsDefault(),
		TrustProxyForwarded: cfg.BehindProxy,
		Version:             effectiveVersion(),
	}, h, assets)
	if err != nil {
		return err
	}

	// Networks live in the database and are managed from the web UI. The
	// config file's networks[] seeds the table on first run only; once
	// rows exist the file list is ignored.
	var wg sync.WaitGroup
	h.UseRoot(ctx, &wg)
	// Web Push scheduler (provisions the VAPID key on first run). On the
	// process WaitGroup so shutdown drains an in-flight delivery before
	// the deferred st.Close().
	if err := h.StartPusher(ctx, &wg); err != nil {
		stop()
		wg.Wait()
		return fmt.Errorf("web push: %w", err)
	}
	if err := startNetworks(ctx, st, h, cfg.Networks); err != nil {
		// Some networks may already be running: cancel and WAIT for them
		// before the deferred st.Close() pulls the store out from under them.
		stop()
		wg.Wait()
		return fmt.Errorf("networks: %w", err)
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
		// Derive every request context (including the hijacked WebSocket ones,
		// which Shutdown does not track) from the signal context, so SIGTERM
		// cancels the WS read loops and their handlers unwind for the drain below.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("listening on http://%s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
	}()

	var runErr error
	select {
	case runErr = <-serveErr:
	case <-ctx.Done():
	}

	log.Print("shutting down")
	// Both signals and unexpected Serve failures take the same teardown path.
	// In particular, cancel the BaseContext before draining hijacked WebSocket
	// handlers, and never close the store while either a session or a network
	// goroutine can still use it.
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful HTTP shutdown: %v; forcing active connections closed", err)
		if closeErr := srv.Close(); closeErr != nil {
			log.Printf("force-closing HTTP server: %v", closeErr)
		}
	}
	// Drain live WebSocket handlers (their contexts were just canceled via
	// BaseContext) before the deferred Store.Close, so no session goroutine
	// races the store shutdown. The graceful wait is bounded; after it expires,
	// tracked sockets are force-closed and the final wait is definitive.
	handler.DrainSessions(3 * time.Second)
	wg.Wait()
	return runErr
}

// seedNetworks validates the config-file definitions and imports them on
// first run (empty database). Seeds get the same full validation as web
// edits (TLS/SASL/proxy/certificate checks) BEFORE anything is
// persisted — once a bad definition lands in the database, fixing
// config.json no longer helps because the database wins, so refusing
// startup here leaves the database untouched. Later runs ignore the file
// list and never reach this (it may reference retired cert paths).
func seedNetworks(ctx context.Context, st *store.Store, fileNetworks []netconf.Network) error {
	for i := range fileNetworks {
		if err := hub.ValidateNetwork(&fileNetworks[i]); err != nil {
			return fmt.Errorf("config networks[%d] (%s): %w", i, fileNetworks[i].EffectiveName(), err)
		}
	}
	seedRows, err := hub.SeedRows(fileNetworks)
	if err != nil {
		return err
	}
	if _, err := st.SeedNetworkConfigs(ctx, seedRows); err != nil {
		return err
	}
	log.Printf("networks: imported %d definitions from the config file", len(seedRows))
	return nil
}

// seedNetworksIfFirstRun runs the config-file seeding decision: an empty
// network_configs table imports the file's networks[]; a populated table
// ignores the file list (the database wins — networks are managed from the
// web UI).
func seedNetworksIfFirstRun(ctx context.Context, st *store.Store, fileNetworks []netconf.Network) error {
	count, err := st.NetworkConfigCount(ctx)
	if err != nil {
		return err
	}
	if len(fileNetworks) == 0 {
		return nil
	}
	if count == 0 {
		return seedNetworks(ctx, st, fileNetworks)
	}
	log.Printf("networks: %d definitions in database; config file networks[] is ignored (manage networks in the web UI)", count)
	return nil
}

// networkRowStarter starts stored network definitions one row at a time,
// carrying the runtime-cap counters across pages.
type networkRowStarter struct {
	h             *hub.Hub
	tracked       int // valid-name rows seen, counted toward the runtime cap
	legacySkipped int // valid-name rows beyond the cap, not started
}

// startRow handles one stored definition. A bad row is skipped with a log
// line, not fatal.
func (ns *networkRowStarter) startRow(row store.NetworkConfig) {
	if row.InvalidName {
		// Registered before (and not counted toward) the runtime cap: an
		// invalid-name row never starts a network, and get_networks
		// synthesizes a recovery entry for every such row it pages — each
		// one must map to a deletable rowid here, or the advertised
		// delete affordance dead-ends on itself.
		label := ns.h.NoteInvalidNetwork(row.PageID)
		log.Printf("networks: %s represents row %d whose stored name violates current safety bounds; delete and recreate it", label, row.PageID)
		return
	}
	if ns.tracked >= store.MaxNetworkConfigs {
		// Keep paging — invalid-name rows on later pages must still
		// register above — but start nothing further.
		ns.legacySkipped++
		return
	}
	ns.tracked++
	if row.Oversized {
		log.Printf("networks: skipping row %d: stored definition exceeds the %d-byte safety limit", row.PageID, store.MaxNetworkConfigBytes)
		ns.h.NoteStoppedNetwork(row.Name)
		return
	}
	nc, err := netconf.Parse([]byte(row.Config))
	if err != nil {
		log.Printf("networks: skipping %q: %v", row.Name, err)
		ns.h.NoteStoppedNetwork(row.Name)
		return
	}
	if err := ns.h.StartNetwork(nc); err != nil {
		log.Printf("networks: starting %q: %v", row.Name, err)
		ns.h.NoteStoppedNetwork(row.Name)
	}
}

// startNetworks seeds the network_configs table from the config file on
// first run, then starts every stored definition.
func startNetworks(ctx context.Context, st *store.Store, h *hub.Hub, fileNetworks []netconf.Network) error {
	if err := seedNetworksIfFirstRun(ctx, st, fileNetworks); err != nil {
		return err
	}
	// Iterate bounded pages. A legacy definition grown beyond the current
	// ingress cap is surfaced to the UI as a stopped network for deletion or
	// recreation, but its blob is never materialized here.
	var after int64
	starter := networkRowStarter{h: h}
	for {
		rows, more, err := st.NetworkConfigsPage(ctx, after, 16)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			if more {
				return fmt.Errorf("network config pagination made no progress")
			}
			break
		}
		for _, row := range rows {
			after = row.PageID
			starter.startRow(row)
		}
		if !more {
			break
		}
	}
	if starter.legacySkipped > 0 {
		log.Printf("networks: %d legacy definitions exceed the %d-network runtime cap and were not started; remove visible rows and restart to recover the remainder", starter.legacySkipped, store.MaxNetworkConfigs)
	}
	return nil
}

func runHashPassword() error {
	var pw string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		// Interactive: read without echo so the password never appears on
		// screen or in scrollback.
		fmt.Fprint(os.Stderr, "password: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr) // terminate the prompt line (ReadPassword ate the newline)
		if err != nil {
			return err
		}
		pw = strings.TrimRight(string(b), "\r\n")
	} else {
		// Piped/redirected input (e.g. from a provisioning script): read a
		// line; there is no terminal to suppress echo on.
		sc := bufio.NewScanner(os.Stdin)
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return err
			}
			return fmt.Errorf("no password read")
		}
		pw = strings.TrimRight(sc.Text(), "\r\n")
	}
	// Enforce the same policy as the in-UI change-password flow so the
	// bootstrap credential isn't weaker than one set later. bcrypt silently
	// ignores input past 72 bytes, so cap it rather than hash a truncated form.
	if n := len(pw); n < 8 || n > 72 {
		return fmt.Errorf("password must be 8–72 bytes (got %d)", n)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	fmt.Println(string(hash))
	return nil
}

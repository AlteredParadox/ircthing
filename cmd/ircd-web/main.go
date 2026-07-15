// Command ircd-web is a self-hosted web IRC client: a bouncer core
// (persistent IRC connections, scrollback persistence) plus an embedded
// web frontend, shipped as a single static binary.
package main

import (
	"flag"
	"io/fs"
	"log"
	"net/http"
	"time"

	"ircthing/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8067", "HTTP listen address")
	flag.Parse()

	assets, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		log.Fatalf("embedded assets: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServerFS(assets))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("listening on http://%s", *addr)
	log.Fatal(srv.ListenAndServe())
}

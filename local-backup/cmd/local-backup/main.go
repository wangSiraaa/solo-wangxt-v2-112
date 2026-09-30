// Command local-backup serves an HTTP/JSON API for local incremental backups.
//
// Storage layout under --repo:
//
//	manifest.db          SQLite catalogue (snapshots, entries, chunk index)
//	blobs/<ab>/<hex>     content-addressed, immutable chunk files
//
// FAULT_INJECT (demo only): "skip-chunk=N" or "crash-commit".
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/example/local-backup/internal/backup"
)

func parseFault(s string) backup.FaultInjection {
	var f backup.FaultInjection
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == "crash-commit":
			f.CrashCommit = true
		case strings.HasPrefix(part, "skip-chunk="):
			n, err := strconv.Atoi(strings.TrimPrefix(part, "skip-chunk="))
			if err == nil {
				f.SkipNewChunk = n
			}
		}
	}
	return f
}

func main() {
	repo := flag.String("repo", "./repo", "repository directory (manifest + blobs)")
	addr := flag.String("addr", "127.0.0.1:8765", "listen address (loopback by default)")
	flag.Parse()

	svc, err := backup.NewService(*repo, parseFault(os.Getenv("FAULT_INJECT")))
	if err != nil {
		log.Fatalf("init service: %v", err)
	}
	defer svc.Close()

	srv := &http.Server{
		Handler: backup.NewServer(svc).Handler(),
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	log.Printf("local-backup API on %s, repo %s (fault=%q)", *addr, *repo, os.Getenv("FAULT_INJECT"))
	if err := srv.Serve(ln); err != nil {
		log.Fatal(err)
	}
}

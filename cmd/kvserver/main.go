package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/steve-weiland/kvstore/internal/server"
	"github.com/steve-weiland/kvstore/internal/store"
)

func main() {
	addr := flag.String("addr", ":9090", "HTTP listen address")
	logFile := flag.String("log-file", "data/kv.log", "path to append-only log file")
	flag.Parse()

	st, err := store.Open(*logFile)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	srv := server.New(st)
	log.Printf("listening on %s (log: %s)", *addr, *logFile)
	log.Fatal(http.ListenAndServe(*addr, srv))
}

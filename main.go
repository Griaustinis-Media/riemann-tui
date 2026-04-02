package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Griaustinis-Media/riemann-tui/internal/dashboard"
)

func main() {
	addr := flag.String("addr", "localhost:5556", "Riemann host:port")
	path := flag.String("path", "/index", "WebSocket endpoint path")
	query := flag.String("query", "true", `Riemann stream query, e.g. 'service = "cpu"'`)
	useTLS := flag.Bool("tls", false, "Use wss:// (TLS)")
	insecure := flag.Bool("insecure", false, "Skip TLS certificate verification (implies --tls)")
	debugFile := flag.String("debug", "", "Write raw WebSocket frames to this file for debugging")
	flag.Parse()

	scheme := "ws"
	if *useTLS || *insecure {
		scheme = "wss"
	}

	d := dashboard.New(scheme, *addr, *path, *query, *insecure)

	if *debugFile != "" {
		f, err := os.OpenFile(*debugFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot open debug file: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		d.DebugLog = f
	}

	if err := d.Run(); err != nil {
		panic(err)
	}
}

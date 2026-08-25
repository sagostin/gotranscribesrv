// Command dgproxy is a man-in-the-middle capture proxy for the Deepgram
// /v1/listen API (realtime WebSocket and pre-recorded HTTP). It relays
// traffic verbatim between a client and the real Deepgram API while logging
// every text frame, HTTP response, and connection event to the console and a
// per-session JSONL file. Authentication is pure passthrough: whatever
// credentials the client sends are forwarded upstream unchanged.
package main

import (
	"flag"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	listen := flag.String("listen", envOr("DGPROXY_LISTEN", ":9090"), "address to listen on")
	upstream := flag.String("upstream", envOr("DGPROXY_UPSTREAM", "wss://api.deepgram.com/v1/listen"), "upstream Deepgram WebSocket URL")
	logDir := flag.String("log-dir", envOr("DGPROXY_LOG_DIR", "./logs"), "directory for session trace files")
	flag.Parse()

	upURL, err := url.Parse(*upstream)
	if err != nil {
		log.Fatalf("invalid upstream URL: %v", err)
	}
	if upURL.Scheme != "wss" && upURL.Scheme != "ws" {
		log.Fatalf("upstream must be ws:// or wss://, got %q", upURL.Scheme)
	}

	if err := os.MkdirAll(*logDir, 0o755); err != nil {
		log.Fatalf("cannot create log dir: %v", err)
	}

	// Derive the HTTP(S) base URL for pre-recorded requests from the WS URL.
	httpUpstream := *upURL
	if httpUpstream.Scheme == "wss" {
		httpUpstream.Scheme = "https"
	} else {
		httpUpstream.Scheme = "http"
	}

	p := &proxy{upstream: upURL, httpUpstream: &httpUpstream, logDir: *logDir}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/listen", p.handleListen)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "dgproxy: only /v1/listen is proxied", http.StatusNotFound)
	})

	log.Printf("dgproxy listening on %s", *listen)
	log.Printf("  websocket upstream : %s", upURL.String())
	log.Printf("  http upstream      : %s", httpUpstream.String())
	log.Printf("  session logs       : %s", *logDir)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

// proxy holds shared configuration for both proxy handlers.
type proxy struct {
	upstream     *url.URL
	httpUpstream *url.URL
	logDir       string
}

// handleListen routes WebSocket upgrades to the WS MITM and everything else
// (pre-recorded POSTs) to the HTTP passthrough.
func (p *proxy) handleListen(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		p.serveWS(w, r)
		return
	}
	p.serveHTTP(w, r)
}

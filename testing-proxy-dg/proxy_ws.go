package main

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// serveWS proxies a realtime /v1/listen WebSocket connection to Deepgram,
// logging every frame in both directions. Auth is pure passthrough: the
// client's Authorization header and subprotocols are forwarded as-is.
func (p *proxy) serveWS(w http.ResponseWriter, r *http.Request) {
	slog, err := newSessionLogger(p.logDir)
	if err != nil {
		http.Error(w, "dgproxy: cannot open trace file", http.StatusInternalServerError)
		return
	}
	defer slog.close()

	slog.log(event{Dir: dirCtoS, Kind: "ws_open", Payload: r.URL.Path + "?" + redactedQuery(r.URL.RawQuery)})

	// Build the upstream URL: configured scheme/host/path + the client's
	// query string verbatim (model, language, encoding, sample_rate, ...).
	u := *p.upstream
	u.RawQuery = r.URL.RawQuery

	// Forward auth and client identity headers only.
	header := http.Header{}
	for _, k := range []string{"Authorization", "X-Api-Key", "User-Agent"} {
		if v := r.Header.Get(k); v != "" {
			header.Set(k, v)
		}
	}
	dialer := websocket.Dialer{
		Subprotocols:     websocket.Subprotocols(r),
		HandshakeTimeout: 15 * time.Second,
	}

	// Dial upstream BEFORE upgrading the client so handshake failures can be
	// reported back as a plain HTTP error.
	upstream, resp, err := dialer.Dial(u.String(), header)
	if err != nil {
		body := ""
		if resp != nil {
			if b, rerr := io.ReadAll(io.LimitReader(resp.Body, 4096)); rerr == nil {
				body = string(b)
			}
			_ = resp.Body.Close()
		}
		slog.log(event{Dir: dirStoC, Kind: "ws_handshake_error", Err: err.Error(), Payload: body})
		status := http.StatusBadGateway
		if resp != nil {
			status = resp.StatusCode
		}
		http.Error(w, "dgproxy: upstream handshake failed: "+err.Error()+" "+body, status)
		return
	}
	slog.log(event{Dir: dirStoC, Kind: "ws_handshake", Status: resp.StatusCode})

	upgrader := websocket.Upgrader{Subprotocols: websocket.Subprotocols(r)}
	client, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.log(event{Dir: dirCtoS, Kind: "ws_upgrade_error", Err: err.Error()})
		_ = upstream.Close()
		return
	}

	done := make(chan struct{}, 2)
	go pumpFrames(upstream, client, dirCtoS, slog, done)
	go pumpFrames(client, upstream, dirStoC, slog, done)

	// One direction finished (a close was relayed). Give the other pump a
	// brief grace period to flush trailing frames (e.g. Deepgram's final
	// Results/Metadata after CloseStream), then hard-close both ends.
	<-done
	time.Sleep(300 * time.Millisecond)
	_ = client.Close()
	_ = upstream.Close()
	<-done
	slog.log(event{Dir: dirCtoS, Kind: "ws_session_end"})
}

// pumpFrames copies frames from src to dst until a close or error, logging
// text frames in full and binary frames as metadata. Close frames are
// relayed with their original code and reason.
func pumpFrames(dst, src *websocket.Conn, dir string, slog *sessionLogger, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	for {
		mt, msg, err := src.ReadMessage()
		if err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				slog.log(event{Dir: dir, Kind: "ws_close", Code: ce.Code, Reason: ce.Text})
				_ = dst.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(ce.Code, ce.Text))
			} else {
				slog.log(event{Dir: dir, Kind: "ws_error", Err: err.Error()})
			}
			return
		}
		switch mt {
		case websocket.TextMessage:
			slog.log(event{Dir: dir, Kind: "ws_text", Payload: string(msg)})
		case websocket.BinaryMessage:
			slog.log(event{Dir: dir, Kind: "ws_binary", Bytes: len(msg)})
		case websocket.PingMessage, websocket.PongMessage:
			// relayed but too noisy to log
		}
		if err := dst.WriteMessage(mt, msg); err != nil {
			slog.log(event{Dir: dir, Kind: "ws_write_error", Err: err.Error()})
			return
		}
	}
}

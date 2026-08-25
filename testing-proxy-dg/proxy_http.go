package main

import (
	"fmt"
	"io"
	"net/http"
)

// hopHeaders are connection-scoped headers that must not be forwarded.
var hopHeaders = map[string]bool{
	"Connection":          true,
	"Upgrade":             true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
}

// serveHTTP proxies a pre-recorded /v1/listen request (POST with an audio
// body) to Deepgram over HTTPS and logs the full JSON response.
func (p *proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	slog, err := newSessionLogger(p.logDir)
	if err != nil {
		http.Error(w, "dgproxy: cannot open trace file", http.StatusInternalServerError)
		return
	}
	defer slog.close()

	u := *p.httpUpstream
	u.RawQuery = r.URL.RawQuery

	slog.log(event{
		Dir:  dirCtoS,
		Kind: "http_request",
		Payload: fmt.Sprintf("%s %s?%s content-type=%s content-length=%d",
			r.Method, r.URL.Path, redactedQuery(r.URL.RawQuery),
			r.Header.Get("Content-Type"), r.ContentLength),
	})

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), r.Body)
	if err != nil {
		slog.log(event{Dir: dirCtoS, Kind: "http_error", Err: err.Error()})
		http.Error(w, "dgproxy: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for k, v := range r.Header {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		outReq.Header[k] = v
	}

	resp, err := http.DefaultClient.Do(outReq)
	if err != nil {
		slog.log(event{Dir: dirStoC, Kind: "http_error", Err: err.Error()})
		http.Error(w, "dgproxy: upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.log(event{Dir: dirStoC, Kind: "http_error", Err: "reading upstream body: " + err.Error()})
		http.Error(w, "dgproxy: reading upstream body failed", http.StatusBadGateway)
		return
	}
	slog.log(event{Dir: dirStoC, Kind: "http_response", Status: resp.StatusCode, Payload: string(body)})

	for k, v := range resp.Header {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

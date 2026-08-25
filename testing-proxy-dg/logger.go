package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Direction of a captured event: client to server (upstream) or server to
// client (downstream).
const (
	dirCtoS = "C->S" // client -> proxy -> deepgram
	dirStoC = "S->C" // deepgram -> proxy -> client
)

// event is one line in a session trace file.
type event struct {
	TS      time.Time `json:"ts"`
	Dir     string    `json:"dir"`
	Kind    string    `json:"kind"`
	Payload string    `json:"payload,omitempty"`
	Bytes   int       `json:"bytes,omitempty"`
	Status  int       `json:"status,omitempty"`
	Code    int       `json:"code,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Err     string    `json:"err,omitempty"`
}

// sessionLogger writes events to a per-session JSONL file and mirrors a
// human-readable rendering to the console. Safe for concurrent use.
type sessionLogger struct {
	mu   sync.Mutex
	id   string
	file *os.File
	enc  *json.Encoder
}

// newSessionLogger creates a timestamped trace file in dir.
func newSessionLogger(dir string) (*sessionLogger, error) {
	id := fmt.Sprintf("%s-%04x", time.Now().Format("20060102-150405"), time.Now().UnixNano()&0xffff)
	path := filepath.Join(dir, id+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	log.Printf("[session %s] trace -> %s", id, path)
	return &sessionLogger{id: id, file: f, enc: json.NewEncoder(f)}, nil
}

// log records one event.
func (s *sessionLogger) log(e event) {
	e.TS = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enc.Encode(e); err != nil {
		log.Printf("[session %s] trace write error: %v", s.id, err)
	}
	log.Printf("[session %s] %s", s.id, render(e))
}

func (s *sessionLogger) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.file.Close()
}

// render formats an event for console output.
func render(e event) string {
	ts := e.TS.Format("15:04:05.000")
	switch {
	case e.Payload != "":
		return fmt.Sprintf("%s %-4s %-13s %s", ts, e.Dir, e.Kind, e.Payload)
	case e.Bytes > 0:
		return fmt.Sprintf("%s %-4s %-13s %d bytes", ts, e.Dir, e.Kind, e.Bytes)
	case e.Kind == "http_response":
		return fmt.Sprintf("%s %-4s %-13s status=%d", ts, e.Dir, e.Kind, e.Status)
	case e.Kind == "ws_close":
		return fmt.Sprintf("%s %-4s %-13s code=%d reason=%q", ts, e.Dir, e.Kind, e.Code, e.Reason)
	case e.Err != "":
		return fmt.Sprintf("%s %-4s %-13s ERROR: %s", ts, e.Dir, e.Kind, e.Err)
	default:
		return fmt.Sprintf("%s %-4s %-13s", ts, e.Dir, e.Kind)
	}
}

// redactedHeaders clones h with sensitive values masked for logging.
func redactedHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "x-api-key" || lk == "sec-websocket-protocol" && containsToken(v) {
			out[k] = []string{"***"}
			continue
		}
		out[k] = v
	}
	return out
}

// redactedQuery returns the raw query string with token/key params masked.
func redactedQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	masked := []string{}
	for _, pair := range strings.Split(rawQuery, "&") {
		k := pair
		if i := strings.IndexByte(pair, '='); i >= 0 {
			k = pair[:i]
		}
		lk := strings.ToLower(k)
		if lk == "token" || lk == "api_key" || lk == "apikey" {
			masked = append(masked, k+"=***")
			continue
		}
		masked = append(masked, pair)
	}
	return strings.Join(masked, "&")
}

func containsToken(v []string) bool {
	for _, s := range v {
		if strings.Contains(strings.ToLower(s), "token") {
			return true
		}
	}
	return false
}

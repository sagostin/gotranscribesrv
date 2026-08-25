package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{}

// fakeUpstream mimics Deepgram: records auth/query, sends a Metadata message
// on connect, echoes text and binary frames, and closes when the client does.
type fakeUpstream struct {
	gotAuth   string
	gotQuery  string
	server    *httptest.Server
	closeCode chan int
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	fu := &fakeUpstream{closeCode: make(chan int, 1)}
	fu.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fu.gotAuth = r.Header.Get("Authorization")
		fu.gotQuery = r.URL.RawQuery
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream upgrade: %v", err)
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"Metadata","request_id":"fake-123"}`))
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				var ce *websocket.CloseError
				if ok := asCloseError(err, &ce); ok {
					fu.closeCode <- ce.Code
				}
				return
			}
			_ = conn.WriteMessage(mt, msg)
		}
	}))
	t.Cleanup(fu.server.Close)
	return fu
}

func asCloseError(err error, ce **websocket.CloseError) bool {
	if e, ok := err.(*websocket.CloseError); ok {
		*ce = e
		return true
	}
	return false
}

func wsURL(httpURL string) *url.URL {
	u, _ := url.Parse(httpURL)
	u.Scheme = "ws"
	return u
}

func startProxy(t *testing.T, upstream *url.URL, logDir string) *httptest.Server {
	t.Helper()
	httpUp := *upstream
	httpUp.Scheme = "http"
	p := &proxy{upstream: upstream, httpUpstream: &httpUp, logDir: logDir}
	srv := httptest.NewServer(http.HandlerFunc(p.handleListen))
	t.Cleanup(srv.Close)
	return srv
}

// readTrace waits for the single session file in logDir to contain want.
func readTrace(t *testing.T, logDir string, want string) []event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		files, _ := filepath.Glob(filepath.Join(logDir, "*.jsonl"))
		if len(files) == 1 {
			data, _ := os.ReadFile(files[0])
			if strings.Contains(string(data), want) {
				var events []event
				for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
					var e event
					if err := json.Unmarshal([]byte(line), &e); err == nil {
						events = append(events, e)
					}
				}
				return events
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("trace file never contained %q", want)
	return nil
}

func TestWebSocketPassthrough(t *testing.T) {
	fu := newFakeUpstream(t)
	logDir := t.TempDir()
	srv := startProxy(t, wsURL(fu.server.URL), logDir)

	header := http.Header{"Authorization": []string{"Token test-key-abc"}}
	dialURL := wsURL(srv.URL).String() + "?model=nova-3&token=should-be-redacted"
	client, _, err := websocket.DefaultDialer.Dial(dialURL, header)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}

	// Deepgram -> client: Metadata
	mt, msg, err := client.ReadMessage()
	if err != nil || mt != websocket.TextMessage || !strings.Contains(string(msg), "Metadata") {
		t.Fatalf("expected Metadata text frame, got mt=%d msg=%q err=%v", mt, msg, err)
	}

	// client -> Deepgram: text control message, echoed back
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"KeepAlive"}`)); err != nil {
		t.Fatalf("write text: %v", err)
	}
	if _, msg, err = client.ReadMessage(); err != nil || string(msg) != `{"type":"KeepAlive"}` {
		t.Fatalf("expected echo, got %q err=%v", msg, err)
	}

	// client -> Deepgram: binary audio, echoed back
	audio := []byte{0x01, 0x02, 0x03, 0x04}
	if err := client.WriteMessage(websocket.BinaryMessage, audio); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if mt, msg, err = client.ReadMessage(); err != nil || mt != websocket.BinaryMessage || len(msg) != 4 {
		t.Fatalf("expected binary echo, got mt=%d len=%d err=%v", mt, len(msg), err)
	}

	// Close and wait for the trace to capture it.
	_ = client.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	_ = client.Close()

	events := readTrace(t, logDir, `"ws_close"`)

	// Auth and query reached the fake upstream verbatim.
	if fu.gotAuth != "Token test-key-abc" {
		t.Errorf("upstream auth = %q, want passthrough", fu.gotAuth)
	}
	if fu.gotQuery != "model=nova-3&token=should-be-redacted" {
		t.Errorf("upstream query = %q, want verbatim", fu.gotQuery)
	}

	// Trace assertions.
	var sawOpen, sawHandshake, sawTextDown, sawTextUp, sawBinary, sawClose bool
	for _, e := range events {
		switch e.Kind {
		case "ws_open":
			sawOpen = true
			if strings.Contains(e.Payload, "should-be-redacted") {
				t.Errorf("token leaked into trace: %s", e.Payload)
			}
			if !strings.Contains(e.Payload, "token=***") {
				t.Errorf("expected redacted token in trace, got %s", e.Payload)
			}
		case "ws_handshake":
			sawHandshake = e.Status == http.StatusSwitchingProtocols
		case "ws_text":
			if e.Dir == dirStoC && strings.Contains(e.Payload, "Metadata") {
				sawTextDown = true
			}
			if e.Dir == dirCtoS && strings.Contains(e.Payload, "KeepAlive") {
				sawTextUp = true
			}
		case "ws_binary":
			sawBinary = e.Bytes == 4
		case "ws_close":
			sawClose = e.Code == websocket.CloseNormalClosure
		}
	}
	if !(sawOpen && sawHandshake && sawTextDown && sawTextUp && sawBinary && sawClose) {
		t.Errorf("incomplete trace: open=%v handshake=%v textDown=%v textUp=%v binary=%v close=%v",
			sawOpen, sawHandshake, sawTextDown, sawTextUp, sawBinary, sawClose)
	}
}

func TestHTTPPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token test-key-abc" {
			t.Errorf("upstream auth = %q, want passthrough", r.Header.Get("Authorization"))
		}
		if r.URL.RawQuery != "model=nova-3" {
			t.Errorf("upstream query = %q, want model=nova-3", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":{"channels":[{"alternatives":[{"transcript":"hello world"}]}]}}`))
	}))
	defer upstream.Close()

	logDir := t.TempDir()
	srv := startProxy(t, wsURL(upstream.URL), logDir)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"?model=nova-3", strings.NewReader("fake-audio"))
	req.Header.Set("Authorization", "Token test-key-abc")
	req.Header.Set("Content-Type", "audio/mpeg")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	events := readTrace(t, logDir, `"http_response"`)
	var sawReq, sawResp bool
	for _, e := range events {
		if e.Kind == "http_request" && e.Dir == dirCtoS {
			sawReq = true
		}
		if e.Kind == "http_response" && e.Status == http.StatusOK &&
			strings.Contains(e.Payload, "hello world") {
			sawResp = true
		}
	}
	if !sawReq || !sawResp {
		t.Errorf("incomplete trace: request=%v response=%v", sawReq, sawResp)
	}
}

func TestUpstreamHandshakeFailure(t *testing.T) {
	// Fake upstream that rejects the upgrade with 401, like Deepgram does
	// for a bad API key.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"err_code":"INVALID_AUTH","err_msg":"Invalid credentials."}`, http.StatusUnauthorized)
	}))
	defer upstream.Close()

	logDir := t.TempDir()
	srv := startProxy(t, wsURL(upstream.URL), logDir)

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(srv.URL).String(), nil)
	if err == nil {
		t.Fatal("expected dial error")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 propagated, got %+v", resp)
	}

	events := readTrace(t, logDir, `"ws_handshake_error"`)
	var sawErr bool
	for _, e := range events {
		if e.Kind == "ws_handshake_error" && strings.Contains(e.Payload, "INVALID_AUTH") {
			sawErr = true
		}
	}
	if !sawErr {
		t.Error("trace missing upstream handshake error body")
	}
}

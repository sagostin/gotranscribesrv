package logging

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// LogManager manages log templates and handles dispatching logs to Loki.
//
// The flow:
//  1. Handler calls BuildLog(type, template, level, fields, args...)
//  2. Handler calls log.Print() (emit locally via slog) then SendLog(log)
//  3. SendLog enqueues onto LogChannel; non-blocking; drops on full
//  4. processLogChannel (single goroutine started in NewLogManager)
//     drains the channel and ships each entry to Loki, unless disabled.
//
// All access to Templates and the consumer goroutine is single-threaded
// (one producer per event, one consumer goroutine), so no extra locking
// is required for those. The LogChannel itself is a buffered chan.
type LogManager struct {
	Templates    map[string]string
	LokiClient   *LokiClient
	LokiEnabled  bool
	LogChannel   chan *LoggingFormat
	wg           sync.WaitGroup
	printToLocal bool
	serverID     string
}

// LoggingFormat is the wire-format of a single event. It serializes to
// JSON for Loki and feeds slog.WithFields for local stdout.
type LoggingFormat struct {
	Message        string                 `json:"message,omitempty"`
	Type           string                 `json:"type,omitempty"`
	Level          slog.Level             `json:"level,omitempty"`
	RequestID      string                 `json:"request_id,omitempty"`
	ServerID       string                 `json:"server_id,omitempty"`
	AdditionalData map[string]interface{} `json:"additional_data,omitempty"`
	Timestamp      time.Time              `json:"timestamp,omitempty"`
}

// NewLogManager initializes a new LogManager. lokiEnabled gates the
// Loki push path entirely (no goroutine work, no allocations beyond
// the channel) so the cost when disabled is just the channel buffer
// and one idle goroutine that immediately skips on receive.
//
// serverID is optional; when omitted (or empty) the SERVER_ID env var
// is used. The resolved value is stamped onto every LoggingFormat so
// the JSON payload identifies the emitting node — not just the Loki
// stream label.
func NewLogManager(lokiClient *LokiClient, lokiEnabled bool, serverID ...string) *LogManager {
	sid := os.Getenv("SERVER_ID")
	if len(serverID) > 0 && serverID[0] != "" {
		sid = serverID[0]
	}
	lm := &LogManager{
		Templates:    make(map[string]string),
		LokiClient:   lokiClient,
		LokiEnabled:  lokiEnabled,
		LogChannel:   make(chan *LoggingFormat, 512),
		printToLocal: true,
		serverID:     sid,
	}
	lm.LoadTemplates()
	lm.wg.Add(1)
	go lm.processLogChannel()
	return lm
}

// AddTemplate adds a new log template to the manager. Names are stored
// upper-case so callers don't need to worry about case. Re-adding
// overwrites.
func (lm *LogManager) AddTemplate(name, template string) {
	lm.Templates[strings.ToUpper(name)] = template
}

// BuildLog creates a fully-formed LoggingFormat. logType becomes the
// "type" label on the Loki stream (great for filtering in Grafana).
// templateName must exist in Templates (case-insensitive); if it
// doesn't, the templateName itself is used as a format string. args
// are passed to fmt.Sprintf. fields is the structured metadata that
// will land in AdditionalData and become Loki label dimensions
// (e.g. "endpoint" → label, others → JSON fields).
//
// The well-known keys "request_id" and "endpoint" are promoted from
// the fields map to dedicated slots on LoggingFormat — request_id
// to the top-level JSON field (and a slog attr in Print), endpoint
// to a Loki stream label. All other fields stay in AdditionalData.
func (lm *LogManager) BuildLog(logType string, templateName string, level slog.Level, fields map[string]interface{}, args ...interface{}) *LoggingFormat {
	message := lm.formatTemplate(templateName, args...)

	var requestID string
	if fields != nil {
		if v, ok := fields["request_id"].(string); ok {
			requestID = v
			delete(fields, "request_id")
		}
	}

	return &LoggingFormat{
		Message:        message,
		Type:           strings.ToUpper(logType),
		Level:          level,
		RequestID:      requestID,
		ServerID:       lm.serverID,
		AdditionalData: fields,
		Timestamp:      time.Now(),
	}
}

// AddField mutates an already-built log entry to add a single field.
// Useful when the set of fields depends on intermediate results (e.g.
// adding audio_bytes only after reading the file).
func (lf *LoggingFormat) AddField(key string, value interface{}) {
	if lf.AdditionalData == nil {
		lf.AdditionalData = make(map[string]interface{})
	}
	lf.AdditionalData[key] = value
}

// formatTemplate resolves a template by name (case-insensitive) and
// applies fmt.Sprintf. Unknown names fall back to using the name
// itself as a format string — matches gomsggw.
func (lm *LogManager) formatTemplate(templateName string, args ...interface{}) string {
	template, exists := lm.Templates[strings.ToUpper(templateName)]
	if !exists {
		return fmt.Sprintf(templateName, args...)
	}
	return fmt.Sprintf(template, args...)
}

// SendLog emits the log to local slog AND enqueues it on the Loki
// shipping channel. The channel send is non-blocking; if the channel
// is full we log a single warning locally and drop the entry. This
// guarantees the request path is never stalled by Loki latency or
// downtime.
func (lm *LogManager) SendLog(log *LoggingFormat) {
	if lm.printToLocal {
		log.Print()
	}
	select {
	case lm.LogChannel <- log:
	default:
		// Emit the drop warning to local slog, not through SendLog,
		// to avoid recursion if the channel is permanently jammed.
		slog.Warn("log channel full, dropping log",
			"type", log.Type, "message", log.Message)
	}
}

// processLogChannel is the single consumer goroutine. It runs until
// LogChannel is closed (via CloseLogManager). When Loki is disabled
// it still drains the channel so the buffer doesn't fill, but skips
// the network call.
func (lm *LogManager) processLogChannel() {
	defer lm.wg.Done()
	for log := range lm.LogChannel {
		if !lm.LokiEnabled || lm.LokiClient == nil {
			continue
		}
		labels := map[string]string{
			"job":       os.Getenv("LOKI_JOB"),
			"server_id": lm.serverID,
			"type":      log.Type,
			"level":     log.Level.String(),
		}
		// If the event declared an "endpoint" field, promote it to a
		// label so it can be filtered cheaply in Grafana.
		if log.AdditionalData != nil {
			if ep, ok := log.AdditionalData["endpoint"].(string); ok && ep != "" {
				labels["endpoint"] = ep
			}
		}
		entry := LogEntry{
			Timestamp: log.Timestamp,
			Line:      log.String(),
		}
		if err := lm.LokiClient.PushLog(labels, entry); err != nil {
			slog.Error("failed to send log to Loki",
				"error", err, "type", log.Type)
		}
	}
}

// Print emits the log locally via slog. This is invoked automatically
// by SendLog; callers that want to emit-without-shipping should call
// it directly.
func (lf *LoggingFormat) Print() {
	attrs := []any{
		slog.String("type", lf.Type),
		slog.String("level", lf.Level.String()),
		slog.String("time", lf.Timestamp.Format(time.RFC3339)),
	}
	if lf.RequestID != "" {
		attrs = append(attrs, slog.String("request_id", lf.RequestID))
	}
	if lf.ServerID != "" {
		attrs = append(attrs, slog.String("server_id", lf.ServerID))
	}
	logEntry := slog.Default().With(attrs...)
	for key, value := range lf.AdditionalData {
		logEntry = logEntry.With(slog.Any(key, value))
	}
	switch {
	case lf.Level >= slog.LevelError:
		logEntry.Error(lf.Message)
	case lf.Level >= slog.LevelWarn:
		logEntry.Warn(lf.Message)
	case lf.Level >= slog.LevelDebug:
		logEntry.Debug(lf.Message)
	default:
		logEntry.Info(lf.Message)
	}
}

// String serializes the LoggingFormat to JSON. This is the line that
// ends up inside the Loki stream's values array.
func (lf *LoggingFormat) String() string {
	data, err := json.Marshal(lf)
	if err != nil {
		return fmt.Sprintf("Error serializing log: %v", err)
	}
	return string(data)
}

// CloseLogManager closes the log channel and waits for the consumer
// goroutine to drain. Safe to call multiple times.
type closeOnce struct {
	sync.Once
}

var closeState sync.Map // *LogManager -> *closeOnce

func (lm *LogManager) CloseLogManager() {
	once, _ := closeState.LoadOrStore(lm, &closeOnce{})
	co := once.(*closeOnce)
	co.Do(func() {
		close(lm.LogChannel)
		lm.wg.Wait()
	})
}

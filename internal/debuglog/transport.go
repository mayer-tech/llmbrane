package debuglog

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// recordingTransport is an http.RoundTripper that records raw HTTP traffic
// to the outbound debug log.
type recordingTransport struct {
	base http.RoundTripper
}

// NewTransport wraps the given RoundTripper with debug recording.
// If debug logging is disabled, the base transport is returned unchanged.
func NewTransport(base http.RoundTripper) http.RoundTripper {
	if !Enabled() {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &recordingTransport{base: base}
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()

	// Read and restore request body
	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}

	resp, err := t.base.RoundTrip(req)

	entry := &Entry{
		Direction: "outbound",
		Method:    req.Method,
		URL:       req.URL.String(),
		Headers:   RedactHeaders(req.Header),
	}

	if len(reqBody) > 0 && json.Valid(reqBody) {
		entry.Body = json.RawMessage(reqBody)
	}

	if err != nil {
		entry.DurationMs = time.Since(start).Milliseconds()
		entry.Error = err.Error()
		Write(entry)
		return resp, err
	}

	entry.Status = resp.StatusCode
	entry.ResponseHeaders = RedactHeaders(resp.Header)

	contentType := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(contentType, "text/event-stream")

	if isSSE {
		// For SSE streams, wrap the body to capture events as they flow through
		resp.Body = &sseRecorder{
			ReadCloser: resp.Body,
			entry:      entry,
			start:      start,
		}
	} else {
		// For regular responses, read the full body and restore it
		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr == nil && len(respBody) > 0 && json.Valid(respBody) {
			entry.ResponseBody = json.RawMessage(respBody)
		}
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		entry.DurationMs = time.Since(start).Milliseconds()
		Write(entry)
	}

	return resp, nil
}

// sseRecorder wraps an SSE response body to capture individual events.
type sseRecorder struct {
	io.ReadCloser
	entry   *Entry
	start   time.Time
	buf     bytes.Buffer
	flushed bool
}

func (s *sseRecorder) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if n > 0 {
		s.buf.Write(p[:n])
		s.extractEvents()
	}
	if err == io.EOF {
		s.flush()
	}
	return n, err
}

func (s *sseRecorder) Close() error {
	s.flush()
	return s.ReadCloser.Close()
}

// extractEvents parses complete SSE data lines from the buffer.
func (s *sseRecorder) extractEvents() {
	for {
		line, err := s.buf.ReadString('\n')
		if err != nil {
			// Incomplete line, put it back
			s.buf.WriteString(line)
			return
		}
		line = strings.TrimSpace(line)
		if data, ok := strings.CutPrefix(line, "data: "); ok && data != "[DONE]" {
			if json.Valid([]byte(data)) {
				s.entry.StreamEvents = append(s.entry.StreamEvents, json.RawMessage(data))
			}
		}
	}
}

// flush writes the log entry once the stream is complete.
func (s *sseRecorder) flush() {
	if s.flushed {
		return
	}
	s.flushed = true
	s.entry.DurationMs = time.Since(s.start).Milliseconds()
	Write(s.entry)
}

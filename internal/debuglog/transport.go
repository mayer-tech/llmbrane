package debuglog

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxCaptureBytes = 4 << 20

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
	entry := NewEntry(req.Context(), "outbound")
	entry.Method = req.Method
	entry.URL = req.URL.String()
	entry.Headers = RedactHeaders(req.Header)

	// The underlying transport controls when the request is read and closed.
	var requestBody *requestRecorder
	if req.Body != nil {
		requestBody = &requestRecorder{body: req.Body, contentLength: req.ContentLength}
		req = req.Clone(req.Context())
		req.Body = requestBody
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		entry.Error = err.Error()
		finishEntry(entry, start, requestBody)
		return resp, err
	}

	entry.Status = resp.StatusCode
	entry.ResponseHeaders = RedactHeaders(resp.Header)
	if resp.Body == nil {
		finishEntry(entry, start, requestBody)
		return resp, nil
	}

	mediaType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	recorder := &responseRecorder{
		body:          resp.Body,
		entry:         entry,
		start:         start,
		requestBody:   requestBody,
		contentLength: resp.ContentLength,
		stream:        strings.EqualFold(strings.TrimSpace(mediaType), "text/event-stream"),
		firstLine:     true,
	}
	if writer, ok := resp.Body.(io.Writer); ok {
		resp.Body = &responseReadWriteCloser{responseRecorder: recorder, writer: writer}
	} else {
		resp.Body = recorder
	}
	return resp, nil
}

// requestRecorder captures only bytes actually read by the underlying transport.
// Its mutex is never held while calling the underlying body, so Close can unblock Read.
type requestRecorder struct {
	body          io.ReadCloser
	contentLength int64
	mu            sync.Mutex
	buf           []byte
	read          int64
	complete      bool
	failed        bool
	overflow      bool
}

func (r *requestRecorder) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > 0 {
		r.read += int64(n)
		if !r.overflow {
			if n > maxCaptureBytes-len(r.buf) {
				r.buf = nil
				r.overflow = true
			} else {
				r.buf = append(r.buf, p[:n]...)
			}
		}
	}
	if err != nil && err != io.EOF {
		r.failed = true
	}
	if err == io.EOF || (err == nil && r.contentLength > 0 && r.read == r.contentLength) {
		r.complete = true
	}
	return n, err
}

func (r *requestRecorder) Close() error {
	return r.body.Close()
}

func (r *requestRecorder) capturedJSON() json.RawMessage {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.complete || r.failed || r.overflow || !json.Valid(r.buf) {
		return nil
	}
	return append(json.RawMessage(nil), r.buf...)
}

func finishEntry(entry *Entry, start time.Time, requestBody *requestRecorder) {
	entry.Body = requestBody.capturedJSON()
	entry.DurationMs = time.Since(start).Milliseconds()
	Write(entry)
}

// responseRecorder observes reads without reading ahead or holding a lock across
// the underlying Read or Close. Capture is limited to the first 4 MiB of wire
// data; completed stream events before that limit remain available for logging.
type responseRecorder struct {
	body          io.ReadCloser
	entry         *Entry
	start         time.Time
	requestBody   *requestRecorder
	contentLength int64
	stream        bool

	mu         sync.Mutex
	finalized  bool
	failed     bool
	overflow   bool
	captured   int
	eventBytes int
	read       int64
	buf        []byte

	// SSE parsing state. A line terminates on CR, LF, or CRLF.
	line      []byte
	data      []byte
	hasData   bool
	pendingCR bool
	firstLine bool
}

type responseReadWriteCloser struct {
	*responseRecorder
	writer io.Writer
}

func (r *responseReadWriteCloser) Write(p []byte) (int, error) {
	return r.writer.Write(p)
}

func (r *responseRecorder) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finalized {
		if n > 0 {
			r.read += int64(n)
			r.capture(p[:n])
		}
		if err != nil && err != io.EOF {
			r.failed = true
			r.entry.Error = err.Error()
		}
		if err == io.EOF {
			r.finish(true)
		}
	}
	return n, err
}

func (r *responseRecorder) Close() error {
	err := r.body.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finalized {
		r.finish(r.contentLength > 0 && r.read == r.contentLength)
	}
	return err
}

// capture processes no more than maxCaptureBytes, including SSE framing and
// ignored fields. This also bounds streams of comments or unfinished lines.
func (r *responseRecorder) capture(p []byte) {
	if r.overflow {
		return
	}
	remaining := maxCaptureBytes - r.captured
	limited := len(p) > remaining
	if limited {
		p = p[:remaining]
	}
	r.captured += len(p)
	if r.stream {
		for _, b := range p {
			r.captureSSEByte(b)
			if r.overflow {
				break
			}
		}
	} else {
		r.buf = append(r.buf, p...)
	}
	if limited {
		r.overflow = true
	}
	if r.overflow {
		r.buf = nil
		r.line = nil
		r.data = nil
	}
}

func (r *responseRecorder) captureSSEByte(b byte) {
	if r.pendingCR {
		r.pendingCR = false
		if b == '\n' {
			return
		}
	}
	switch b {
	case '\r':
		r.processSSELine()
		r.pendingCR = true
	case '\n':
		r.processSSELine()
	default:
		r.line = append(r.line, b)
	}
}

func (r *responseRecorder) processSSELine() {
	line := r.line
	if r.firstLine {
		line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
		r.firstLine = false
	}
	if len(line) == 0 {
		if r.hasData && !bytes.Equal(r.data, []byte("[DONE]")) && json.Valid(r.data) {
			// Account for both event payloads and slice bookkeeping. Wire bytes
			// alone would allow millions of tiny events to consume far more memory.
			const eventOverhead = 24
			if len(r.data)+eventOverhead <= maxCaptureBytes-r.eventBytes {
				r.entry.StreamEvents = append(r.entry.StreamEvents, json.RawMessage(r.data))
				r.eventBytes += len(r.data) + eventOverhead
			} else {
				r.overflow = true
			}
		}
		r.data = nil
		r.hasData = false
	} else if line[0] != ':' {
		field, value, found := bytes.Cut(line, []byte(":"))
		if bytes.Equal(field, []byte("data")) {
			if found && len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			if r.hasData {
				r.data = append(r.data, '\n')
			}
			r.data = append(r.data, value...)
			r.hasData = true
		}
	}
	r.line = r.line[:0]
}

// finish must be called with mu held. A response is complete only at EOF or
// after the known content length has been consumed without an error.
func (r *responseRecorder) finish(complete bool) {
	r.finalized = true
	if !r.stream && complete && !r.failed && !r.overflow && json.Valid(r.buf) {
		r.entry.ResponseBody = json.RawMessage(r.buf)
	}
	finishEntry(r.entry, r.start, r.requestBody)
	r.buf = nil
	r.line = nil
	r.data = nil
}

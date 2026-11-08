package router

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayer-tech/llmbrane/internal/debuglog"
	"github.com/mayer-tech/llmbrane/internal/inbound/anthropic"
	"github.com/mayer-tech/llmbrane/internal/inbound/openai"
)

func enableMiddlewareDebug(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := debuglog.Init(dir, time.Hour); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := debuglog.Init("", 0); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func middlewareLogEntry(t *testing.T, dir string) debuglog.Entry {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*_inbound.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("inbound log files = %v, error = %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	_, body, ok := bytes.Cut(data, []byte("\n"))
	if !ok {
		t.Fatal("missing log entry")
	}
	var entry debuglog.Entry
	if err := json.Unmarshal(body, &entry); err != nil {
		t.Fatal(err)
	}
	return entry
}

type trackingBody struct {
	reader io.Reader
	read   int64
	closed bool
}

func (b *trackingBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += int64(n)
	return n, err
}

func (b *trackingBody) Close() error {
	b.closed = true
	return nil
}

type repeatedByte byte

func (b repeatedByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

func requestAtSize(size int) io.Reader {
	const prefix = `{"ok":true}`
	const suffix = "\t \n"
	return io.MultiReader(strings.NewReader(prefix), io.LimitReader(repeatedByte(' '), int64(size-len(prefix)-len(suffix))), strings.NewReader(suffix))
}

func TestDebugRequestCaptureBoundaryAndReplay(t *testing.T) {
	for _, size := range []int{maxDebugRequestBytes - 1, maxDebugRequestBytes, maxDebugRequestBytes + 1, maxDebugRequestBytes + 8192} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			dir := enableMiddlewareDebug(t)
			body := &trackingBody{reader: requestAtSize(size)}
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.Body = body
			req.ContentLength = -1
			expected := sha256.New()
			if _, err := io.Copy(expected, requestAtSize(size)); err != nil {
				t.Fatal(err)
			}
			handler := DebugLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if body.read != int64(min(size, maxDebugRequestBytes+1)) {
					t.Errorf("debug read %d bytes before handler", body.read)
				}
				got := sha256.New()
				n, err := io.Copy(got, r.Body)
				if err != nil || n != int64(size) || !bytes.Equal(got.Sum(nil), expected.Sum(nil)) {
					t.Errorf("forwarded %d bytes, error %v, hash matches %t", n, err, bytes.Equal(got.Sum(nil), expected.Sum(nil)))
				}
				if err := r.Body.Close(); err != nil {
					t.Error(err)
				}
			}))
			handler.ServeHTTP(httptest.NewRecorder(), req)
			if !body.closed {
				t.Fatal("original body was not closed")
			}
			entry := middlewareLogEntry(t, dir)
			if size <= maxDebugRequestBytes {
				var logged map[string]bool
				if err := json.Unmarshal(entry.Body, &logged); err != nil || !logged["ok"] {
					t.Fatalf("missing or invalid request log: %s, error %v", entry.Body, err)
				}
			} else if len(entry.Body) != 0 {
				t.Fatalf("oversized request was logged: %s", entry.Body)
			}
		})
	}
}

var errBodyRead = errors.New("body read failed")

type errorThenMoreBody struct {
	step   int
	closed bool
}

func (b *errorThenMoreBody) Read(p []byte) (int, error) {
	switch b.step {
	case 0:
		b.step++
		return copy(p, `{"ok":true}`), errBodyRead
	case 1:
		b.step++
		return copy(p, "tail"), nil
	default:
		return 0, io.EOF
	}
}

func (b *errorThenMoreBody) Close() error {
	b.closed = true
	return errBodyRead
}

func TestDebugRequestPreservesReadAndCloseErrors(t *testing.T) {
	dir := enableMiddlewareDebug(t)
	body := &errorThenMoreBody{}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Body = body
	DebugLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix, err := io.ReadAll(r.Body)
		if string(prefix) != `{"ok":true}` || !errors.Is(err, errBodyRead) {
			t.Errorf("first read = %q, %v", prefix, err)
		}
		tail, err := io.ReadAll(r.Body)
		if string(tail) != "tail" || err != nil {
			t.Errorf("remaining read = %q, %v", tail, err)
		}
		if err := r.Body.Close(); !errors.Is(err, errBodyRead) {
			t.Errorf("close error = %v", err)
		}
	})).ServeHTTP(httptest.NewRecorder(), req)
	if !body.closed {
		t.Fatal("original body was not closed")
	}
	if entry := middlewareLogEntry(t, dir); len(entry.Body) != 0 {
		t.Fatalf("incomplete request was logged: %s", entry.Body)
	}
}

func TestDebugRequestPreservesErrorBeyondPrefix(t *testing.T) {
	dir := enableMiddlewareDebug(t)
	tail := &errorThenMoreBody{}
	body := &trackingBody{reader: io.MultiReader(io.LimitReader(repeatedByte('x'), maxDebugRequestBytes+1), tail)}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Body = body
	DebugLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if n != int64(maxDebugRequestBytes+1+len(`{"ok":true}`)) || !errors.Is(err, errBodyRead) {
			t.Errorf("forwarded %d bytes, error %v", n, err)
		}
		remaining, err := io.ReadAll(r.Body)
		if string(remaining) != "tail" || err != nil {
			t.Errorf("remaining read = %q, %v", remaining, err)
		}
	})).ServeHTTP(httptest.NewRecorder(), req)
	if entry := middlewareLogEntry(t, dir); len(entry.Body) != 0 {
		t.Fatalf("oversized request was logged: %s", entry.Body)
	}
}

func TestDebugDisabledDoesNotTouchRequestOrResponse(t *testing.T) {
	if err := debuglog.Init("", 0); err != nil {
		t.Fatal(err)
	}
	body := &trackingBody{reader: strings.NewReader(`{"ok":true}`)}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Body = body
	writer := httptest.NewRecorder()
	DebugLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if w != writer || r != req || r.Body != body || body.read != 0 {
			t.Error("disabled middleware changed or read the request or response writer")
		}
		data, err := io.ReadAll(r.Body)
		if string(data) != `{"ok":true}` || err != nil {
			t.Errorf("body = %q, %v", data, err)
		}
		_, _ = w.Write(data)
	})).ServeHTTP(writer, req)
	if writer.Body.String() != `{"ok":true}` {
		t.Fatalf("response = %q", writer.Body.String())
	}
}

func TestDebugPreservesInboundOversizeDetection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		path    string
		message string
	}{
		{"anthropic", anthropic.NewHandler(nil).HandleMessages, "/v1/messages", "40 MiB"},
		{"responses", openai.NewHandler(nil).HandleResponses, "/v1/responses", "size limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := enableMiddlewareDebug(t)
			req := httptest.NewRequest(http.MethodPost, tc.path, requestAtSize(maxDebugRequestBytes+1))
			req.ContentLength = -1
			w := httptest.NewRecorder()
			DebugLog(tc.handler).ServeHTTP(w, req)
			if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), tc.message) {
				t.Fatalf("status = %d, response = %s", w.Code, w.Body)
			}
			if entry := middlewareLogEntry(t, dir); len(entry.Body) != 0 {
				t.Fatalf("oversized request was logged: %s", entry.Body)
			}
		})
	}
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (w *flushRecorder) Flush() {
	w.flushes++
	w.ResponseRecorder.Flush()
}

func TestDebugStreamingResponseCaptureIsBounded(t *testing.T) {
	dir := enableMiddlewareDebug(t)
	const first = "data: {\"first\":1}\n\n"
	const partial = "data: 1"
	const tail = "2\n\ndata: [DONE]\n\n"
	filler := ":" + strings.Repeat(" ", maxDebugResponseBytes-len(first)-len(partial)-2) + "\n"
	response := first + filler + partial + tail
	writer := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	DebugLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rr, ok := w.(*responseRecorder)
		if !ok {
			t.Fatal("missing debug response recorder")
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("missing flusher")
		}
		for _, chunk := range []string{first, filler, partial, tail} {
			n, err := io.WriteString(w, chunk)
			if n != len(chunk) || err != nil {
				t.Fatalf("write = %d, %v; want %d bytes", n, err, len(chunk))
			}
			flusher.Flush()
		}
		if rr.body.Len() != maxDebugResponseBytes || !rr.truncated {
			t.Errorf("captured %d bytes, truncated = %t", rr.body.Len(), rr.truncated)
		}
	})).ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/", nil))
	if writer.Body.String() != response || writer.flushes != 4 {
		t.Fatalf("response length = %d, flushes = %d", writer.Body.Len(), writer.flushes)
	}
	entry := middlewareLogEntry(t, dir)
	if len(entry.ResponseBody) != 0 || len(entry.StreamEvents) != 1 {
		t.Fatalf("unexpected response log: body %s, events %s", entry.ResponseBody, entry.StreamEvents)
	}
	var event map[string]int
	if err := json.Unmarshal(entry.StreamEvents[0], &event); err != nil || event["first"] != 1 {
		t.Fatalf("unexpected stream event: %s, error %v", entry.StreamEvents[0], err)
	}
}

func TestDebugOmitsTruncatedValidResponsePrefix(t *testing.T) {
	dir := enableMiddlewareDebug(t)
	prefix := `{"ok":true}` + strings.Repeat(" ", maxDebugResponseBytes-len(`{"ok":true}`))
	response := prefix + "x"
	w := httptest.NewRecorder()
	DebugLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, response)
	})).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Body.String() != response {
		t.Fatal("response changed")
	}
	entry := middlewareLogEntry(t, dir)
	if len(entry.ResponseBody) != 0 || len(entry.StreamEvents) != 0 {
		t.Fatalf("truncated response was logged: %+v", entry)
	}
}

func TestResponseRecorderCaptureBoundary(t *testing.T) {
	for _, size := range []int{maxDebugResponseBytes - 1, maxDebugResponseBytes, maxDebugResponseBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			writer := httptest.NewRecorder()
			rr := &responseRecorder{ResponseWriter: writer}
			data := bytes.Repeat([]byte("x"), size)
			n, err := rr.Write(data)
			if n != size || err != nil || !bytes.Equal(writer.Body.Bytes(), data) {
				t.Fatalf("write = %d, %v, forwarded %d bytes", n, err, writer.Body.Len())
			}
			if rr.body.Len() != min(size, maxDebugResponseBytes) || rr.truncated != (size > maxDebugResponseBytes) {
				t.Fatalf("captured %d bytes, truncated = %t", rr.body.Len(), rr.truncated)
			}
		})
	}
}

type partialResponseWriter struct {
	header http.Header
}

func (w *partialResponseWriter) Header() http.Header { return w.header }
func (w *partialResponseWriter) WriteHeader(int)     {}
func (w *partialResponseWriter) Write(p []byte) (int, error) {
	return min(len(p), 3), io.ErrClosedPipe
}

func TestResponseRecorderPreservesWriteErrors(t *testing.T) {
	w := &partialResponseWriter{header: make(http.Header)}
	rr := &responseRecorder{ResponseWriter: w}
	n, err := rr.Write([]byte("abcdef"))
	if n != 3 || !errors.Is(err, io.ErrClosedPipe) || rr.body.String() != "abc" {
		t.Fatalf("write = %d, %v, captured %q", n, err, rr.body.String())
	}
}

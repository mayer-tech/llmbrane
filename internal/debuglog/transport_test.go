package debuglog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type responseResult struct {
	response *http.Response
	err      error
}

func transportRequest(t *testing.T, body io.ReadCloser, contentType string, length int64) (*http.Response, string) {
	t.Helper()
	dir := initTestLogger(t)
	ctx := WithRequest(context.Background(), "transport-test")
	path := filepath.Join(dir, NewEntry(ctx, "outbound").filePrefix+"_outbound.jsonl")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: body, ContentLength: length}, nil
	})
	result := make(chan responseResult, 1)
	go func() {
		resp, err := NewTransport(base).RoundTrip(req)
		result <- responseResult{resp, err}
	}()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		return got.response, path
	case <-time.After(time.Second):
		body.Close()
		t.Fatal("RoundTrip read the response body before returning")
		return nil, ""
	}
}

func onlyEntry(t *testing.T, path string) Entry {
	t.Helper()
	entries := readEntries(t, path)
	if len(entries) != 1 {
		t.Fatalf("expected exactly one entry, got %d", len(entries))
	}
	return entries[0]
}

func jsonMatches(got json.RawMessage, want string) bool {
	var compact bytes.Buffer
	return json.Compact(&compact, got) == nil && compact.String() == want
}

func assertNoEntry(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("entry was finalized prematurely: %v", err)
	}
}

func TestTransportResponseIsPassiveAndIncremental(t *testing.T) {
	for _, contentType := range []string{"application/json", "text/event-stream"} {
		t.Run(contentType, func(t *testing.T) {
			pr, pw := io.Pipe()
			defer pw.Close()
			resp, path := transportRequest(t, pr, contentType, -1)
			assertNoEntry(t, path)
			const chunk = "{\"value\":1}"
			written := make(chan error, 1)
			go func() {
				_, err := io.WriteString(pw, chunk)
				written <- err
			}()
			buf := make([]byte, 128)
			n, err := resp.Body.Read(buf)
			if err != nil || string(buf[:n]) != chunk {
				t.Fatalf("incremental read = %q, %v", buf[:n], err)
			}
			if err := <-written; err != nil {
				t.Fatal(err)
			}
			assertNoEntry(t, path)
			if err := pw.Close(); err != nil {
				t.Fatal(err)
			}
			if n, err := resp.Body.Read(buf); n != 0 || err != io.EOF {
				t.Fatalf("expected EOF, got %d, %v", n, err)
			}
			entry := onlyEntry(t, path)
			if contentType == "application/json" && !jsonMatches(entry.ResponseBody, chunk) {
				t.Fatalf("unexpected response body %s", entry.ResponseBody)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			onlyEntry(t, path)
		})
	}
}

type resultBody struct {
	data       []byte
	readErr    error
	closeErr   error
	closeCount int
}

func (b *resultBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, b.readErr
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	if len(b.data) == 0 {
		return n, b.readErr
	}
	return n, nil
}

func (b *resultBody) Close() error {
	b.closeCount++
	return b.closeErr
}

func TestTransportReadAndCloseErrors(t *testing.T) {
	readErr := errors.New("provider read failed")
	closeErr := errors.New("provider close failed")
	for _, contentType := range []string{"application/json", "text/event-stream"} {
		t.Run(contentType, func(t *testing.T) {
			payload := `{"ok":true}`
			if contentType == "text/event-stream" {
				payload = "data: {\"ok\":true}\n\n"
			}
			body := &resultBody{data: []byte(payload), readErr: readErr, closeErr: closeErr}
			resp, path := transportRequest(t, body, contentType, -1)
			buf := make([]byte, 128)
			n, err := resp.Body.Read(buf)
			if err != readErr || string(buf[:n]) != payload {
				t.Fatalf("read = %q, %v", buf[:n], err)
			}
			assertNoEntry(t, path)
			if err := resp.Body.Close(); err != closeErr || body.closeCount != 1 {
				t.Fatalf("close = %v, count %d", err, body.closeCount)
			}
			entry := onlyEntry(t, path)
			if entry.Error != readErr.Error() || len(entry.ResponseBody) != 0 {
				t.Fatalf("unexpected error entry: %+v", entry)
			}
			if contentType == "text/event-stream" && len(entry.StreamEvents) != 1 {
				t.Fatalf("complete event was lost: %+v", entry.StreamEvents)
			}
		})
	}
}

func TestTransportNonStreamCompletionAndLimits(t *testing.T) {
	atLimit := `"` + strings.Repeat("x", maxCaptureBytes-2) + `"`
	tests := []struct {
		name       string
		body       string
		readCount  int
		length     int64
		wantLogged bool
	}{
		{"valid", `{"a":1}`, -1, -1, true},
		{"exact limit", atLimit, -1, -1, true},
		{"over limit", atLimit + " ", -1, -1, false},
		{"invalid", `{"a":`, -1, -1, false},
		{"valid prefix closed early", `{} trailing`, 2, -1, false},
		{"known length consumed then closed", `{"a":1}`, 7, 7, true},
		{"known length closed early", `{} trailing`, 2, 11, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, path := transportRequest(t, io.NopCloser(strings.NewReader(tt.body)), "application/json", tt.length)
			var got []byte
			var err error
			if tt.readCount < 0 {
				got, err = io.ReadAll(resp.Body)
			} else {
				got = make([]byte, tt.readCount)
				_, err = io.ReadFull(resp.Body, got)
			}
			if err != nil || !strings.HasPrefix(tt.body, string(got)) {
				t.Fatalf("body changed: %v", err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			entry := onlyEntry(t, path)
			if tt.wantLogged != (len(entry.ResponseBody) > 0) {
				t.Fatalf("unexpected logged body length %d", len(entry.ResponseBody))
			}
			if tt.wantLogged && !json.Valid(entry.ResponseBody) {
				t.Fatal("logged body is not JSON")
			}
		})
	}
}

func TestTransportSSEEventFraming(t *testing.T) {
	payload := "\xef\xbb\xbf: comment\r\nevent: message\r\ndata:{\"a\":\r\ndata: 1}\r\n\r\n" +
		"data: [DONE]\n\n" +
		"data: not-json\n\n" +
		"data: {\"cr\":true}\r\r" +
		"data: {\"incomplete\":true}\n"
	resp, path := transportRequest(t, io.NopCloser(strings.NewReader(payload)), "Text/Event-Stream; charset=utf-8", -1)
	var received bytes.Buffer
	buf := make([]byte, 1)
	for {
		n, err := resp.Body.Read(buf)
		received.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if received.String() != payload {
		t.Fatal("stream was modified")
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	entry := onlyEntry(t, path)
	if len(entry.StreamEvents) != 2 || !jsonMatches(entry.StreamEvents[0], `{"a":1}`) || !jsonMatches(entry.StreamEvents[1], `{"cr":true}`) {
		t.Fatalf("unexpected events: %q", entry.StreamEvents)
	}
}

func TestTransportSSEBoundedAndPartial(t *testing.T) {
	for _, filler := range []string{
		strings.Repeat("x", maxCaptureBytes),
		strings.Repeat(": ignored\n", maxCaptureBytes/10+1),
		strings.Repeat("data: 0\n\n", maxCaptureBytes/9+1),
	} {
		payload := "data: {\"first\":1}\n\n" + filler + "\n\ndata: {\"late\":2}\n\n"
		resp, path := transportRequest(t, io.NopCloser(strings.NewReader(payload)), "text/event-stream", -1)
		got, err := io.ReadAll(resp.Body)
		if err != nil || string(got) != payload {
			t.Fatalf("stream changed: %v", err)
		}
		resp.Body.Close()
		entry := onlyEntry(t, path)
		if len(entry.StreamEvents) == 0 || !jsonMatches(entry.StreamEvents[0], `{"first":1}`) {
			t.Fatal("first complete event missing")
		}
		for _, event := range entry.StreamEvents {
			if jsonMatches(event, `{"late":2}`) {
				t.Fatal("captured an event beyond the limit")
			}
		}
	}
}

func TestTransportCloseUnblocksReadAndPreservesIdleTimeout(t *testing.T) {
	for _, idle := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "idle"}[idle], func(t *testing.T) {
			pr, pw := io.Pipe()
			defer pw.Close()
			resp, path := transportRequest(t, pr, "application/json", -1)
			reader := resp.Body
			if idle {
				reader = canonical.NewIdleTimeoutReader(reader, 25*time.Millisecond)
			}
			result := make(chan error, 1)
			go func() {
				_, err := reader.Read(make([]byte, 1))
				result <- err
			}()
			if !idle {
				if err := reader.Close(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("expected read error after close")
				}
			case <-time.After(time.Second):
				pw.Close()
				t.Fatal("close did not unblock read")
			}
			if idle {
				reader.Close()
			}
			onlyEntry(t, path)
		})
	}
}

func TestTransportConcurrentCloseAndEOFLogsOnce(t *testing.T) {
	pr, pw := io.Pipe()
	resp, path := transportRequest(t, pr, "text/event-stream", -1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(io.Discard, resp.Body)
	}()
	go func() {
		defer wg.Done()
		pw.Close()
		resp.Body.Close()
	}()
	wg.Wait()
	onlyEntry(t, path)
}

func TestTransportRequestBodyIsPassiveAndErrorsPropagate(t *testing.T) {
	dir := initTestLogger(t)
	ctx := WithRequest(context.Background(), "request-test")
	path := filepath.Join(dir, NewEntry(ctx, "outbound").filePrefix+"_outbound.jsonl")
	readErr := errors.New("request read failed")
	body := &resultBody{data: []byte(`{"valid":true}`), readErr: readErr}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com", body)
	if err != nil {
		t.Fatal(err)
	}
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(req.Body)
		if err != readErr || string(data) != `{"valid":true}` {
			t.Fatalf("request read = %q, %v", data, err)
		}
		req.Body.Close()
		return nil, err
	})
	_, err = NewTransport(base).RoundTrip(req)
	if err != readErr || body.closeCount != 1 {
		t.Fatalf("request error = %v, close count = %d", err, body.closeCount)
	}
	entry := onlyEntry(t, path)
	if len(entry.Body) != 0 || entry.Error != readErr.Error() {
		t.Fatalf("unexpected request entry: %+v", entry)
	}
}

func TestTransportRequestJSONCaptured(t *testing.T) {
	dir := initTestLogger(t)
	ctx := WithRequest(context.Background(), "request-json")
	path := filepath.Join(dir, NewEntry(ctx, "outbound").filePrefix+"_outbound.jsonl")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com", strings.NewReader(`{"input":1}`))
	if err != nil {
		t.Fatal(err)
	}
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		_, err := io.Copy(io.Discard, req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output":2}`))}, nil
	})
	resp, err := NewTransport(base).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	entry := onlyEntry(t, path)
	if !jsonMatches(entry.Body, `{"input":1}`) || !jsonMatches(entry.ResponseBody, `{"output":2}`) {
		t.Fatalf("unexpected request/response entry: %+v", entry)
	}
}

type readWriteBody struct {
	reader io.Reader
	bytes.Buffer
}

func (b *readWriteBody) Read(p []byte) (int, error) { return b.reader.Read(p) }
func (b *readWriteBody) Close() error               { return nil }

func TestTransportPreservesResponseWriter(t *testing.T) {
	body := &readWriteBody{reader: strings.NewReader("response")}
	resp, _ := transportRequest(t, body, "application/octet-stream", -1)
	writer, ok := resp.Body.(io.Writer)
	if !ok {
		t.Fatal("response body lost its writer")
	}
	if _, err := writer.Write([]byte("request")); err != nil || body.Buffer.String() != "request" {
		t.Fatalf("write not forwarded: %v", err)
	}
	resp.Body.Close()
}

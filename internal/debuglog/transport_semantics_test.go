package debuglog

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTransportCancellationUnblocksResponseRead(t *testing.T) {
	dir := initTestLogger(t)
	ctx, cancel := context.WithCancel(WithRequest(context.Background(), "cancel-response"))
	defer cancel()
	path := filepath.Join(dir, NewEntry(ctx, "outbound").filePrefix+"_outbound.jsonl")
	pr, pw := io.Pipe()
	defer pw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		go func() {
			<-req.Context().Done()
			pw.CloseWithError(req.Context().Err())
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	})
	resp, err := NewTransport(base).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 1))
		read <- err
	}()
	cancel()
	select {
	case err := <-read:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read error = %v", err)
		}
	case <-time.After(time.Second):
		pw.Close()
		t.Fatal("cancellation did not unblock read")
	}
	resp.Body.Close()
	if entry := onlyEntry(t, path); entry.Error != context.Canceled.Error() {
		t.Fatalf("unexpected logged error %q", entry.Error)
	}
}

func TestTransportDoesNotReadRequestAhead(t *testing.T) {
	dir := initTestLogger(t)
	ctx := WithRequest(context.Background(), "passive-request")
	path := filepath.Join(dir, NewEntry(ctx, "outbound").filePrefix+"_outbound.jsonl")
	pr, pw := io.Pipe()
	defer pw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com", pr)
	if err != nil {
		t.Fatal(err)
	}
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.Body.Close()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
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
		io.Copy(io.Discard, got.response.Body)
		got.response.Body.Close()
		if entry := onlyEntry(t, path); len(entry.Body) != 0 {
			t.Fatal("unread request was logged")
		}
	case <-time.After(time.Second):
		pw.Close()
		t.Fatal("transport read request ahead")
	}
}

func TestTransportRequestCaptureLimit(t *testing.T) {
	for _, size := range []int{maxCaptureBytes, maxCaptureBytes + 1} {
		t.Run(map[bool]string{true: "at limit", false: "over limit"}[size == maxCaptureBytes], func(t *testing.T) {
			dir := initTestLogger(t)
			ctx := WithRequest(context.Background(), "request-limit")
			path := filepath.Join(dir, NewEntry(ctx, "outbound").filePrefix+"_outbound.jsonl")
			payload := `"` + strings.Repeat("x", size-2) + `"`
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com", strings.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				count, err := io.Copy(io.Discard, req.Body)
				if err != nil || count != int64(size) {
					t.Fatalf("request size %d, error %v", count, err)
				}
				req.Body.Close()
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})
			resp, err := NewTransport(base).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			entry := onlyEntry(t, path)
			if (len(entry.Body) > 0) != (size == maxCaptureBytes) {
				t.Fatalf("unexpected request capture length %d", len(entry.Body))
			}
		})
	}
}

func TestTransportEOFWithDataAndEarlyStreamClose(t *testing.T) {
	body := &resultBody{data: []byte(`{"ok":true}`), readErr: io.EOF}
	resp, path := transportRequest(t, body, "application/json", -1)
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != io.EOF || string(buf[:n]) != `{"ok":true}` {
		t.Fatalf("read = %q, %v", buf[:n], err)
	}
	if entry := onlyEntry(t, path); !jsonMatches(entry.ResponseBody, `{"ok":true}`) {
		t.Fatalf("EOF data missing: %s", entry.ResponseBody)
	}
	resp.Body.Close()

	stream := "data: {\"ok\":true}\n\ndata: {\"partial\":true}\n"
	resp, path = transportRequest(t, io.NopCloser(strings.NewReader(stream)), "text/event-stream", -1)
	if _, err := io.ReadFull(resp.Body, make([]byte, len(stream))); err != nil {
		t.Fatal(err)
	}
	assertNoEntry(t, path)
	resp.Body.Close()
	entry := onlyEntry(t, path)
	if len(entry.StreamEvents) != 1 || !jsonMatches(entry.StreamEvents[0], `{"ok":true}`) {
		t.Fatalf("unexpected early-close events: %s", entry.StreamEvents)
	}
}

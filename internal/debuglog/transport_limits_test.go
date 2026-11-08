package debuglog

import (
	"io"
	"strings"
	"testing"
)

func TestTransportSSECaptureBoundary(t *testing.T) {
	const event = "data: {\"at_limit\":true}\n\n"
	for _, extra := range []int{0, 1} {
		t.Run(map[bool]string{true: "event ends at limit", false: "event crosses limit"}[extra == 0], func(t *testing.T) {
			// Padding is an SSE comment, so the event remains independently framed.
			padding := ":" + strings.Repeat("x", maxCaptureBytes-len(event)-2+extra) + "\n"
			payload := padding + event
			resp, path := transportRequest(t, io.NopCloser(strings.NewReader(payload)), "text/event-stream", -1)
			got, err := io.ReadAll(resp.Body)
			if err != nil || string(got) != payload {
				t.Fatalf("read changed: %v", err)
			}
			resp.Body.Close()
			entry := onlyEntry(t, path)
			if extra == 0 {
				if len(entry.StreamEvents) != 1 || !jsonMatches(entry.StreamEvents[0], `{"at_limit":true}`) {
					t.Fatalf("event at limit missing: %s", entry.StreamEvents)
				}
			} else if len(entry.StreamEvents) != 0 {
				t.Fatalf("partial event was logged: %s", entry.StreamEvents)
			}
		})
	}
}

func TestTransportSSERetainsOnlyBoundedEventMetadata(t *testing.T) {
	payload := strings.Repeat("data: 0\n\n", maxCaptureBytes/len("data: 0\n\n")+100)
	resp, path := transportRequest(t, io.NopCloser(strings.NewReader(payload)), "text/event-stream", -1)
	recorder, ok := resp.Body.(*responseRecorder)
	if !ok {
		t.Fatal("unexpected response recorder")
	}
	count, err := io.Copy(io.Discard, resp.Body)
	if err != nil || count != int64(len(payload)) {
		t.Fatalf("read %d bytes: %v", count, err)
	}
	if !recorder.overflow || recorder.captured > maxCaptureBytes || recorder.eventBytes > maxCaptureBytes {
		t.Fatalf("capture exceeds limits: wire=%d events=%d overflow=%v", recorder.captured, recorder.eventBytes, recorder.overflow)
	}
	resp.Body.Close()
	entry := onlyEntry(t, path)
	if len(entry.StreamEvents) == 0 || len(entry.StreamEvents)*25 > maxCaptureBytes {
		t.Fatalf("unexpected event count %d", len(entry.StreamEvents))
	}
}

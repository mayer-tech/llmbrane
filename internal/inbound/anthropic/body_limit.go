package anthropic

import (
	"bytes"
	"errors"
	"io"
	"net/http"
)

// maxMessagesBodyBytes allows room for encoded images and reasoning envelopes.
const maxMessagesBodyBytes = 40 << 20

// limitMessagesBody checks the entire body before decoding, including any data
// after the first JSON value. Call it after the method check and before creating
// the JSON decoder in HandleMessages. The existing decoder and route-specific
// trailing-data validation can then operate unchanged.
func limitMessagesBody(w http.ResponseWriter, r *http.Request) bool {
	body := http.MaxBytesReader(w, r.Body, maxMessagesBodyBytes)
	data, err := io.ReadAll(body)
	var sizeErr *http.MaxBytesError
	if errors.As(err, &sizeErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "Request body exceeds the 40 MiB limit", "request_too_large")
		return false
	}

	if err != nil {
		writeError(w, http.StatusBadRequest, "Failed to read request body", "invalid_request_error")
		return false
	}
	r.Body = messagesReplayBody{Reader: bytes.NewReader(data), Closer: body}
	return true
}

type messagesReplayBody struct {
	io.Reader
	io.Closer
}

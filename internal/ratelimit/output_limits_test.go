package ratelimit

import (
	"context"
	"testing"
)

func TestExplicitZeroOutputReservation(t *testing.T) {
	zero := 0
	req := requestWithMessages("hello")
	req.Params.MaxOutputTokens = &zero
	reservation, estimate, err := NewManager().Acquire(context.Background(), "scope", "", Settings{OutputReservationTokens: 8192}, req)
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Cancel()
	if estimate.Output != 0 || estimate.OutputSource != "max_output_tokens" {
		t.Fatalf("estimate = %+v", estimate)
	}
}

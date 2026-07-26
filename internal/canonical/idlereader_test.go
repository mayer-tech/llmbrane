package canonical_test

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

func TestIdleTimeoutReader_Success(t *testing.T) {
	pr, pw := io.Pipe()

	tr := canonical.NewIdleTimeoutReader(pr, 100*time.Millisecond)
	defer tr.Close()

	go func() {
		for i := 0; i < 3; i++ {
			time.Sleep(30 * time.Millisecond)
			pw.Write([]byte("data\n"))
		}
		pw.Close()
	}()

	buf := make([]byte, 1024)
	total := 0
	for {
		n, err := tr.Read(buf)
		total += n
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("unexpected read error: %v", err)
		}
	}

	if expected := len("data\n") * 3; total != expected {
		t.Errorf("expected %d bytes read, got %d", expected, total)
	}
}

func TestIdleTimeoutReader_Timeout(t *testing.T) {
	pr, _ := io.Pipe()

	tr := canonical.NewIdleTimeoutReader(pr, 50*time.Millisecond)
	defer tr.Close()

	buf := make([]byte, 1024)
	start := time.Now()
	_, err := tr.Read(buf)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error due to idle timeout, got nil")
	}

	if elapsed < 40*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Errorf("expected timeout around 50ms, elapsed: %v", elapsed)
	}
}

func TestIdleTimeoutReader_Disabled(t *testing.T) {
	r := io.NopCloser(strings.NewReader("hello"))
	tr := canonical.NewIdleTimeoutReader(r, 0)

	buf := make([]byte, 10)
	n, err := tr.Read(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Errorf("expected 'hello', got %q", string(buf[:n]))
	}
}

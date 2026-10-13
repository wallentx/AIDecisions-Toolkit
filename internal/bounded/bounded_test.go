package bounded

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadBoundaryAndConsumption(t *testing.T) {
	for _, size := range []int{8, 9, 100} {
		r := &countingReader{Reader: strings.NewReader(strings.Repeat("x", size))}
		data, err := Read(r, 8, "sample")
		if size == 8 {
			if err != nil || string(data) != strings.Repeat("x", size) {
				t.Fatalf("exact boundary rejected: %q, %v", data, err)
			}
		} else if !errors.Is(err, ErrLimit) || data != nil || r.count != 9 {
			t.Fatalf("oversized input not bounded: size=%d, read=%d, data=%q, err=%v", size, r.count, data, err)
		}
	}
}

func TestFileBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample")
	for _, size := range []int{8, 9} {
		if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0600); err != nil {
			t.Fatal(err)
		}
		data, err := File(path, 8, "sample")
		if size == 8 && (err != nil || len(data) != size) {
			t.Fatalf("exact boundary: data=%q, err=%v", data, err)
		}
		if size == 9 && (!errors.Is(err, ErrLimit) || data != nil) {
			t.Fatalf("oversized file accepted: %q, %v", data, err)
		}
	}
}

func TestBufferBoundary(t *testing.T) {
	buffer := NewBuffer(8, "process output")
	if n, err := buffer.Write([]byte("1234")); n != 4 || err != nil {
		t.Fatalf("first chunk: n=%d, err=%v", n, err)
	}
	if n, err := buffer.Write([]byte("5678")); n != 4 || err != nil || buffer.Err() != nil {
		t.Fatalf("exact boundary: n=%d, err=%v", n, err)
	}
	if n, err := buffer.Write([]byte("9")); n != 0 || !errors.Is(err, ErrLimit) || !errors.Is(buffer.Err(), ErrLimit) {
		t.Fatalf("overflow: n=%d, err=%v", n, err)
	}
	if n, err := buffer.Write([]byte("ignored")); n != 0 || !errors.Is(err, ErrLimit) || string(buffer.Bytes()) != "12345678" {
		t.Fatalf("write after overflow: n=%d, bytes=%q, err=%v", n, buffer.Bytes(), err)
	}
	buffer = NewBuffer(8, "process output")
	if n, err := buffer.Write([]byte("123456789")); n != 8 || !errors.Is(err, ErrLimit) || len(buffer.Bytes()) != 8 {
		t.Fatalf("single oversized chunk: n=%d, err=%v", n, err)
	}
}

type countingReader struct {
	io.Reader
	count int
}

func (reader *countingReader) Read(data []byte) (int, error) {
	n, err := reader.Reader.Read(data)
	reader.count += n
	return n, err
}

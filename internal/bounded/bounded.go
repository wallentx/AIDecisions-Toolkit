// Package bounded reads external input without unbounded allocations.
package bounded

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// ErrLimit marks input that exceeds its allowed byte count.
var ErrLimit = errors.New("input exceeds limit")

func limitError(label string, limit int64) error {
	return fmt.Errorf("%s exceeds %d bytes: %w", label, limit, ErrLimit)
}

// Read consumes at most limit+1 bytes and returns no data when the input is too
// large. The extra byte distinguishes a complete input at the boundary.
func Read(reader io.Reader, limit int64, label string) ([]byte, error) {
	if limit < 0 || limit == math.MaxInt64 {
		return nil, errors.New("invalid input byte limit")
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	if int64(len(data)) > limit {
		return nil, limitError(label, limit)
	}
	return data, nil
}

// File opens a file and applies the same bound as Read, including special files
// whose reported size does not reflect the amount of readable data.
func File(path string, limit int64, label string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return Read(file, limit, label)
}

// Buffer is an output writer for subprocesses. It stores at most limit bytes
// and stops accepting data after the first oversized write.
type Buffer struct {
	data  bytes.Buffer
	limit int64
	label string
	err   error
}

func NewBuffer(limit int64, label string) *Buffer {
	buffer := &Buffer{limit: limit, label: label}
	if limit < 0 {
		buffer.err = errors.New("invalid input byte limit")
	}
	return buffer
}

func (buffer *Buffer) Write(data []byte) (int, error) {
	if buffer.err != nil {
		return 0, buffer.err
	}
	remaining := buffer.limit - int64(buffer.data.Len())
	if int64(len(data)) > remaining {
		n, _ := buffer.data.Write(data[:int(remaining)])
		buffer.err = limitError(buffer.label, buffer.limit)
		return n, buffer.err
	}
	return buffer.data.Write(data)
}

func (buffer *Buffer) Bytes() []byte { return buffer.data.Bytes() }
func (buffer *Buffer) Err() error    { return buffer.err }

// Package proto defines backpack's two newline-delimited JSON wire
// protocols and small framing helpers:
//
//   - the daemon protocol (backpack engine -> backpackd, root), daemon.go;
//   - the engine protocol (CLI/TUI -> engine), engine.go.
//
// Framing is identical for both: each message is one JSON object on one
// line terminated by '\n'. Use WriteLine to send and LineReader to receive.
package proto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrLineTooLong is returned by LineReader when a line exceeds its limit.
// The stream is no longer in sync afterwards; the caller must close it.
var ErrLineTooLong = errors.New("proto: line exceeds size limit")

// LineReader reads '\n'-terminated lines with a hard size limit. It never
// buffers more than limit+bufio's internal buffer bytes, so a hostile peer
// cannot make it allocate unbounded memory.
type LineReader struct {
	r     *bufio.Reader
	limit int
}

// NewLineReader returns a reader that rejects lines longer than limit bytes
// (excluding the trailing '\n').
func NewLineReader(r io.Reader, limit int) *LineReader {
	return &LineReader{r: bufio.NewReaderSize(r, 4096), limit: limit}
}

// ReadLine returns the next line without its trailing "\n" (and without a
// trailing "\r"). It returns io.EOF at a clean end of stream. A final line
// without a newline is returned as a line; the next call returns io.EOF.
// It returns ErrLineTooLong as soon as the limit is exceeded, without
// reading the rest of the line.
func (l *LineReader) ReadLine() ([]byte, error) {
	var line []byte
	for {
		chunk, err := l.r.ReadSlice('\n')
		line = append(line, chunk...)
		content := len(line)
		if content > 0 && line[content-1] == '\n' {
			content--
		}
		if content > l.limit {
			return nil, ErrLineTooLong
		}
		switch {
		case err == nil:
			line = bytes.TrimSuffix(line[:content], []byte("\r"))
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(line) == 0 {
				return nil, io.EOF
			}
			return bytes.TrimSuffix(line, []byte("\r")), nil
		default:
			return nil, err
		}
	}
}

// Decode reads the next line and unmarshals it into v. With strict set,
// unknown fields and trailing data are rejected (the daemon decodes
// strictly; the engine and clients decode leniently for forward
// compatibility). JSON errors are wrapped in *DecodeError; framing errors
// (io.EOF, ErrLineTooLong, I/O errors) are returned as is.
func (l *LineReader) Decode(v any, strict bool) error {
	line, err := l.ReadLine()
	if err != nil {
		return err
	}
	return Unmarshal(line, v, strict)
}

// DecodeError reports a malformed message.
type DecodeError struct{ Err error }

func (e *DecodeError) Error() string { return "proto: malformed message: " + e.Err.Error() }
func (e *DecodeError) Unwrap() error { return e.Err }

// Unmarshal decodes one message. See LineReader.Decode for strict.
func Unmarshal(line []byte, v any, strict bool) error {
	dec := json.NewDecoder(bytes.NewReader(line))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		return &DecodeError{Err: err}
	}
	if strict && dec.More() {
		return &DecodeError{Err: errors.New("trailing data after message")}
	}
	return nil
}

// WriteLine marshals v and writes it followed by '\n' in a single Write.
func WriteLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("proto: marshal: %w", err)
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// Package sse reads the data payloads of a Server-Sent Events stream.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// MaxEventSize bounds the payload of a single event. Larger events are
// skipped rather than buffered without limit.
const MaxEventSize = 32 << 20

// Read calls fn with the data payload of every event in r until EOF.
// Multiple data lines in one event are joined with "\n" as the spec requires.
func Read(r io.Reader, fn func(data []byte)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var data []byte
	hasData, oversized := false, false
	dispatch := func() {
		if hasData && !oversized {
			fn(data)
		}
		data, hasData, oversized = data[:0], false, false
	}
	for {
		line, tooLong, err := readLine(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				dispatch()
				return nil
			}
			return err
		}
		if tooLong {
			oversized = true
			continue
		}
		if len(line) == 0 {
			dispatch()
			continue
		}
		value, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		value = bytes.TrimPrefix(value, []byte(" "))
		if hasData {
			data = append(data, '\n')
		}
		if len(data)+len(value) > MaxEventSize {
			oversized = true
		} else {
			data = append(data, value...)
		}
		hasData = true
	}
}

// readLine returns one line without its terminator. Lines longer than
// MaxEventSize are consumed and reported as tooLong.
func readLine(br *bufio.Reader) (line []byte, tooLong bool, err error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(buf)+len(chunk) > MaxEventSize {
			tooLong = true
		} else {
			buf = append(buf, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && (!errors.Is(err, io.EOF) || len(buf) == 0) {
			return nil, false, err
		}
		buf = bytes.TrimSuffix(buf, []byte("\n"))
		buf = bytes.TrimSuffix(buf, []byte("\r"))
		return buf, tooLong, nil
	}
}

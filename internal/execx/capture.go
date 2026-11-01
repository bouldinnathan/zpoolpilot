// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package execx

import (
	"bytes"
	"fmt"
)

const (
	// DefaultMaxStdoutBytes accommodates large `zfs get all` inventories while
	// preventing a subprocess from growing the collector or apply process
	// without bound.
	DefaultMaxStdoutBytes int64 = 64 << 20
	DefaultMaxStderrBytes int64 = 1 << 20
)

// BoundedBuffer retains at most Limit bytes while continuing to consume the
// writer's complete stream. Returning successful writes after the limit is
// reached lets the child terminate normally; callers must check LimitError and
// treat overflow as a failed command.
type BoundedBuffer struct {
	buffer   bytes.Buffer
	limit    int64
	overflow bool
}

func NewBoundedBuffer(limit int64) *BoundedBuffer {
	if limit < 0 {
		limit = 0
	}
	return &BoundedBuffer{limit: limit}
}

func (buffer *BoundedBuffer) Write(value []byte) (int, error) {
	length := len(value)
	remaining := buffer.limit - int64(buffer.buffer.Len())
	if remaining > 0 {
		keep := int64(length)
		if keep > remaining {
			keep = remaining
		}
		_, _ = buffer.buffer.Write(value[:int(keep)])
	}
	if int64(length) > remaining {
		buffer.overflow = true
	}
	return length, nil
}

func (buffer *BoundedBuffer) Bytes() []byte { return buffer.buffer.Bytes() }

func (buffer *BoundedBuffer) String() string { return buffer.buffer.String() }

// LimitError reports whether the stream exceeded the retained byte limit.
func (buffer *BoundedBuffer) LimitError(stream string) error {
	if buffer == nil || !buffer.overflow {
		return nil
	}
	return fmt.Errorf("%w: %s exceeded %d bytes", ErrOutputLimit, stream, buffer.limit)
}

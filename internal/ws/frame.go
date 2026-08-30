// Package ws is a minimal server-side WebSocket implementation (RFC 6455)
// written on top of the standard library.
//
// The assessment forbids libraries that implement the core feature, and a
// chat app's core *is* the real-time transport, so the handshake and the wire
// framing are implemented here rather than imported. Scope is deliberately
// narrow: text frames, ping/pong, close. Binary frames, fragmentation and
// extensions (permessage-deflate) are rejected — the chat protocol never needs
// them, and every feature left out is one less thing to get wrong.
package ws

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Opcode values from RFC 6455 §5.2.
type Opcode byte

const (
	OpContinuation Opcode = 0x0
	OpText         Opcode = 0x1
	OpBinary       Opcode = 0x2
	OpClose        Opcode = 0x8
	OpPing         Opcode = 0x9
	OpPong         Opcode = 0xA
)

// Close status codes (RFC 6455 §7.4.1) that this implementation uses.
const (
	CloseNormal          = 1000
	CloseGoingAway       = 1001
	CloseProtocolError   = 1002
	CloseUnsupportedData = 1003
	CloseMessageTooBig   = 1009
)

var (
	ErrFrameTooLarge = errors.New("ws: frame exceeds maximum size")
	ErrProtocol      = errors.New("ws: protocol error")
)

// frame is one decoded wire frame.
type frame struct {
	fin     bool
	opcode  Opcode
	payload []byte
}

// readFrame decodes a single frame. maxPayload bounds the allocation: the
// length field is attacker-controlled (a client can claim a 2^63-byte
// payload), so we must refuse before allocating, not after.
func readFrame(r io.Reader, maxPayload int64) (frame, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return frame{}, err
	}
	f := frame{
		fin:    hdr[0]&0x80 != 0,
		opcode: Opcode(hdr[0] & 0x0F),
	}
	// RSV1-3 must be zero unless an extension negotiated them; we negotiate none.
	if hdr[0]&0x70 != 0 {
		return frame{}, fmt.Errorf("%w: reserved bits set", ErrProtocol)
	}
	masked := hdr[1]&0x80 != 0
	length := int64(hdr[1] & 0x7F)

	// 7-bit length, or 16-bit / 64-bit extended lengths (§5.2).
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return frame{}, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return frame{}, err
		}
		u := binary.BigEndian.Uint64(ext[:])
		if u>>63 != 0 { // most significant bit must be 0
			return frame{}, fmt.Errorf("%w: invalid 64-bit length", ErrProtocol)
		}
		length = int64(u)
	}
	if length > maxPayload {
		return frame{}, ErrFrameTooLarge
	}

	// Client→server frames MUST be masked (§5.1); an unmasked one means a
	// broken or hostile client, and the RFC says to fail the connection.
	if !masked {
		return frame{}, fmt.Errorf("%w: client frame not masked", ErrProtocol)
	}
	var key [4]byte
	if _, err := io.ReadFull(r, key[:]); err != nil {
		return frame{}, err
	}

	f.payload = make([]byte, length)
	if _, err := io.ReadFull(r, f.payload); err != nil {
		return frame{}, err
	}
	mask(f.payload, key)
	return f, nil
}

// mask XORs payload with the 4-byte key. Masking is an involution, so the same
// function both masks and unmasks.
func mask(payload []byte, key [4]byte) {
	for i := range payload {
		payload[i] ^= key[i%4]
	}
}

// writeFrame encodes one unfragmented, unmasked frame. Server→client frames
// are never masked (§5.1), which is why no key parameter exists here.
func writeFrame(w io.Writer, op Opcode, payload []byte) error {
	buf := make([]byte, 0, 10+len(payload))
	buf = append(buf, 0x80|byte(op)) // FIN set: we never fragment
	n := len(payload)
	switch {
	case n < 126:
		buf = append(buf, byte(n))
	case n <= 0xFFFF:
		buf = append(buf, 126, byte(n>>8), byte(n))
	default:
		buf = append(buf, 127)
		buf = binary.BigEndian.AppendUint64(buf, uint64(n))
	}
	buf = append(buf, payload...)
	// One Write call for header+payload so a concurrent writer (guarded by the
	// caller's mutex, but still) can never interleave inside a frame.
	_, err := w.Write(buf)
	return err
}

package proto

import (
	"encoding/binary"
	"fmt"
)

// EncodeFrame builds one complete binary frame: the 12-byte
// little-endian header followed by payload bytes produced by
// encodePayload (nil produces an empty payload). The total must fit
// MaxFrameSize; oversized payloads fail with ErrFrameTooLarge and are
// never truncated.
func EncodeFrame(header Header, encodePayload func(*Encoder) error) ([]byte, error) {
	var payload []byte
	if encodePayload != nil {
		e := NewEncoder()
		if err := encodePayload(e); err != nil {
			return nil, err
		}
		p, err := e.Bytes()
		if err != nil {
			return nil, err
		}
		payload = p
	}
	if HeaderSize+len(payload) > MaxFrameSize {
		return nil, fmt.Errorf(
			"proto: frame size=%d max=%d: %w",
			HeaderSize+len(payload),
			MaxFrameSize,
			ErrFrameTooLarge,
		)
	}
	out := make([]byte, HeaderSize+len(payload))
	binary.LittleEndian.PutUint16(out[0:2], header.Opcode)
	binary.LittleEndian.PutUint16(out[2:4], header.MsgVersion)
	binary.LittleEndian.PutUint32(out[4:8], header.Seq)
	binary.LittleEndian.PutUint32(out[8:12], header.Tick)
	copy(out[HeaderSize:], payload)
	return out, nil
}

// DecodeFrame parses one complete binary frame supplied as a single
// WebSocket message. Semantics:
//
//  1. len(frame) > MaxFrameSize is rejected with ErrFrameTooLarge
//     BEFORE any header interpretation.
//  2. len(frame) < HeaderSize is rejected with ErrTruncated.
//  3. The 12-byte header is parsed little-endian (any opcode value).
//  4. A Decoder bounded to exactly the payload bytes is returned.
//  5. The payload need NOT be fully consumed; trailing unknown bytes
//     are valid (msg_version forward compatibility).
func DecodeFrame(frame []byte) (Header, *Decoder, error) {
	if len(frame) > MaxFrameSize {
		return Header{}, nil, fmt.Errorf(
			"proto: frame size=%d max=%d: %w",
			len(frame),
			MaxFrameSize,
			ErrFrameTooLarge,
		)
	}
	if len(frame) < HeaderSize {
		return Header{}, nil, fmt.Errorf(
			"proto: frame size=%d smaller than header=%d: %w",
			len(frame),
			HeaderSize,
			ErrTruncated,
		)
	}
	header := Header{
		Opcode:     binary.LittleEndian.Uint16(frame[0:2]),
		MsgVersion: binary.LittleEndian.Uint16(frame[2:4]),
		Seq:        binary.LittleEndian.Uint32(frame[4:8]),
		Tick:       binary.LittleEndian.Uint32(frame[8:12]),
	}
	return header, NewDecoder(frame[HeaderSize:]), nil
}

package proto

import "errors"

// Wire limits frozen by the spec (§6 framing).
const (
	// HeaderSize is the exact byte size of the frame envelope:
	// u16 opcode + u16 msg_version + u32 seq + u32 tick.
	HeaderSize = 12

	// MaxFrameSize is the maximum total frame size in bytes,
	// INCLUDING the 12-byte envelope. len == MaxFrameSize is
	// structurally allowed; len == MaxFrameSize+1 is rejected.
	MaxFrameSize = 64 * 1024 // 65536, not 64000

	// MaxStringBytes is the general protocol string cap in UTF-8 bytes.
	MaxStringBytes = 1024

	// MaxChatBytes is the deliberate chat-text exception.
	MaxChatBytes = 512

	// MaxAccessTokenBytes is the deliberate accessToken exception
	// (Keycloak JWTs with roles/claims routinely exceed 1 KiB).
	MaxAccessTokenBytes = 8 * 1024

	// MaxArrayCount is the maximum element count of any protocol array.
	MaxArrayCount = 1024

	// MaxAngle is the maximum valid 12-bit wire angle (inclusive).
	MaxAngle = 4095

	// maxU16 bounds every u16 length/count prefix on the wire.
	maxU16 = 0xFFFF
)

// Stable sentinel errors suitable for errors.Is.
// Machine-readable strings are short snake_case tokens.
var (
	// ErrFrameTooLarge reports "frame_too_large": total frame bytes
	// (header + payload) exceed MaxFrameSize.
	ErrFrameTooLarge = errors.New("frame_too_large")

	// ErrTruncated reports "truncated": the buffer ends before a
	// complete value could be read.
	ErrTruncated = errors.New("truncated")

	// ErrStringTooLong reports "string_too_long": a string's UTF-8 byte
	// length exceeds the applicable semantic cap or the u16 prefix range.
	ErrStringTooLong = errors.New("string_too_long")

	// ErrInvalidUTF8 reports "invalid_utf8": string bytes are not valid UTF-8.
	ErrInvalidUTF8 = errors.New("invalid_utf8")

	// ErrArrayTooLong reports "array_too_long": an array count exceeds
	// the applicable cap (never above MaxArrayCount).
	ErrArrayTooLong = errors.New("array_too_long")

	// ErrAngleOutOfRange reports "angle_out_of_range": a wire angle
	// falls outside 0..MaxAngle.
	ErrAngleOutOfRange = errors.New("angle_out_of_range")

	// ErrEntryTooLarge reports "entry_too_large": a single entry's bytes
	// exceed the u16 entryLen range.
	ErrEntryTooLarge = errors.New("entry_too_large")

	// ErrStableIDOutOfRange reports "stable_id_out_of_range": a stable
	// catalog/listing namespace ID exceeds the u16 range even though its
	// union carrier on the wire is wider (e.g. Use kind=0 IDs in the
	// fixed-u32 payload must still fit u16).
	ErrStableIDOutOfRange = errors.New("stable_id_out_of_range")
)

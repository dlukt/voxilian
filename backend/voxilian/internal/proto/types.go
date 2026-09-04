package proto

// Header is the 12-byte frame envelope preceding every payload.
// Opcode is intentionally unvalidated here: unknown opcodes are a
// higher-layer concern and the core decoder parses any uint16 value.
// Seq and Tick preserve the full uint32 range; wrap comparison
// (RFC 1982 style) belongs to a later task, not this codec.
type Header struct {
	Opcode     uint16
	MsgVersion uint16
	Seq        uint32
	Tick       uint32
}

// Cell is a sim grid coordinate: i32 cx + i32 cz on the wire.
type Cell struct {
	X int32
	Z int32
}

// Position is a millimeter fixed-point world coordinate:
// i32 x + i32 y + i32 z on the wire. Units are millimeters.
// Floats never appear in the wire codec.
type Position struct {
	X int32
	Y int32
	Z int32
}

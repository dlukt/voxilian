package proto

import "fmt"

// Connect / re-auth / error messages: opcodes 100, 101, 200, 201, 202.
//
// Directions: Hello and Reauth are C→S; Welcome, ReauthOK and Error are
// S→C. The codec performs no direction enforcement and no semantic
// validation (no JWT checks, no client-version policy, no named
// error-code mapping); it only reads/writes wire fields.

// Hello is opcode 100 (C→S): authenticates the account session.
// AccessToken uses MaxAccessTokenBytes (8 KiB), not MaxStringBytes.
type Hello struct {
	ClientVersion uint32
	ProtoVersion  uint16
	AccessToken   string
}

// Encode writes u32 clientVersion + u16 protoVersion + string(accessToken, 8192).
func (m Hello) Encode(e *Encoder) {
	e.U32(m.ClientVersion)
	e.U16(m.ProtoVersion)
	e.String(m.AccessToken, MaxAccessTokenBytes)
}

// DecodeHello reads a Hello payload, tolerating trailing unknown bytes
// (msg_version forward compatibility).
func DecodeHello(d *Decoder) (Hello, error) {
	var m Hello
	var err error
	if m.ClientVersion, err = d.U32(); err != nil {
		return Hello{}, fmt.Errorf("proto: hello clientVersion: %w", err)
	}
	if m.ProtoVersion, err = d.U16(); err != nil {
		return Hello{}, fmt.Errorf("proto: hello protoVersion: %w", err)
	}
	if m.AccessToken, err = d.String(MaxAccessTokenBytes); err != nil {
		return Hello{}, fmt.Errorf("proto: hello accessToken: %w", err)
	}
	d.SkipRemaining()
	return m, nil
}

// Reauth is opcode 101 (C→S): refreshes authorization over a live WS.
// Same 8 KiB token cap as Hello.
type Reauth struct {
	AccessToken string
}

// Encode writes string(accessToken, 8192).
func (m Reauth) Encode(e *Encoder) {
	e.String(m.AccessToken, MaxAccessTokenBytes)
}

// DecodeReauth reads a Reauth payload, tolerating trailing unknown bytes.
func DecodeReauth(d *Decoder) (Reauth, error) {
	token, err := d.String(MaxAccessTokenBytes)
	if err != nil {
		return Reauth{}, fmt.Errorf("proto: reauth accessToken: %w", err)
	}
	d.SkipRemaining()
	return Reauth{AccessToken: token}, nil
}

// WorldInfo is the nested world descriptor inside Welcome.
// Mode carries a raw wire value; no semantic world-mode enum is frozen here.
type WorldInfo struct {
	Mode    uint8
	Seed    uint64
	Version uint32
}

// Welcome is opcode 200 (S→C): server handshake answering Hello.
type Welcome struct {
	ServerTimeMs uint64
	Chunk        uint8
	AOIRadius    uint16
	TickRates    []uint16
	World        WorldInfo
}

// Encode writes serverTimeMs u64, chunk u8, aoiRadius u16, then the
// tick-rate table as u8 count + u16 values (deliberately NOT the normal
// u16 array prefix), then world{mode u8, seed u64, version u32}.
// More than 255 tick rates fail with ErrArrayTooLong; the count is
// never truncated or wrapped.
func (m Welcome) Encode(e *Encoder) {
	e.U64(m.ServerTimeMs)
	e.U8(m.Chunk)
	e.U16(m.AOIRadius)
	writeU8Count(e, len(m.TickRates))
	for _, r := range m.TickRates {
		e.U16(r)
	}
	e.U8(m.World.Mode)
	e.U64(m.World.Seed)
	e.U32(m.World.Version)
}

// writeU8Count writes a u8 array count prefix for layouts (like
// Welcome.tickRates) that use a single-byte count on the wire.
// Counts above 255 record ErrArrayTooLong via the Encoder's sticky
// error, making later writes no-ops instead of wrapping to 0.
func writeU8Count(e *Encoder, n int) {
	if n < 0 || n > 0xFF {
		e.fail(fmt.Errorf("proto: array count=%d max=255: %w", n, ErrArrayTooLong))
		return
	}
	e.U8(uint8(n))
}

// DecodeWelcome reads a Welcome payload, tolerating trailing unknown bytes.
// A u8 count is intrinsically bounded, so no cap check is needed before
// allocating the tick-rate slice.
func DecodeWelcome(d *Decoder) (Welcome, error) {
	var m Welcome
	var err error
	if m.ServerTimeMs, err = d.U64(); err != nil {
		return Welcome{}, fmt.Errorf("proto: welcome serverTimeMs: %w", err)
	}
	if m.Chunk, err = d.U8(); err != nil {
		return Welcome{}, fmt.Errorf("proto: welcome chunk: %w", err)
	}
	if m.AOIRadius, err = d.U16(); err != nil {
		return Welcome{}, fmt.Errorf("proto: welcome aoiRadius: %w", err)
	}
	var count uint8
	if count, err = d.U8(); err != nil {
		return Welcome{}, fmt.Errorf("proto: welcome tickRates: %w", err)
	}
	m.TickRates = make([]uint16, 0, count)
	for i := 0; i < int(count); i++ {
		var r uint16
		if r, err = d.U16(); err != nil {
			return Welcome{}, fmt.Errorf("proto: welcome tickRates[%d]: %w", i, err)
		}
		m.TickRates = append(m.TickRates, r)
	}
	if m.World.Mode, err = d.U8(); err != nil {
		return Welcome{}, fmt.Errorf("proto: welcome world mode: %w", err)
	}
	if m.World.Seed, err = d.U64(); err != nil {
		return Welcome{}, fmt.Errorf("proto: welcome world seed: %w", err)
	}
	if m.World.Version, err = d.U32(); err != nil {
		return Welcome{}, fmt.Errorf("proto: welcome world version: %w", err)
	}
	d.SkipRemaining()
	return m, nil
}

// ReauthOK is opcode 201 (S→C): empty acknowledgement of Reauth.
type ReauthOK struct{}

// Encode writes no current payload bytes.
func (m ReauthOK) Encode(_ *Encoder) {}

// DecodeReauthOK accepts any payload, ignoring trailing bytes so a
// future additive version still decodes.
func DecodeReauthOK(d *Decoder) (ReauthOK, error) {
	d.SkipRemaining()
	return ReauthOK{}, nil
}

// ErrorMessage is opcode 202 (S→C): {code u16, message string}.
// Code is a raw wire value: the spec assigns no numeric error codes
// here, so no named constants or string mappings are defined.
// The message uses MaxStringBytes (this is not chat).
type ErrorMessage struct {
	Code    uint16
	Message string
}

// Encode writes u16 code + string(message, 1024).
func (m ErrorMessage) Encode(e *Encoder) {
	e.U16(m.Code)
	e.String(m.Message, MaxStringBytes)
}

// DecodeErrorMessage reads an Error payload, tolerating trailing unknown bytes.
func DecodeErrorMessage(d *Decoder) (ErrorMessage, error) {
	var m ErrorMessage
	var err error
	if m.Code, err = d.U16(); err != nil {
		return ErrorMessage{}, fmt.Errorf("proto: error code: %w", err)
	}
	if m.Message, err = d.String(MaxStringBytes); err != nil {
		return ErrorMessage{}, fmt.Errorf("proto: error message: %w", err)
	}
	d.SkipRemaining()
	return m, nil
}

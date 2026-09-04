# testdata/protocol — shared Go/Godot golden fixtures (M2 owns the format)

`_harness_example.hex` is the M0-T4 harness sentinel ONLY: bytes
`00 01 7f ff`. It MUST NOT be treated as a Voxilian protocol golden
vector. M2 fixture discovery excludes underscore-prefixed names; real
per-opcode vectors are added by M2-T4. Do not create fake opcode
fixtures here.

## What these fixtures are

Every real (non-underscore-prefixed) `.hex` file is **one complete
WebSocket binary frame**: the 12-byte envelope
`[u16 opcode][u16 msg_version][u32 seq][u32 tick]` followed by the
message payload. Never payload-only. Decode with `DecodeFrame` first,
then the concrete per-opcode payload decoder.

Filename format: `<opcode>_<snake_name>.hex` (e.g.
`205_entity_move.hex`). Underscore-prefixed names are harness-only and
never count as protocol vectors.

## Byte syntax

- Lowercase hexadecimal octets (`aa`, not `AA`).
- Ordinary ASCII whitespace between octets (space, tab, LF, CR —
  the Go reader strips exactly these and nothing else).
- 16 bytes per line where practical, final newline.
- No `0x` prefixes, commas, colons, comments, sidebars, or metadata
  inside fixture files. Human-readable descriptions live in this
  README and in `internal/proto/golden_test.go`, never in the bytes.

## Wire conventions (both sides must match)

- All multibyte integers are **little-endian**.
- `string` = `u16` **UTF-8 byte length** (not rune count) + bytes.
- Positions are signed `i32` millimeters (fixed-point, no floats).
- Angles are `u16` in range `0..4095`.
- Repeated structures are `[u16 entryLen][entry bytes]`; `entryLen`
  counts only the bytes after its own prefix.
- Opcode 218 `chunk_fragment` carries an explicit `byteLen u16`
  before its byte blob, so trailing `msg_version` fields stay
  distinguishable from chunk data.

## Canonical fixture header (test data only)

Every real fixture uses, changing only `Opcode`:

```text
MsgVersion = 0
Seq        = 0x01020304
Tick       = 0x05060708
```

So an opcode `OP` always starts with
`[OP little-endian u16] 00 00 04 03 02 01 08 07 06 05`
(e.g. opcode 100: `64 00 00 00 04 03 02 01 08 07 06 05`).
`MsgVersion = 0` is a golden-test value only and does NOT declare a
deployment message version; no `ProtocolVersion`/`CurrentMsgVersion`
constant exists. `Seq`/`Tick` are fixed arbitrary test values.

## Stability rule

Fixtures are the stable cross-language contract. They must NOT be
regenerated silently when codecs change: any intentional wire change
requires explicit fixture review and update in the same diff. Go tests
fail on mismatch and never rewrite fixtures (`UPDATE_GOLDEN`-style
behavior is forbidden). Godot reads exactly these same files:

```text
read text -> remove ASCII whitespace ->
decode each 2 hex chars to one byte ->
parse the same little-endian frame
```

## Catalog

| filename | opcode | payload summary |
|---|---|---|
| 100_hello.hex | 100 | clientVersion u32, protoVersion u16, accessToken 8 KiB (`tok-é`) |
| 101_reauth.hex | 101 | accessToken (`reauth-token`) |
| 102_move.hex | 102 | inputSeq u32, heldDirs u8, runFlag u8, yaw angle |
| 103_attack.hex | 103 | target u32 |
| 104_cast.hex | 104 | spell u16, target u32 |
| 105_use.hex | 105 | kind u8 (=0 skill), id fixed u32 |
| 106_get.hex | 106 | entity u32, item u32 |
| 107_drop.hex | 107 | item u32 |
| 108_put.hex | 108 | item u32, container u32 |
| 109_give.hex | 109 | target u32, item u32, qty u16 |
| 110_offer.hex | 110 | target u32, 2× item u32 |
| 111_counter.hex | 111 | 2× item u32 |
| 112_accept.hex | 112 | empty (header only) |
| 113_cancel.hex | 113 | empty (header only) |
| 114_buy.hex | 114 | vendor u32, listing u16, qty u16 |
| 115_rest.hex | 115 | state u8 (raw 0xff) |
| 116_eat.hex | 116 | item u32 |
| 117_say.hex | 117 | channel u8, chat text (`hé`, 3 UTF-8 bytes) |
| 118_say_group.hex | 118 | chat text (`grüp`) |
| 119_safety_toggle.hex | 119 | empty (header only) |
| 120_respawn_ack.hex | 120 | empty (header only) |
| 121_character_list_request.hex | 121 | empty (header only) |
| 122_character_create.hex | 122 | slot, name (`Zoë`), gender, face, stats[6], spells, skills |
| 123_character_delete.hex | 123 | slot u8 |
| 124_enter_world.hex | 124 | slot u8 |
| 125_ack.hex | 125 | ackSeq u32 |
| 126_leave_world.hex | 126 | empty (header only) |
| 200_welcome.hex | 200 | serverTimeMs u64, chunk u8, aoiRadius u16, tickRates u8-count, world |
| 201_reauth_ok.hex | 201 | empty (header only) |
| 202_error.hex | 202 | code u16, message (`oops-é`) |
| 203_cell_snapshot.hex | 203 | cell + 1 entry-framed entityEntry |
| 204_entity_create.hex | 204 | single direct entityEntry (no entryLen) |
| 205_entity_move.hex | 205 | entity, pos, angle, speed, lastProcessedInputSeq |
| 206_entity_remove.hex | 206 | entity u32 |
| 207_stat.hex | 207 | entity u32 + signed stat entry (value −123) |
| 208_stat_group.hex | 208 | entity u32 + 2 entry-framed stat entries |
| 209_said.hex | 209 | from u32, channel u8, chat text (`hé`) |
| 210_effect.hex | 210 | id u16, target u32, pos |
| 211_inventory_delta.hex | 211 | 2 entry-framed inventory entries (direct + contained) |
| 212_offer_update.hex | 212 | with u32, state u8 (raw 0xff), 2 framed offer items |
| 213_trade_result.hex | 213 | ok u8 (raw 0xff) |
| 214_death.hex | 214 | victim u32 |
| 215_respawn.hex | 215 | pos |
| 216_character_list_result.hex | 216 | 2 entry-framed character summaries (`Ada`, `Zoë`) |
| 217_character_op.hex | 217 | op/ok raw u8 values |
| 218_chunk_fragment.hex | 218 | cell, chunkIdx, fragIdx, fragCount, byteLen=3 + 3 bytes |
| 219_world_ready.hex | 219 | empty (header only) |
| 220_shop_list.hex | 220 | vendor u32 + 2 framed shop listings |

# testdata/protocol — shared Go/Godot golden fixtures (M2 owns the format)

`_harness_example.hex` is the M0-T4 harness sentinel ONLY: bytes
`00 01 7f ff`. It MUST NOT be treated as a Voxilian protocol golden
vector. M2 fixture discovery excludes underscore-prefixed names; real
per-opcode vectors are added by M2-T4. Do not create fake opcode
fixtures here.

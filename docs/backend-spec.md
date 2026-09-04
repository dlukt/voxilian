# Voxilian Backend SPEC (v0.3.12 — documentation only, no implementation)

> Status: DRAFT for discussion. Normative keywords: MUST / SHOULD / MAY.
> Companion doc: `docs/meridian59.md` (game-mechanics reference, source of all
> numbers/formulas below). Nothing here is implemented yet.

## 0. Locked decisions

| # | Decision | Value |
|---|---|---|
| D1 | World model | **Seamless + embedded interiors**: one continuous overworld; dungeons / Underworld / guildhalls are embedded regions/interiors in the same coordinate space (separate coordinate bands or y-layered volumes), NOT boxed instances |
| D2 | Transport | **WebSocket + hand-packed binary codec, M59-style** (Godot 4.7.2 client). Opcode envelope, versioned messages, zero codec deps both sides. JSON reserved for admin/debug surfaces only |
| D3 | Topology | **Single world process** for MVP. Sessions, presence, and rate limits are in-process memory. No Redis, no external bus. Spatial workers are a later scale-out, designed for but not built |
| D4 | Persistence | **Snapshot + write-through**: PG is authoritative; sim keeps hot state in memory, snapshots every N sec + write-through on critical events (death, trade, logout, guild/faction change) |
| D5 | DB stack | **PostgreSQL 18 + pgx + sqlc + goose**. No other datastore. Current pinned versions in §2 |
| D6 | Auth | **External Keycloak IdP (OIDC)**. Godot client uses Authorization Code + PKCE via system browser; backend validates access JWTs via cached JWKS and never sees passwords (DECISION §13.4) |
| D7 | Persistence rule | **PG materialized state = recovery source of truth; ledger = audit trail, NOT event sourcing.** Critical ops update state + ledger atomically in one PG txn; aggregate-root `revision`s (character, item, bank) guard child rows so async snapshots can never overwrite newer critical writes (§8.1) |

## 1. Goals / non-goals

Goals (MVP backend):
- G1: Authoritative sim for a seamless overworld: movement, AOI streaming,
  combat (M59 hit/damage formulas), vitals/regen, mobs + AI, loot, trade/bank,
  death + corpse + Underworld-region respawn.
- G2: M59 progression port: 6 stats (creation point-buy), spells/skills 1–99%
  use-based improvement, HP-as-level advancement, learn-points gate.
- G3: Sessions/presence/rate-limits in process memory; durable ledger in
  PG 18; clean `sim` vs `store` separation so spatial sharding can be added
  without rewriting game logic.
- G4: Operable locally via compose; observable (logs/metrics/health); admin
  console parity with M59 basics (create account/character, save, kick).

Non-goals (MVP): guilds wars/rent, factions/territory, Justicar justice,
Assassin Game, Jala hinder matrix, housing/rentables, weather/hour cycle,
multi-process sharding, client prediction reconciliation beyond baseline
server-reconcile, browser/web export specifics.

## 2. Tech stack (pinned, Sept 2026)

| Component | Version | Notes |
|---|---|---|
| Go | 1.27 (repo already `go 1.27.0`) | |
| PostgreSQL | 18.x (local 18.6) | Source of truth |
| pgx | `github.com/jackc/pgx/v5` **v5.10.0** | Driver + pool; supports PG14+, Go 1.25+ |
| sqlc | **v1.31.1** | Generate typed queries from SQL; config `sqlc.yaml`, `pgx/v5` emit |
| goose | `github.com/pressly/goose/v3` **v3.27.x** | `migrations/*.sql`, embed + `voxilian migrate` cobra subcommand |
| CLI | cobra (already `v1.10.2` in `go.mod`) | `voxilian serve / migrate / admin / seed` |
| WS | `github.com/coder/websocket` **v1.8.15** | Pick over archived gorilla; single maintained dep |
| OIDC/JWT | `github.com/lestrrat-go/jwx/v4` **v4.4.0** + `github.com/jwx-go/jwkfetch/v4` **v4.0.4** | Access-token validation against Keycloak JWKS (D6); Go 1.27 stdlib `encoding/json/v2`, no `GOEXPERIMENT=jsonv2` |
| Logging | `log/slog` (stdlib) | JSON in prod, text in dev |
| Metrics | Prometheus client (current) + `/metrics` | Counters/histograms per §11 |
| Testing | stdlib + `testcontainers-go` (current) for PG integration | Unit sim with fake clock/store |

## 3. Process architecture (single world process)

```text
              +------------------- voxilian (one process) -------------------+
Godot clients <--WS--> gateway (auth, sessions, AOI fanout) <-> sim (cells, |
                         combat, AI, regen, advancement) <-> store (sqlc→PG) |
                                                                            |
  sessions/presence/rate-limits: in-memory registries (gateway-owned)        |
              +--------------------------------------------------------------+
PG 18 (durable) · memory (ephemeral; rebuilt on restart) · /metrics /healthz
```

- MUST: exactly one goroutine-group owns each map cell's mutable sim state
  (single-writer per cell; cross-cell actions via sim message queue, never
  shared maps). This is what later allows moving a cell to another worker.
- MUST: gateway never applies game rules; it authenticates, rate-limits,
  routes intents to sim, and fans out AOI-filtered deltas.
- MUST: store is the only package importing pgx/sqlc output. Sim depends on a
  `Store` interface (fake-able in tests).
- Proposed repo layout (docs only):
  `cmd/ (serve,migrate,admin,seed)` · `internal/{config,gateway,sim,store,session,auth,admin,observe,world}` ·
  `migrations/` · `queries/` · `sqlc.yaml` · `compose.yaml`.

## 4. Seamless world model

- D1 consequence: **no room teleport graph as the primary space**. M59's 265
  rooms become *authored content bands* placed in world coordinates
  (starter town footprint, dungeons as walled complexes / y-separated volumes,
  Underworld as a distant region). M59 `ROOM_*` flags become **volume flags**
  (safe-death, no-PK, no-combat, sanctuary ×2/×3 heal, hometown, kill-zone).
- Coordinates: world units in meters, float64 server-side; **voxel chunk
  16³**, origin and constants defined by `internal/world` and shared with
  the Godot client (single source: `world.toml` or generated constants —
  DECISION §13.1). 1 M59 square ≈ 1 m for range ports (validate in
  playtest). World content comes from the configured `WorldSource`
  (DECISION §13.5): `classic` authored data or `procedural` deterministic
  generation.
- Spatial index: uniform grid cells of **32 m** (2×2 chunks) → each cell has
  an owner worker (MVP: all local). Neighbor lookup for melee/AoE/AOI.
- AOI/interest: per-session subscription = cells within **96 m** (3-cell
  radius; tunable to 128 m if min-spec testing shows headroom — DECISION
  §13.1) + interior volume override. Gateway sends: full snapshot on
  cell-enter, then deltas at tick; entity despawn on cell-exit. MUST
  throttle: movement states ≤ 10 Hz per entity in AOI; combat/vital events
  immediate. Procedural mode: client generates terrain from seed, AOI
  bandwidth is entities-only; classic mode streams chunk data (hence the
  conservative 96 m default).
- Embedded interiors: dungeons/Underworld/guildhall volumes flagged
  `INTERIOR`; entry by walking through portal volumes. Wording precision:
  a portal to a distant coordinate band IS a server-side coordinate
  remap under the hood — the "seamless" guarantee is no loading screen,
  no session break, uninterrupted input (optionally masked by a short
  fade), followed by a cell-snapshot swap. Only death-respawn and admin
  summons use explicit remaps outside portals (logged).
- Portals/doors/locks (M59 shatter-lock, guildhall keys) are volume
  edge-rules evaluated by sim.

## 5. Sim loop & tick rates (ported from M59, adapted)

| Loop | Rate | Notes |
|---|---|---|
| Movement integration + cell handoff | 20 Hz | Server-authoritative; `102 move {inputSeq, heldDirs, runFlag, yaw}` ≤ 10 Hz, never positions; header tick = sampling tick; server echoes `lastProcessedInputSeq` in `205`; client rewinds+reapplies on error > 0.5 m (§6.3); walk ~3.5 m/s, run ~7 m/s w/ M59 vigor gate (DECISION §13.2) |
| Mob AI (chase/move/attack decision) | chase 6–7 Hz equivalent, move/attack 1 Hz (M59: 150 ms / 1500 ms / 1000 ms) | Per-mob staggered timers, not global lockstep |
| Melee/ranged attacks | max 1 swing/s per attacker (M59 `IsOkayAttackTime`) | Hit `(Off*55)/Def` 10–95%; server-side range check at processing time, no rewind for MVP; position-history ring kept for future lag comp (§13.2) |
| Spell casts | per-spell `cast_time` + 2 s post-cast | Mana/vigor/reagent/karma gates per spec |
| HP/mana/vigor regen | event-driven timers per entity (M59 `CalculateHealthTime/ManaTime`) | Same formulas; sanctuary ×2/×3; faction regen phase 2 |
| Advancement/HP-gain rolls | on kill events | Same highmark math; write-through to PG on +1 HP / +1% milestone |
| Snapshot saver | every 60 s dirty-entities + on critical events | Revision-guarded conditional writes only (§8.1); write-through: death, trade accept, logout, char create/delete, guild/faction change (phase 2) |

- MUST: sim uses injectable clock + RNG for deterministic tests.
- MUST: all damage/rolls happen server-side; client-sent damage values are
  rejected (anti-cheat carries M59's buffed-Max halving, PK loot tags, reagent
  checks).

### 5.1 Cell ownership and handoff (single-writer seam for future sharding)

MVP runs all cells in one process, but the ownership rules below are
normative NOW so sharding later is a transport change, not a rewrite:

- An entity has exactly one authoritative cell owner at any tick.
- Handoff carries an ownership epoch (cell + `u64` generation bumped per
  transfer). Source cell stops mutating the entity after hand-over;
  destination installs it before/at a well-defined tick and only then
  accepts its intents.
- Intents arriving mid-migration are queued at the gateway and routed to
  the destination owner — never processed twice, never dropped silently
  (sender gets `202 error{retry}` only if the queue itself is saturated,
  §7).
- Cross-cell actions split by kind — a coordinator cell NEVER mutates a
  neighbor directly:
  - Combat: attacker owner validates (range/LOS/cooldowns against a
    committed neighbor snapshot) → emits `DamageIntent{opID u64, ...}`
    to the target owner → target owner applies damage **exactly once**
    (dedupes by `opID`; receivers are idempotent, retries safe).
  - Atomic multi-party ops (trade accept, give): coordinator validates
    both sides against current revisions → ONE PG transaction commits
    durable ownership (§8.1) → commit messages carrying the same `opID`
    update each owning cell; cells apply only the `opID` they haven't
    seen.
  - Every cross-cell operation carries an `opID` (`u64` snowflake-ish:
    time + worker + counter); all receivers idempotent. This preserves
    the single-writer invariant while actually allowing HP and
    inventories to change across boundaries.
  - Post-commit delivery failure: if the PG transaction committed but an
    in-memory commit notification never reaches/applies on an owning
    cell, that aggregate MUST be reloaded/reconciled from PG before it
    accepts further mutating intents. PG is authoritative — recovery is
    reload, never "make the notification transactional".
  - `opID` dedupe is bounded, not infinite: per-entity/cell recent-`opID`
    cache covering at least the maximum internal retry/handoff window;
    entity handoff carries the recent dedupe state so a retry across a
    handoff cannot double-apply.

## 6. WebSocket protocol (binary, versioned — DECISION §13.8)

Framing: every WS message is one binary frame (D2):
`[u16 opcode][u16 msg_version][u32 seq][u32 tick][payload...]`.
Integers little-endian; `string` = `u16 len + UTF-8 bytes` (max 1024
bytes; chat text max 512; `accessToken` max 8 KiB — Keycloak JWTs with
roles/claims routinely exceed 1 KiB); `array` = `u16 count + elements`
(max 1024 elements); `cell` = `i32 cx + i32 cz`; `pos` = `3×i32`
millimeters (fixed-point, deterministic); angles `u16` 0–4095 (M59's
12-bit convention). The application frame ceiling remains exactly
64 KiB (`proto.MaxFrameSize` = 65536). The WebSocket connection MUST
set its message read limit to 65536 bytes. A message that exceeds the
WebSocket read limit is never parsed and MAY be terminated directly by
the transport with WebSocket status 1009 (Message Too Big). This is
the only transport-level exception to the usual `202 protocol_error`
response rule, because the application does not receive a complete
bounded frame from which to produce a protocol reply. Frames that are
received by the application but are malformed for other reasons use
`202 protocol_error` and do not cause a disconnect on the first offense
unless continuing the connection is unsafe. Codecs use stdlib only — Go
`encoding/binary`, Godot `PackedByteArray.encode_*/decode_*` (both C++,
fast on low-end; no protobuf/GDExtension weight). Protobuf is the
documented escape hatch if hand maintenance ever stops scaling; the
opcode envelope survives that migration.
Versioning: `protoVersion` (in `100 hello`) versions the whole protocol
— breaking changes bump it and old clients are rejected. `msg_version`
(per-message) versions one message's layout — additive changes bump it
and parsers MUST ignore trailing unknown bytes (forward compatibility
within a `protoVersion`). For repeated structures this is NOT enough
(frame boundaries don't delimit entries), so EVERY repeated entry is
`[u16 entryLen][entry bytes]`: parsers decode the known prefix and skip
the remainder. Changing an entry layout is therefore a `msg_version`
bump, never a silent break; removing/renaming fields is a
`protoVersion` break.
Runtime message versions start at 1. Every currently frozen message
layout in `protoVersion` 1 uses `msg_version = 1`. An additive change
to one opcode increments only that opcode's `msg_version`; other
opcodes remain at their existing versions. Receivers MUST continue
decoding the known prefix and ignoring unknown trailing fields as
already specified. `msg_version = 0` is not emitted by normal runtime
senders. The M2 golden fixtures deliberately use 0 as fixture-only
test data and remain valid decoder compatibility vectors. There is no
`msg_version` negotiation protocol, and a receiver MUST NOT reject an
incoming message merely because its `msg_version` is greater than 1;
M2 forward-compatible decoding remains binding.
Sequencing: header `seq` is a per-session `u32` counter (S→C and C→S
independent). Comparison is modulo-2³² serial arithmetic (RFC 1982
style); wraparound is normal, not an error. Header `tick` is the `u32`
sim-tick counter (wraps, same arithmetic); `200 welcome` additionally
carries `serverTimeMs u64` wall-clock for client clock sync — the two
fields are distinct by construction. C→S header `tick` means “latest
estimated server tick when this input was sampled” (drives the future
lag-compensation rewind, §13.2; MVP validates it only for sanity:
reject ticks > 5 s in the future).
Identity: the wire NEVER carries database IDs. All `entity`/`item`/
`target`/`vendor`/`container` fields are `u32 NetEntityID` —
session-local handles issued in `203 cell_snapshot` / `204
entity_create` / `211 inventory_delta` / `220 shop_list` and invalidated
by `206 entity_remove` or disconnect. IDs allocate monotonically per
session and MUST NOT be reused until reconnect/session reset (no ABA:
a stale command referencing an old handle stays invalid forever).
Durable IDs stay `BIGINT` inside PG only. Using a stale/invalid handle
→ `202 error`, never a crash.
Namespaces: fields named `entity/item/target/vendor/container/from`
are NetEntityID `u32`. Fields named `spell/skill/proto/listing` are
STABLE `u16` IDs from seed data (`105 use{kind=0}` → stable skill ID;
`kind=1` → item NetEntityID; `114 buy{listing}` → stable shop-listing
ID defined per vendor proto in seed files).

### 6.1 Session lifecycle

States: `CONNECTED → AUTHENTICATED → CHARACTER_SELECTED → IN_WORLD`.
`100 hello` authenticates (D6) and opens an **account session**
(`CONNECTED → AUTHENTICATED`) — no character is loaded yet. Per-opcode
permissions (anything else → `202 error{bad_state}`):

```text
121 character_list      AUTHENTICATED and later
122 character_create    AUTHENTICATED only
123 character_delete    AUTHENTICATED only
124 enter_world         AUTHENTICATED only
125 ack                 CHARACTER_SELECTED / IN_WORLD
126 leave_world         IN_WORLD only
102–120 gameplay       IN_WORLD only
```

- `121 character_list {}` → `216 character_list {count u16 +
  [[u16 entryLen]{slot u8, charName string, level u16}]...}`. `level`
  means `characters.vitals.base_max` (HP/BaseMaxHP is the character's
  level, so fresh characters list as 20). Malformed persisted vitals
  are a server-side invariant failure — never silently emit level 0.
- `122 character_create {slot u8 (0/1), name string, gender u8,
  face {hairStyle u8, hairColor u8, skinTone u8, parts u8[5]},
  stats u8[6], spells {count u16 + u16[..]}, skills {count u16 +
  u16[..]}}` → `217 character_op {op u8, ok u8}` (failures also via
  `202 error` with codes: `name_taken`, `slot_occupied`, `bad_stats`,
  `bad_budget`). Slot + stats + budget validated server-side per §9; row
  created in the same PG txn (§8.1).
- `123 character_delete {slot u8}` → `217 character_op` (soft-delete;
  name becomes reusable, §8). Invalid or empty slot → `217
  {op=delete, ok=0}`. Deletion runs under the same per-account
  lifecycle guard as `enter_world`/`leave_world`/takeover, and a target
  character bound to ANY live session in `CHARACTER_SELECTED` or
  `IN_WORLD` (i.e. being streamed/loaded, not only fully in-world) is
  rejected with `202 error{character_in_use}`. The store performs the
  revision-CAS soft-delete; a stale CAS / transient persistence
  conflict surfaces as `202 error{retry}` at the WS layer.
- `124 enter_world {slot u8}` → loads character, binds session to it
  (`AUTHENTICATED → CHARACTER_SELECTED`), then streams the baseline:
  `217 character_op{enter,ok}`, then `203 cell_snapshot` (+ `218
  chunk_fragment`s for classic mode, `220 shop_list`s for nearby
  vendors), then `219 world_ready {}` — and ONLY at `world_ready` does
  the session enter `IN_WORLD`. One AOI spans multiple cells, so the
  first snapshot is NOT the boundary; `world_ready` is. The slot must
  be 0/1 and hold a live character of the authenticated account:
  invalid/empty slot → `217 {op=enter_world, ok=0}` (never `bad_state`,
  `character_in_use`, or `retry` for an ordinary missing slot), while a
  malformed 124 payload stays `202 error{protocol_error}` and an
  unavailable character lookup is `202 error{retry}` with the session
  still `AUTHENTICATED`/unbound and no baseline emitted. Full
  enter-world lifecycle, provisional-baseline, and takeover semantics
  are frozen in §6.1.2.
- `126 leave_world {}` → unbinds the character (AOI cleared, presence
  dropped, dirty state flushed), session back to `AUTHENTICATED`. This
  is how characters are switched WITHOUT reconnecting. Required
  ordering: hold the per-account lifecycle guard → quiesce/clear
  AOI/drop presence/flush dirty character through the world seam →
  only after successful flush, unbind the character and transition
  `IN_WORLD → AUTHENTICATED`. A failed flush yields `202
  error{retry}` with the session still `IN_WORLD` and the character
  still bound — no partial leave. No response opcode is defined for a
  successful leave.
- `125 ack {ackSeq u32}` — client acknowledges highest applied S→C
  `seq`. ACKs drive flow control only (§7); the server keeps NO replay
  buffer. Reconnect (new WS + `hello`) ALWAYS performs a full resync
  (fresh snapshots); previous `seq` state is discarded.

Duplicate login / multibox (LOCKED): **one `IN_WORLD` session per
account.** A second `enter_world` (same or other character) kicks the
old world session (`202 error{kicked}`) and binds the new one. Same-
account simultaneous play/multiboxing and self-trading are therefore
impossible by construction. `enter_world` arbitration MUST be serialized
per account (mutex/actor-owned registry op in the single process) so two
simultaneous `enter_world` requests cannot both transiently reach
`IN_WORLD`. The same account lifecycle guard serializes `enter_world`,
`leave_world`, forced takeover, AND character deletion: deleting the
currently `IN_WORLD` character returns `202 error{character_in_use}`;
on forced takeover of the same character the old actor is
quiesced/flushed (or its session directly rebound) BEFORE the
replacement finishes its baseline and receives `world_ready` — a new
connection MUST NOT load stale PG state while the old connection still
holds dirty in-memory state.

#### 6.1.2 Enter-world baseline lifecycle (frozen, v0.3.11)

Normal uncontended enter, under the per-account lifecycle guard held
across the ENTIRE logical operation (decode → re-read → lookup →
arbitration → bind → 217 → baseline → 219 → complete): the guard spans
baseline emission and the `world_ready` barrier deliberately, so a
second same-account enter, deletion, leave, or takeover cannot overlap
a baseline. (T5 later moves delivery onto bounded queues; the logical
serialization does not change.)

- Atomic begin: `AUTHENTICATED` + unbound becomes
  `CHARACTER_SELECTED` + bound as one registry step — never
  `CHARACTER_SELECTED`-yet-unbound or `AUTHENTICATED`-yet-bound.
  Preconditions: session exists and is `AUTHENTICATED`, no current
  binding, requested character free. Any failure mutates nothing.
- Provisional state: while the baseline streams,
  `CHARACTER_SELECTED` + bound + indexed. Gameplay 102–120 stays
  rejected by the lifecycle table; ack/reauth/character_list stay
  permitted (the synchronous read loop processes no further client
  message until the enter handler returns — no second reader).
- Order: `217 {enter_world, ok=1}`, then baseline events in
  provider-emitted order (203/218/220 only; the provider never emits
  217/219/202), then `219 world_ready`. Only after the 219 write
  succeeds does the registry atomically complete
  `CHARACTER_SELECTED → IN_WORLD` keeping the same binding — so no
  inbound gameplay can run before the client holds the barrier AND
  the registry is `IN_WORLD`. A post-219 complete failure is an
  internal invariant failure (terminate; never report success).
- Provisional-baseline rule: anything sent before `world_ready` is
  provisional. A baseline that never terminates in `world_ready` is
  incomplete and MUST be discarded by the client. On operational
  provider failure (socket still writable): roll back to
  `AUTHENTICATED`/unbound, send `202 error{retry}`, no 219 — the same
  socket may retry 124. On a network-write failure of any of
  217/203/218/220/219: best-effort rollback, return the write error,
  and let the connection terminate (never a 202 over a broken write).
  No `baseline_cancel` opcode exists.
- T4a staging: if another same-account session is already
  `CHARACTER_SELECTED`/`IN_WORLD`, the new enter returns `202
  error{retry}` with zero mutation (no kick, no flush, no baseline).
  Same-account session discovery is deterministic and order-free; more
  than one other world-active session is an internal invariant
  failure, never resolved by picking one.
- T4b final behavior (frozen now, implemented later): after guard
  acquisition — re-read new session, resolve character, identify the
  old same-account world session, quiesce/flush it via the takeover
  seam, and only on flush success unbind it to `AUTHENTICATED`, send
  best-effort `202 error{kicked}`, force-close the old socket, then
  run the normal new-session baseline. Flush failure → new session
  gets `retry`, old stays `IN_WORLD`/bound/unkicked, no baseline
  begins. A broken old socket does not block takeover once its state
  was safely flushed and unbound. Simultaneous same-account enters
  serialize on the guard: the loser sees a completed winner and takes
  over normally. Different accounts never block each other.

#### 6.1.1 `217 character_op` numeric registry (frozen, v0.3.10)

The wire layout is unchanged: `217 character_op {op u8, ok u8}`.

```text
op:
    1 = create
    2 = delete
    3 = enter_world

ok:
    0 = rejected / operation did not happen
    1 = success
```

Values outside these remain reserved for future additive semantics;
adding named values does not change the layout or require a
`msg_version` bump. Current server semantics: create success →
`217 {op=1, ok=1}`; delete success → `217 {op=2, ok=1}`; enter_world
success → `217 {op=3, ok=1}`. `ok=0` is used only for simple request
rejection where no more specific machine-readable `202` code exists
(e.g. invalid slot, missing character in the requested slot,
syntactically invalid display name). `ok=0` MUST NOT stand in for
`name_taken`, `slot_occupied`, `bad_stats`, `bad_budget`,
`character_in_use`, or `retry` — those keep their `202` numeric codes.
Name rejection splits accordingly: invalid syntax → `217
{op=create, ok=0}`; blocklisted/reserved/live-name conflict → `202
error{name_taken}` (deliberately revealing only "unavailable", never
which list matched). No new wire error code is added for names.

### 6.2 Connect / re-auth

- `100 hello {clientVersion u32, protoVersion u16, accessToken string}`
  → `200 welcome {serverTimeMs u64, chunk u8, aoiRadius u16, tickRates
  {count u8 + u16[..]}, world{mode u8, seed u64, version u32}}` or `202
  error`. `accessToken` = Keycloak access JWT (D6), JWKS-validated
  (`iss`/`aud`/expiry/signature), `sub` mapped to account
  (auto-provisioned, §8). Opens the account session (`→ AUTHENTICATED`).
- `101 reauth {accessToken string}` over the live WS → `201 reauth_ok {}`
  or `202 error{session_expired}` (→ browser login, then full resync).
  Re-auth has a hard deadline (§11): 90 s grace after token expiry, then
  disconnect. Exact hello/reauth/deadline semantics are frozen in
  §6.2.2.

#### 6.2.1 Access-token validator (staged: M3 baseline vs M11 hardening)

The backend validates already-issued access JWTs; it never performs
Authorization Code exchange, PKCE, browser/loopback, refresh-token, or
ID-token flows (client/M11 concerns). Realm/client names, token TTLs,
and deployment configuration are M11-T1 decisions. M3 receives three
explicit trusted inputs — `issuer`, `audience`, `jwksURL` — all
non-empty, with exact `iss` comparison and `aud`-contains matching.
The JWKS URL comes only from trusted server configuration: it is NEVER
derived from JWT headers, untrusted `jku`/`x5u` headers are NEVER
honored, and there is no OIDC discovery in M3. Libraries: `jwx/v4
v4.4.0` + `jwkfetch/v4 v4.0.4` (§2, Go 1.27 stdlib JSON, no
`GOEXPERIMENT`).

M3-T2 baseline: one real HTTP JWKS fetch at validator
construction/startup (exact-allowlisted URL, bounded body), the fetched
JWK set held immutable in memory, no per-token network fetch, full
signature + core claim validation, real account auto-provisioning, real
hello/reauth, and the deterministic 90 s authorization deadline of
§6.2.2. M11-T2 upgrades this baseline with background cache, key
rotation, cache TTL, retry/backoff, stale-key behavior, pre-auth
per-IP rate limiting, and the expanded adversarial suite. The final §11
cached-JWKS requirement stays normative; M3 is the explicitly staged
baseline, not a full implementation of it.

A valid access JWT MUST satisfy all of: (1) structurally valid signed
JWS; (2) signature verifies against the pre-fetched trusted JWKS;
(3) key selected by `kid` from the trusted set; (4) verification
algorithm from trusted JWK metadata, never blindly from the JWT
header; (5) `iss` exactly matches the configured issuer; (6) `aud`
contains the configured audience; (7) `exp` present; (8) `exp` still
in the future at validation time; (9) `sub` present and non-empty;
(10) `nbf`/`iat` validity enforced when present. `jwt.ParseInsecure`,
unverified/unvalidated parse modes, and `alg=none` are forbidden, as
is inferring verification keys from the token itself. The 8 KiB
`accessToken` wire cap is unchanged.

The `email` claim is optional: absent, or present as a non-empty
string. A wrong-typed `email` invalidates the token; empty string
counts as absent. Account identity is Keycloak `sub`, not email.
First auto-provision writes `accounts.keycloak_sub` = validated `sub`
and `accounts.email` = validated email or NULL; an existing account
keeps its stored email (login never re-synchronizes it) and re-auth
performs no email persistence.

Auto-provision is race-safe on the existing UNIQUE
`accounts.keycloak_sub`: lookup by `sub`, return the existing ID when
found, else INSERT; a concurrent UNIQUE-race loser re-reads by `sub`
and returns the winner's ID. Two simultaneous first logins converge on
one durable row and one ID. No migration required.

#### 6.2.2 Hello, re-auth, and the 90 s authorization deadline (frozen, v0.3.9)

Successful hello: `DecodeHello` → validate access JWT → obtain `{sub,
email?, exp}` → `EnsureAccount(sub, email?)` →
`Registry.Authenticate(sessionID, sub, accountID, exp)` → arm the hard
authorization deadline → send `200 welcome`. The transition stays
`CONNECTED → AUTHENTICATED`; no character is loaded and no character
rows are queried. JWT-invalid hello (bad signature, expired, wrong
issuer/audience, missing/empty `sub`, missing `exp`, malformed JWT) →
`202 error{session_expired}`, session stays `CONNECTED`. JWT-valid but
account mapping unavailable (PG down, query/insert failure) → `202
error{retry}`, session stays `CONNECTED` (new logins are rejected while
persistence is unavailable per §10, without blaming the JWT). No
account row means no `AUTHENTICATED`.

Re-auth over an established session: validate the new JWT exactly like
hello (it must be currently valid — grace never extends token
validity) → validated `sub` MUST equal the session `sub` (else `202
error{session_expired}`, mutating nothing: no `Sub`/`AccountID`/
`TokenExp`/deadline change, no auto-provision, no account switch) →
update only `TokenExp` → arm a replacement deadline → send `201
reauth_ok`. Re-auth performs no PG provisioning/query, so it keeps
working during a temporary PG outage. A failed reauth leaves the old
identity, old `TokenExp`, and remaining grace intact.

Deadline: `ReauthGrace = 90 s`;
`authorizationDeadline = TokenExp + ReauthGrace`. An already-expired
token at hello is invalid immediately — no grace opens a new session.
For an authenticated session: before `TokenExp`, normal operation; on
`[TokenExp, TokenExp + 90 s)`, the session stays usable and a fresh
currently-valid token may reauth; at `now >= TokenExp + 90 s` the old
authorization is dead: no new application opcode is dispatched (not
even reauth — validation cannot rescue the session), the server
best-effort sends `202 error{session_expired}` and closes the WS, and
the client must reconnect + hello. The disconnect MUST fire even when
the client is idle, so the deadline is a scheduled timer, not merely
an on-next-message check. Successful reauth cancels/supersedes the old
timer: a late-firing stale callback MUST NOT disconnect the
reauthenticated session. All expiry/deadline comparisons use an
injectable clock (tests never wait a real 90 s).

### 6.3 Message catalog

- C→S gameplay intents (`102–120`, `126`, `IN_WORLD` only except `126`):
  `102 move {inputSeq u32, heldDirs u8 bitmask, runFlag u8, yaw u16}`
  (`inputSeq` uses the same modulo-2³² serial arithmetic as header `seq`;
  intents only — client-sent positions rejected; `yaw` carries
  facing/heading since `angle` in `205` would otherwise have no C→S
  source; header `tick` = sampling tick, §6 framing), `103 attack
  {target u32 NetEntityID}`, `104 cast {spell u16 stable, target u32}`,
  `105 use {kind u8, id u32}` (fixed `u32` union payload: `kind=0 skill`
  → `id` MUST fit `u16` stable skill ID; `kind=1 item` → `id` is
  NetEntityID),
  `106 get {entity u32, item u32}`, `107 drop {item u32}`,
  `108 put {item u32, container u32}`, `109 give {target u32, item u32,
  qty u16}`, `110 offer {target u32, items {count u16 + u32[..]}}`,
  `111 counter {items {count u16 + u32[..]}}`, `112 accept {}`,
  `113 cancel {}`, `114 buy {vendor u32 NetEntityID, listing u16 stable,
  qty u16}` (listing IDs defined per vendor proto in seed files),
  `115 rest {state u8}`, `116 eat {item u32}`, `117 say {channel u8, text
  string}`, `118 say_group {text string}`, `119 safety_toggle {}`,
  `120 respawn_ack {}`, `126 leave_world {}`. Unknown/rate-limited
  intents → `202 error` (no disconnect on first offense).
- S→C deltas (`202–220`): `202 error {code u16, message string}`,
  `203 cell_snapshot {cell, count u16 + [[u16 entryLen]entityEntry]...}`
  where `entityEntry = {entity u32 NetEntityID, kind u8, proto u16, pos,
  angle u16, speed u8}` (`proto` = stable ID in the namespace selected by
  `kind`: spell/skill/item/mob proto or vendor ID — so the client knows
  orc from troll from banker), `204 entity_create {entityEntry}`, `205 entity_move
  {entity u32, pos, angle u16, speed u8, lastProcessedInputSeq u32}`
  (the reconciliation anchor for the client's OWN character only; for
  entities not controlled by this session it MUST be 0 and MUST be
  ignored by the client), `206 entity_remove
  {entity u32}`, `207 stat {entity u32, statId u8, value/min/max/curmax
  i32}` (M59 shape — reuse for HUD), `208 stat_group {entity u32, count
  u16 + [[u16 entryLen]{statId u8, value/min/max/curmax i32}]...}`,
  `209 said {from u32, channel u8, text string}`, `210 effect {id u16,
  target u32, pos}`, `211 inventory_delta {count u16 + [[u16
  entryLen]inventoryEntry]...}` where `inventoryEntry v1 = {item u32
  NetEntityID, proto u16 stable item-proto ID, qty u16, hits i32,
  location u8, container u32 NetEntityID, slot string}` (field order
  binding; fixed prefix 19 bytes + slot UTF-8 bytes: 4+2+2+4+1+4+2;
  `location` 0 = directly owned by the current character with
  `container` ignored (senders emit 0), 1 = contained in another
  inventory item named by `container`; unknown `location` values are
  reserved for additive evolution and MUST NOT be rejected by the
  codec; `slot` = authoritative item-location slot label, general
  1024-byte string cap, no numeric slot IDs; `hits` is signed to match
  the persisted representation, no gameplay meaning imposed; `qty`
  stays `u16`, producers emit wire-representable quantities;
  `item_instances.revision`, raw enchants JSONB, and database IDs NEVER
  cross this message — future typed enchant metadata appends to the
  entry under a `msg_version` bump and is skipped via `entryLen`).
  `211` entries are authoritative upserts for client-visible inventory
  state; removal/invalidation of an item handle uses `206
  entity_remove` — there is deliberately no inventory-specific delete
  op, so two competing removal mechanisms can never diverge),
  `212 offer_update {with u32, state u8, count u16 + [[u16 entryLen]{item
  u32, qty u16}]...}`, `213 trade_result {ok u8}`, `214 death {victim
  u32}`, `215 respawn {pos}`, `218 chunk_fragment {cell, chunkIdx u32,
  fragIdx u16, fragCount u16, byteLen u16, bytes u8[byteLen]}`
  (`byteLen` ≤ 60 KiB = 61440, so the max complete frame is
  12+8+4+2+2+2+61440 = 61470 bytes, safely under 64 KiB; the explicit
  length lets decoders read exactly `byteLen` bytes and then ignore any
  future `msg_version` trailing fields per the global versioning rule;
  classic mode voxel streaming needs reliable-lane pacing; the
  production fragment-per-tick rate (≤N fragments/tick/session) belongs
  to the REAL production world streamer — M3-T5 implements bounded
  reliable queueing/backpressure only (§7.1) and defines no
  fragment-per-tick config value, while M10-T4 chooses and validates the
  production `N` against the real world source and min-spec bandwidth;
  procedural mode never sends these), `219 world_ready {}` (baseline
  boundary, §6.1), `220 shop_list
  {vendor u32, count u16 + [[u16 entryLen]{listing u16 stable, price u32,
  qty u16}]...}`.
- Reconciliation (normative): the client tags every `102 move` with
  `inputSeq` and predicts locally. On each `205` for its own character
  it compares server `pos` against its prediction for
  `lastProcessedInputSeq`; if error > 0.5 m it rewinds to server state
  and reapplies inputs `lastProcessedInputSeq+1…`. Server never sees
  client predictions — the 0.5 m rule is enforced client-side against
  authoritative snapshots; server-side anomaly detection (§11) stays an
  independent tripwire.

### 6.4 `202 error` numeric registry (frozen, v0.3.8)

The wire layout is unchanged: `202 error {code u16, message string}`.
The stable numeric registry is:

```text
0   reserved / unspecified
1   bad_state
2   protocol_error
3   session_expired
4   kicked
5   character_in_use
6   retry
7   name_taken
8   slot_occupied
9   bad_stats
10  bad_budget
11  rate_limited
12  invalid_handle
```

Rules:

- `0` MUST NOT be emitted for a currently defined error reason.
- Numeric codes are the machine-readable contract. `message` is
  diagnostic/human-readable context and MUST NOT be parsed by clients
  for control flow.
- Clients MUST tolerate unknown future numeric error codes and may
  display/log `message`.
- Adding a new numeric code is additive and does not change the `202`
  message layout or require a `msg_version` bump.
- Existing numeric meanings MUST NEVER be reassigned.
  Removing/reinterpreting an existing numeric meaning is a
  `protoVersion`-breaking change.
- `protocol_error` is the reason for protocol-level problems such as:
  unsupported/unknown opcode, a client sending an S→C-only opcode, a
  non-binary WebSocket application message, a malformed frame below the
  transport read ceiling, or a malformed hello/reauth payload.
- `bad_state` is ONLY a known C→S opcode that is structurally valid but
  not permitted in the session's current lifecycle state (§6.1).
  `bad_state` MUST NOT be used for unknown opcodes.

## 7. In-memory ephemeral state (no external store)

Sessions, presence, and rate limits live in gateway-owned in-memory
registries (guarded maps / `sync.Map`; single process, no cross-instance sync).

- Sessions: `sessionID → {sub, accountID, charID NULLABLE (set at
  `enter_world`), conn, state (§6.1), tokenExp}`, indexed by `sub` and by
  character. Keyed by Keycloak `sub`, not by opaque token. Game session
  lives as long as the WS is up and the client keeps re-authing (hard
  90 s deadline past token expiry, §11); expiry of one access token does
  NOT drop the session. Logout/death do NOT rely on it.
- Presence: `charID → {conn, currentCells, heartbeatAt}`. Heartbeat is
  WebSocket Ping/Pong every 15 s (no application heartbeat message);
  sweep every 30 s drops dead conns. AOI cell sets derived from sim
  positions, deleted on disconnect.
- Rate limits: per-character token buckets in memory (movement/intent caps).
- Cross-cell fanout: in-process channels from sim to gateway (same shape a
  future bus would carry, so sharding later only swaps the transport).
- Restart semantics (accepted tradeoff of D3): sessions drop on restart —
  clients re-login; presence rebuilds on connect; authoritative sim state
  restores from PG materialized state (D7, §8.1). MUST document this in the
  client reconnect flow (`202 error{session_expired}` → re-login → full
  resync).

### 7.1 Slow-client and backpressure rules (frozen, v0.3.12)

M3-T5 delivers this section in two parts: T5a implements the bounded
two-lane queue/backpressure core (everything below except the ACK window
and metric registration); T5b adds opcode-125 flow control plus the
Prometheus metrics. The split is a plan concern — the semantics below
are frozen now.

#### 7.1.1 Exact outbound configuration

Every session owns one bounded outbound queue configured by (MVP
defaults shown):

```yaml
outbound:
  max_messages: 1024
  max_bytes: 262144
  reliable_enqueue_timeout_ms: 1000
  write_timeout_ms: 5000
  max_unacked_messages: 1024
```

- `max_messages`: total resident outbound messages for one session.
- `max_bytes`: total resident complete-frame bytes for one session.
- `reliable_enqueue_timeout_ms`: maximum time a synchronous reliable
  producer may wait for queue capacity before the session is failed
  closed as a slow client.
- `write_timeout_ms`: maximum time one normal queued physical WebSocket
  write may take.
- `max_unacked_messages`: T5b application-level ACK lag window after
  entering IN_WORLD (§7.1.11).

Environment overrides (exact names, no alternate aliases; the normal
precedence `defaults < config.yaml < VOX_*` applies):

```text
VOX_OUTBOUND_MAX_MESSAGES
VOX_OUTBOUND_MAX_BYTES
VOX_OUTBOUND_RELIABLE_ENQUEUE_TIMEOUT_MS
VOX_OUTBOUND_WRITE_TIMEOUT_MS
VOX_OUTBOUND_MAX_UNACKED_MESSAGES
```

Validation (integral milliseconds only — no floating-point durations):

```text
max_messages                 1..65535
max_bytes                    >= 65536 and <= 67108864 (64 MiB ceiling)
reliable_enqueue_timeout_ms  1..60000
write_timeout_ms             1..60000
max_unacked_messages         1..1000000
```

`max_bytes >= 65536` guarantees any one valid Voxilian frame (≤ 64 KiB,
§6) can fit the configured queue budget. `max_unacked_messages` is
frozen now, stays safely below the 2³¹ serial-ambiguity boundary, and
is implemented by T5b only.

#### 7.1.2 Budget accounting

The session's outbound budget counts:

```text
queued frames + the frame currently being physically written
```

until that write completes or fails — a frame remains resident for
budget purposes while a slow socket is holding it. The two lanes share
the configured `max_messages`/`max_bytes` TOTAL resident budget: the
configured limit is per SESSION, not per lane. Byte accounting is exact
complete protocol-frame bytes including the 12-byte Voxilian frame
header; payload size is never estimated and Go object overhead is never
counted.

#### 7.1.3 Lanes and coalescing

Every session has exactly two outbound lanes — no third normal lane in
M3:

```text
critical/reliable lane    FIFO, ordered, never silently dropped
coalescible-state lane    keyed, newest-wins, droppable
```

Critical lane: all currently-existing gateway `SendFunc` traffic is
critical by default, without exception — `200 welcome`, `201
reauth_ok`, `202` ordinary protocol/application errors, `203 baseline
snapshots`, `204 entity_create`, `206 entity_remove`, `207/208 stats`,
`209 chat`, `210 effects`, `211 inventory_delta`, `212 offer_update`,
`213 trade_result`, `214 death`, `215 respawn`, `216 character_list`,
`217 character_op`, `218 chunk_fragment`, `219 world_ready`, `220
shop_list`. Future code may explicitly use another producer API for
coalescible state; an opcode MUST NOT be inferred coalescible merely
because it is a "delta".

Coalescible lane: M3 freezes one canonical use — `205 entity_move`
keyed by entity NetEntityID. Future absolute/latest-state producers may
opt into the same lane using an explicit producer-supplied key; the
outbound queue MUST NOT decode payloads to discover a coalescing key. A
generic comparable key shape such as `{Kind uint16, ID uint64}` is
appropriate (exact Go naming may differ).

Same-key coalescing: if a coalescible state message with key K is
queued but has not begun writing and a newer message for K arrives, the
old queued value is replaced by the new one. The replaced message is
never written, never receives an S→C seq, and consumes no ACK-window
state — the newest value wins. Once a state frame has been selected by
the writer and begins its physical write it is no longer replaceable; a
new value for the same key may become the next queued value. Bytes are
never mutated underneath an active WebSocket write.

#### 7.1.4 Saturation and eviction

State saturation: for a NEW coalescible state key that cannot fit the
total resident budget, drop the new state update and keep the session
alive (no sequence is allocated — deliberate overload shedding). For a
replacement of an existing key, remove the old queued value first, then
attempt to admit the newest value; if the newest still cannot fit
because critical traffic consumes the budget, drop the new value and
leave no stale older value for that key — known-stale state is never
retained merely because it was smaller.

Critical traffic may evict state: when admitting a critical frame,
queued coalescible state is expendable. The queue evicts oldest queued
state entries as necessary to make room; critical frames themselves are
never evicted. If the frame still cannot fit because resident CRITICAL
traffic alone fills the budget, the critical saturation behavior
(§7.1.6) applies.

#### 7.1.5 Synchronous reliable producer

The existing gateway `SendFunc` remains logically synchronous:

```text
SendFunc → encode payload → admit critical frame → wait until that
exact frame is physically written or definitively failed → return
```

This preserves all existing handler assumptions; in particular the T4
baseline (`217`, `203/218/220…`, `219`) continues to have a real
physical `world_ready` barrier — all baseline snapshots and required
classic-mode fragments MUST have been physically written before `219`.
`SendFunc` returning nil means the frame was physically written; it
MUST NOT mean merely "queued successfully".

A synchronous reliable producer may wait for resident queue capacity,
but only up to `reliable_enqueue_timeout_ms` and its caller context —
whichever deadline/cancellation occurs first wins. If capacity does not
become available in time, the session is classified slow, the
connection is failed closed, and the send returns a stable slow-client
error. Producers never block indefinitely.

Every NORMAL queued physical WebSocket write uses a fresh internal
`write_timeout_ms` timeout — never one inherited solely from a
long-lived HTTP request context. On write timeout the session fails
closed as a slow client: all pending synchronous reliable senders are
woken/failed, queued state is discarded, remaining critical queue
contents are discarded, and the WebSocket is force-closed. Reconnect and
full resync (§7.1.10) are the recovery mechanism.

#### 7.1.6 Non-blocking producers and fail-closed slow sessions

M4 cell-owner goroutines use a future-sim-facing non-blocking producer
seam with two operations, conceptually `TryCritical(...)` and
`TryState(key, ...)`. They MUST NOT wait for a socket write, wait for
queue capacity, sleep, or spin — they return based only on immediate
bounded in-memory queue state.

`TryCritical` tries immediate admission (evicting coalescible state if
useful). If the critical frame still cannot fit because critical
backlog alone fills the budget, the critical event is NOT dropped: the
affected session is failed closed and a stable slow-client/saturation
result is returned. Cell owners never block indefinitely, critical
events are never silently dropped, and a slow affected client is
disconnected instead — the producing cell/world goroutine remains free
to continue.

`TryState` coalesces same key if possible, otherwise admits immediately
if budget allows, otherwise drops the newest state update; it MUST NOT
disconnect solely because one state update was dropped. Its result
distinguishes at least `queued`, `coalesced`, `dropped`, and
`closed/slow` (exact representation is an implementation detail).

When the outbound system drops a slow client — critical queue
saturation, reliable enqueue timeout, or physical write timeout — no
new `202` error code is introduced: the server may simply terminate the
connection, and reconnect performs a new WS, `hello`, fresh
`enter_world`, and fresh full baseline.

#### 7.1.7 Writer pump and scheduling

Normal queued traffic is drained by exactly one bounded-lifetime writer
goroutine per WebSocket session — no goroutine per message, no
unbounded worker pool, no one-goroutine-per-waiter admission. The pump
schedules the critical lane first; the state lane runs only when no
critical frame is ready. Critical FIFO order is binding. State ordering
across DIFFERENT keys is not a wire contract but MUST be deterministic
and testable — a small ordered list plus key index is preferable to Go
map iteration.

The T4b connection-level writer gate remains the final physical
serialization: the normal queue writer calls through that existing
low-level writer, and direct terminal/emergency writes may also call
through it. Therefore a queued writer, a terminal kicked write, and an
authorization-deadline write can never physically write one WebSocket
concurrently; no second raw writer path is introduced.

`218 chunk_fragment` remains critical/reliable and is never
coalescible. T4's synchronous BaselineProvider naturally paces
production because each critical `SendFunc` returns only after that
fragment has been physically written; T5a introduces no artificial
sleep or fixed fragment timer.

#### 7.1.8 Frame preparation, sequencing, and completion

To enforce exact byte budgets while preserving seq/write ordering, the
message PAYLOAD is encoded exactly once BEFORE queue admission using
`proto.Encoder`, and those payload bytes are frozen/copied into the
queued item. The full queued size is `proto.HeaderSize + len(payload)`.
The S→C sequence is NOT allocated at admission and the frame tick is
NOT frozen at admission.

Only after the writer selects the queued item and owns physical writer
serialization does it allocate `NextServerSeq`, sample `TickFunc`,
construct the final frame header, append the already-frozen payload,
and physically write. Thus a queued-but-replaced state message, a
queued-but-dropped state message, and a critical admission timeout
never allocate a sequence, and physical wire order remains identical to
sequence allocation order (the T4b invariant stays binding).

The `encode func(*proto.Encoder) error` passed by current gateway
callers is invoked exactly once, during payload preparation — never
once to estimate size and again to write. If payload preparation fails,
the encoding error is returned, nothing is queued, no sequence is
allocated, and the client is NOT classified slow: an internal encoding
bug is not backpressure.

The synchronous critical queue item carries one bounded completion
signal: on physical write success the waiting SendFunc returns nil; on
failure it returns the actual classified error. Closing the session
completes ALL outstanding synchronous waiters — no sender stays parked
forever after the socket has closed.

#### 7.1.9 Queue shutdown and terminal control bypass

Queue/session shutdown is idempotent. Concurrent causes — client
disconnect, write timeout, critical saturation, authorization expiry,
forced duplicate-login kick, normal deferred cleanup — are all safe:
only the first shutdown performs internal queue closure, all later
closes are harmless, and no send-on-closed-channel panics occur.

Two terminal-control frames are the explicit exceptions to normal
queueing, both best-effort DIRECT low-level writer paths that still
serialize on the one physical writer gate (§7.1.7):

- Forced duplicate-login `202 error{kicked}` (T4b): it MUST continue to
  bypass the normal outbound queue — a saturated old queue must never
  prevent authoritative retirement, and the kick must never enqueue
  behind the traffic that made the old session unresponsive.
- Hard authorization-deadline `202 error{session_expired}`: likewise a
  best-effort direct low-level write bounded by the existing deadline
  write context, then connection close. The 90-second authorization
  semantics (§6.2.2) do not change.

#### 7.1.10 Slow-client disconnect and full-resync semantics

After any backpressure/slow-client disconnect, the old session's seq
state, queue contents, coalesced state, and ACK state are discarded; a
reconnect starts a brand-new session and a full baseline (`new WS →
hello → fresh enter_world → fresh full baseline`). Queued frames from
the old socket are never replayed — there is no replay buffer (§6.1).

#### 7.1.11 ACK flow control — frozen for M3-T5b

Opcode `125 ack {ackSeq u32}` remains cumulative: the highest S→C
sequence fully applied by the client. No replay buffer exists. The
window does NOT gate the initial baseline: M3 has one synchronous
per-connection read loop, so the client cannot process an inbound ACK
concurrently while its server-side `124 enter_world` handler streams
the baseline. Baseline `217/203/218/220/219` traffic is governed by
transport/queue backpressure only — never by the application ACK
window, so no baseline deadlock is possible.

When `CompleteEnterWorld` changes `CHARACTER_SELECTED → IN_WORLD`, the
flow-control baseline initializes to the current server S→C sequence —
the already-written `219 world_ready` sequence (`lastAck =
lastFlowSent = worldReadySeq` for flow-control purposes). This
establishes that the baseline itself is outside the steady-state ACK
lag window; it does not synthesize an actual client ACK. Every normal
S→C application frame allocated while IN_WORLD advances the sent-flow
sequence; pre-world AUTHENTICATED traffic and baseline
CHARACTER_SELECTED traffic do not consume the window. A successful
`leave_world → AUTHENTICATED` clears the accounting, and a later enter
initializes a NEW baseline at its new `world_ready` sequence — no stale
ACK debt carries across re-enter or full resync.

T5b enforces: the number of sent IN_WORLD frames not cumulatively
ACKed MUST stay ≤ `max_unacked_messages`. No payloads are retained. If
sending the next frame would exceed the limit, the session is
classified slow, disconnected/fail-closed, and that next frame is not
written. No replay.

ACK acceptance uses modulo-2³² serial arithmetic via the M2 helpers
(`Serial32After`/`Serial32Before`): `0` is legitimately after
`MaxUint32` within the valid serial window; the configured window is
far below 2³¹, so half-range ambiguity never occurs during valid
operation. In IN_WORLD: `ack == lastAck` is a duplicate (no-op); `ack`
before `lastAck` is stale (no-op); `lastAck < ack <= lastFlowSent` is a
valid cumulative advance; `ack` after `lastFlowSent` is future/invalid
→ `202 protocol_error`. A valid, stale, or duplicate ACK receives no
success reply. In CHARACTER_SELECTED — before the flow window is
initialized — a structurally valid ACK is accepted as a no-op with no
response (the lifecycle table already permits opcode 125 there); a
malformed payload still maps to `202 protocol_error`.

#### 7.1.12 Saturation metrics — frozen for M3-T5b

Exact Prometheus names (no session/account/character labels; no
high-cardinality identifiers):

```text
vox_session_drops_total{reason}
vox_outbound_queue_depth_messages{lane}
vox_outbound_queue_depth_bytes{lane}
vox_outbound_state_drops_total{reason}
vox_outbound_state_coalesced_total
vox_outbound_ack_lag_messages
```

Queue-depth lanes are exactly `lane="critical"` and `lane="state"` — no
opcode label, no entity label. Initial stable drop reasons are
`critical_queue_saturated`, `reliable_enqueue_timeout`, `write_timeout`,
and `ack_lag`; forced `kicked` and `session_expired` are NOT
backpressure drop reasons for this metric. T5a exposes only a narrow
no-op-by-default observer seam — sufficient to observe queue depth
messages/bytes per lane, state dropped, state coalesced, and session
slow-drop reason — so T5b can attach Prometheus without rewriting queue
internals; the core queue does not import Prometheus.

The internal-delivery rule is unchanged in spirit: sim→gateway channels
are bounded with defined overload behavior (shed newest movement first,
never critical events; count + log). Cell-owner goroutines MUST NOT
block indefinitely on gateway delivery; critical-lane saturation fails
closed by disconnecting/resyncing the affected sessions rather than
dropping the event or stalling the simulation. Unbounded queues are a
spec violation.

## 8. PostgreSQL schema (authoritative; migrations via goose, queries via sqlc)

Tables (D4; M59 property names in parens where ported):
- `accounts(id, keycloak_sub TEXT UNIQUE NOT NULL, email CITEXT, created_at)`;
  no passwords anywhere — credentials, reset, registration, and mails live in
  Keycloak. First login with an unknown `sub` auto-provisions a row, then
  in-game character creation applies (max 2 chars, §13.3).
- `characters(id BIGINT, account_id FK → accounts, slot SMALLINT 0/1,
  name CITEXT, gender SMALLINT, face JSONB,
  might/intellect/stamina/agility/mysticism/aim SMALLINT,
  karma INT, hometown TEXT,
  pos_x/pos_y/pos_z BIGINT (millimeters, fixed-point — same units as the
  wire `pos`, NOT `POINT` which is 2-D),
  vitals JSONB {hp,base_max,max,mana,max_mana,vigor,threshold,stomach},
  advancement JSONB {adv_points, adv_timer_due, gain_chance, school_casts},
  flags INT (murder/outlaw/safety/pk bits),
  revision BIGINT (D7, §8.1), created_at, updated_at, deleted_at NULL)` —
  soft-delete keeps corpses/audits consistent.
  Uniqueness via partial indexes (migrations use `CREATE UNIQUE INDEX
  ... WHERE deleted_at IS NULL` — partial uniqueness is NOT an inline
  table constraint): `chars_acct_slot_uidx ON characters(account_id,
  slot)`, `chars_name_uidx ON characters(name)`. The slot index enforces
  the 2-char limit **transactionally** (no app-level race); the name
  index lets deleted names be reused while live names stay globally
  unique. Display-name rules DECIDED (§9 creation rule).
- `character_spells(character_id, spell_id INTEGER FK →
  spell_protos(id), ability SMALLINT 1–99, atrophy_flag BOOL)`,
  `character_skills(character_id, skill_id INTEGER FK →
  skill_protos(id), ...)` — PK(char, id). The valid 1..65535 namespace is
  guaranteed by the referenced catalog PK rows themselves; no separate
  SQL domain exists or is required.
- `item_instances(id BIGINT, proto INT FK → item_protos (CHECK 1..65535), qty INT, hits INT, enchants JSONB,
  revision BIGINT, created_at)` + `item_locations(item_id PK → instances,
  kind SMALLINT (0=inventory,1=ground,2=corpse,3=vault,4=container),
  character_id NULLABLE FK, corpse_id NULLABLE FK,
  container_item_id NULLABLE FK → item_instances,
  vault_region TEXT NULLABLE, pos_x/pos_y/pos_z NULLABLE BIGINT mm,
  slot TEXT NULLABLE,
  CHECK (
    (kind=0 AND character_id IS NOT NULL AND slot IS NOT NULL
      AND corpse_id IS NULL AND container_item_id IS NULL
      AND vault_region IS NULL
      AND pos_x IS NULL AND pos_y IS NULL AND pos_z IS NULL) OR
    (kind=1 AND pos_x IS NOT NULL AND pos_y IS NOT NULL
      AND pos_z IS NOT NULL
      AND character_id IS NULL AND corpse_id IS NULL
      AND container_item_id IS NULL AND vault_region IS NULL
      AND slot IS NULL) OR
    (kind=2 AND corpse_id IS NOT NULL AND character_id IS NULL
      AND container_item_id IS NULL AND vault_region IS NULL
      AND slot IS NULL
      AND pos_x IS NULL AND pos_y IS NULL AND pos_z IS NULL) OR
    (kind=3 AND character_id IS NOT NULL AND vault_region IS NOT NULL
      AND slot IS NOT NULL AND corpse_id IS NULL
      AND container_item_id IS NULL
      AND pos_x IS NULL AND pos_y IS NULL AND pos_z IS NULL) OR
    (kind=4 AND container_item_id IS NOT NULL AND slot IS NOT NULL
      AND character_id IS NULL AND corpse_id IS NULL
      AND vault_region IS NULL
      AND pos_x IS NULL AND pos_y IS NULL AND pos_z IS NULL)
  ),
  CHECK (container_item_id IS NULL OR container_item_id <> item_id)
  -- no immediate self-containment. Deeper ancestry cycles (A→B→A) are
  -- rejected by the sim/store transaction at move time, not by SQL.)`. One row in `item_locations` = one location: SQL itself makes
  “item simultaneously in inventory and corpse” impossible. No separate
  `corpse_items`/`vaults-item` tables.
- `corpses(id BIGINT, character_id FK, pos_x/pos_y/pos_z BIGINT mm,
  created_at, expires_at)`.
- `banks(character_id FK, system TEXT, balance BIGINT, revision BIGINT,
  PK(character_id, system))` (two systems like M59: Tos/Jasper-shared vs
  Kocatan — rename to world regions later).
- `ledger(id BIGINT, kind SMALLINT, actor_account_id NULLABLE FK,
  actor_character_id NULLABLE FK,
  CHECK (num_nonnulls(actor_account_id, actor_character_id) = 1),
  cpty_account_id/cpty_character_id NULLABLE FK (both NULL = system/mint,
  CHECK (num_nonnulls(cpty_account_id, cpty_character_id) <= 1)),
  amount BIGINT NULLABLE, qty INT NULLABLE, item_id NULLABLE FK,
  created_at)` — append-only money/item movements (trade/bank/vault/loot).
- `kills(id BIGINT, killer_kind SMALLINT (0=character,1=mob),
  killer_character_id NULLABLE FK, killer_mob_id INT NULLABLE FK →
  mob_protos, victim_kind SMALLINT, victim_character_id NULLABLE FK,
  victim_mob_id INT NULLABLE FK → mob_protos, pos_x/pos_y/pos_z BIGINT mm,
  created_at)` for advancement audit + karma/justice phase 2. One-of
  invariant enforced in SQL: kind 0 ⇒ character set + mob NULL, kind 1 ⇒
  mob set + character NULL, for killer and victim independently; no other
  kind values valid.
- Sanctions are account-scoped current state (enforceable pre-selection,
  surviving character switches; Keycloak identity maps to accounts; no
  history table in MVP): `bans(account_id BIGINT PK FK → accounts(id),
  reason TEXT NOT NULL, expires_at TIMESTAMPTZ NULL, created_at
  TIMESTAMPTZ NOT NULL DEFAULT now())` and identical `mutes(...)`.
  `expires_at NULL` = permanent until revoked; otherwise active while
  `expires_at > now()`; revoke deletes the row. No IPs, no
  character-scoped sanctions, no metadata.
- Indexes: characters(account_id), item_locations(character_id, corpse_id),
  corpses(expires_at), ledger(actor_character_id, created_at),
  ledger(actor_account_id, created_at), kills(victim_character_id,
  created_at). CHECK constraints on stat/ability ranges (1–50 creation /
  1–99 ability) so bad sim code fails loudly.
- sqlc: `queries/*.sql` → `internal/store/gen/`; migrations embed via
  `go:embed`; `voxilian migrate up/down/status`.
- Migration `0001` MUST enable the `citext` extension.

### 8.2 Prototype catalog tables (runtime content registry)

Seed files (YAML, versioned in repo) are the SOURCE; PG catalog tables
are the RUNTIME registry `voxilian seed` upserts into. Sim and gateway
read protos/listing IDs from an IMMUTABLE IN-MEMORY REGISTRY loaded
from PG at startup/world load — gameplay hot paths MUST NOT query PG
for prototype data (this also keeps combat/loot working under the §10
PG-outage grace). A reseed swaps a validated registry atomically.
The seed/runtime registry MUST also carry enough creation metadata to
back the M3 creation seam (§9): new-character eligibility, initial
ability value/rule, the free default spell stable ID, starter
Mace/Coins stable item protos, starter spawn/hometown, and the
blocklist/reserved-name policy. M9 decides the final source-file
schema; T3a stays independent from M9 by interface.

Wire-visible stable IDs are `u16` on the wire but MUST be `INTEGER NOT
NULL CHECK (id BETWEEN 1 AND 65535)` in PG — never `SMALLINT` (signed,
max 32767, allows negatives):

- `spell_protos(id INT PK CHECK 1..65535, school SMALLINT,
  level SMALLINT, mana INT, exertion INT, cast_ms INT, min_hp INT,
  outlaw BOOL, harmful BOOL, reagents JSONB, params JSONB, version INT)`
- `skill_protos(id INT PK CHECK 1..65535, division SMALLINT,
  level SMALLINT, exertion INT, params JSONB, version INT)`
- `item_protos(id INT PK CHECK 1..65535, kind SMALLINT, slot TEXT,
  base JSONB, version INT)`
- `mob_protos(id INT PK CHECK 1..65535, key TEXT UNIQUE (e.g.
  "orc_warrior"), level SMALLINT, difficulty SMALLINT, karma INT,
  atk JSONB, resists JSONB, spells JSONB, loot_tid TEXT, version INT)`
  — numeric IDs for ALL concrete prototypes (plus symbolic key), because
  the client must distinguish them on the wire (see `entityEntry.proto`,
  §6.3). Vendors ARE mob protos (NPC roles): `shop_listings.vendor_id INT
  FK → mob_protos.id, listing INT CHECK 1..65535,
  item_proto INT FK → item_protos, price BIGINT, qty INT,
  PK(vendor_id, listing))`.
- Version semantics (idempotency is exact, not "bump on run"): `version`
  is SOURCE-DEFINED per record. Re-running identical source data is a
  strict no-op (row untouched). A changed record MUST carry a newer
  `version`; seed refuses version rollback unless explicitly forced
  (`--allow-downgrade`, logged, admin-only).
- Shop-listing version ownership: listings are NEVER independently
  versioned or seeded. A vendor's complete `shop_listings` set is part of
  that vendor's `mob_proto` source record and inherits
  `mob_protos.version`. Any listing change for a vendor — add, remove,
  item, price, or qty — requires a newer vendor `mob_proto.version` (or
  explicit forced downgrade). On an accepted vendor version transition,
  its listing set is replaced transactionally as a whole; an empty
  incoming set removes all prior listings. Re-running the same vendor
  version with an identical listing set is a strict no-op; the same
  version with a different set is a version conflict. Listing order in
  source is not semantic; identity is `(vendor_id, listing)`.
  `--allow-downgrade` applies to the vendor proto and its listing set
  together.

`item_instances.proto` references `item_protos.id` (INT, range-checked);
character spell/skill rows reference `spell_protos`/`skill_protos` IDs.

### 8.1 Persistence ordering and recovery (D7)

**PG materialized state is the recovery source of truth. The ledger is an
immutable audit trail, NOT event sourcing** — it cannot and MUST NOT be
used to reconstruct character state. Recovery = load materialized rows;
ledger is never replayed.

- Critical operations (death, trade accept, char create/delete, logout
  flush, +1 HP / +1% milestones, guild/faction phase-2 changes) update
  materialized state transactionally; whenever the operation produces
  ledger/audit rows, those rows commit in the SAME transaction — there
  are no meaningless ledger entries for pure-state ops like logout, and
  a crash between state and audit MUST be impossible by construction,
  not by cleanup.
- Every mutable persisted **aggregate root** carries a monotonic
  `revision BIGINT` (characters, item_instances, banks — NOT child rows
  like `character_spells`/`character_skills`/`item_locations`, which are
  guarded by their root per the aggregate rule below). All snapshot
  writes are compare-and-swap, exactly one form (no alternatives):

  ```sql
  UPDATE characters
  SET ..., revision = $expected + 1
  WHERE id = $id
    AND revision = $expected;
  ```

  Invariant: expected revision matches → this write owns the aggregate;
  anything else → stale/conflicting write, abort the txn, log + count a
  metric. `revision` is the PERSISTED CAS generation: it increments only
  when a PG write commits, NOT on every in-memory sim mutation. The saver
  works off a per-entity dirty queue carrying the exact revision it read;
  it never re-reads-and-blind-writes.
- Restart recovery: load newest materialized rows (any revision), rebuild
  sim, resume timers from stored due-times. Crash/panic is assumed to
  bypass ALL cleanup — survival comes from the txn + revision rules
  above, never from shutdown hooks (though §10 still defines graceful
  shutdown for the clean path).
- Aggregate revisions (normative invariant): per-row revisions are NOT
  enough — `character_spells`, `character_skills`, and `item_locations`
  carry no revision of their own. Instead:

  ```text
  A snapshot may mutate an aggregate's child rows only
  if it successfully advances that aggregate's root revision.
  ```

  `characters.revision` guards character + spells + skills as one atomic
  snapshot txn; `item_instances.revision` guards item + its location row;
  `banks.revision` guards balance rows. Snapshot save = `UPDATE root
  ... WHERE revision = $expected` first; only on success do child rows
  write, all in the same PG txn. On restart, in-memory revision counters
  initialize FROM the persisted revisions (never from zero).

- Normal live character snapshot CAS. The saver owns gameplay state plus
  children, and nothing else:

  ```text
  MAY update: karma, pos_x, pos_y, pos_z, vitals, advancement, flags,
              updated_at, revision (+ character_spells, character_skills)
  NEVER via normal save: id, account_id, slot, name, gender, face,
              might, intellect, stamina, agility, mysticism, aim,
              hometown, created_at, deleted_at
  ```

  Changing an identity/profile field later requires its own explicit
  character-root CAS operation — never a blind setter, never the normal
  save. The normal save requires `deleted_at IS NULL` and performs
  root-CAS → replace complete spell snapshot → replace complete skill
  snapshot → commit in one transaction.

- Character soft-delete CAS (a character-root CAS mutation, not a plain
  write):

  ```text
  UPDATE characters
  SET deleted_at = now(),
      updated_at = now(),
      revision = expected + 1
  WHERE id = ... AND revision = expected AND deleted_at IS NULL
  ```

  It keeps the row and its spell/skill children (uniqueness releases via
  the partial indexes), and cannot win a revision race against a
  concurrent save/delete on the same expected revision.

- Normal item snapshot CAS. The saver may update `qty`, `hits`,
  `enchants`, `revision`, plus the complete `item_locations` row — and
  nothing else (`id`, `proto`, `created_at` are creation identity; a
  future proto change would need its own explicit root-CAS operation). A
  successful save always leaves exactly one persisted location row.
  Order: root CAS → (if container destination) containment serialization
  + ancestry validation → upsert complete location → commit; any failure
  rolls everything back.

- Container-cycle rule: an item's `container_item_id` ancestry MUST be
  acyclic. A placement is rejected if the destination is the moving item
  itself, if destination ancestry reaches the moving item, or if the
  destination ancestry is already cyclic. The SQL self-containment CHECK
  stays as defense-in-depth.

- Containment concurrency rule (MVP): all saves creating/replacing a
  containment edge (`kind = 4`) serialize ancestry-check + location-write
  through ONE transaction-scoped PostgreSQL advisory lock shared by the
  item-containment graph. Order stays root-CAS FIRST, then lock, then
  check, then write — under plain READ COMMITTED, so a waiter sees edges
  committed by the previous holder. Without this, two roots racing
  `A → B` / `B → A` could both pass root CAS (different rows) against
  the same acyclic pre-state. Non-container moves need no lock (they
  remove edges). No triggers, no recursive SQL constraints, no
  distributed locks, no migration.

## 9. Gameplay services (what sim MUST enforce; numbers in `meridian59.md`)

- Creation: `122 character_create` validates slot 0/1 (+ transactional
  uniqueness, §8). Name handling (frozen): NFC-normalize FIRST and
  persist the NFC form; count Unicode CODE POINTS after normalization
  (3–16 inclusive; not grapheme clusters, not bytes); permit only
  Unicode Letter/Mark/Number plus ASCII space (U+0020), apostrophe
  (U+0027), hyphen-minus (U+002D) — reject everything else (no
  trimming, collapsing, lowercasing, accent-stripping, or any silent
  rewrite besides NFC); blocklist/reserved matched EXACT-NAME on the
  NFC + Unicode case-folded key (no substring matching, nothing extra
  persisted); live names stay case-insensitive global-live-unique via
  the CITEXT partial index as the final race authority. Name staging:
  M3 validates against an injected immutable NamePolicy
  (blocked + reserved exact-name sets); M9-T1 loads the production
  blocklist/reserved data and supplies the runtime policy — M3 creates
  no `seed/` content files. Stats order Might/Intellect/Stamina/Agility/
  Mysticism/Aim, each 1–50, sum ≤ 200 (no joke-punishment characters;
  invalid input is rejected with `bad_stats`). Ability budget: the wire
  carries only stable IDs, resolved through injected creation metadata
  (no per-ability PG SELECT in the hot path); each selected spell/skill
  must be offered to new characters at level 1 (10 pts) or 2 (25 pts);
  total ≤ 45; unknown/unoffered/bad-level/duplicate IDs, explicit
  selection of a server-granted free spell, and over-budget totals are
  `bad_budget` (spell and skill IDs are separate namespaces). Trusted
  creation metadata also supplies: free default spell(s) (currently
  Blink, server-added, uncharged), starter Mace + 500-Coins templates,
  hometown, and spawn position — M3 implements the mechanism, M9
  supplies the real content IDs/metadata; no canonical IDs are
  hard-coded in M3. Resolved initial abilities must be 1–99 with
  `atrophy_flag = false`; broken trusted metadata is an internal
  content error, never `bad_budget`. Karma from the final starting
  spell set by school: Qor-only −20, Shalille-only +20, both or
  neither 0 (literal integers, no hidden scaling). Initial vitals JSON:
  `hp/base_max/max = 20`, `mana/max_mana = 15 + Mysticism/5` (integer
  division), `vigor = 100`, `threshold = 80`, `stomach = 0`;
  `advancement = {}`, `flags = 0` (progression machinery belongs to
  later milestones). `face` persists as
  `{"hair_style","hair_color","skin_tone","parts":[5×u8]}` (no cosmetic
  range validation); `gender` persists as the raw wire integer (no
  numeric sex semantics invented here). Creation is ONE PG transaction:
  character root + selected spell/skill rows + free spell + starter
  Mace/Coins instances with inventory locations (starter enchants `{}`
  unless the trusted template says otherwise); any child/item failure
  rolls back the root too. The leaving-newbie-zone package (+1000,
  reagents, apples, uptime bonus) is NOT part of initial creation.
- Vitals/regen: HP=level (20 start, cap `100+Stam`/150); mana `15+Myst/5`
  + nodes; vigor/exertion/rest thresholds; hunger decay; exact M59 formulas,
  constants server-side (`world.toml`/flags, not client).
- Improvement: success `((100-req)*ab/100)+req`; two-gate +1% on use;
  10 pts / 15–22 min window; 75-swing checks; learn-points 16+Int gate at
  trainers; HP-gain highmark roll on kill-with-damage; write-through on gains.
- Combat: offense/defense formulas, weapon type/quality, 30/hit + ⅓-HP caps,
  armor/shield/resist (±100 clip), spell `rand*(50+power/2)/99`, touch/wall/AoE
  rules, vigor/mana/reagent/karma gates, 1 swing/s.
- Mobs: proto table (level/difficulty/karma/atk-type/resists/spells/loot TID)
  seeded from `meridian59.md` §8; spawners bound to authored volumes with
  respawn timers; loot rolls server-side into corpse/world items.
- Trade/bank/vault: offer/counter/accept state machine with both-party
  re-confirm; trades are single PG transactions; lawful-refusal + PK-tag rules
  ported.
- Death: corpse spawn + full droppable drop, advancement wipe/halve,
  Underworld-region respawn (HP 1/Mana 1/Vigor÷4), leaving-penalty
  (cost 100 → newbie /3; −1 HP / −1–2 skills w/ Stam saves); Portal-of-Life
  mitigation hook reserved.
- Phase 2 (reserved, not MVP): guilds, factions/territory, justice, assassin
  game, Jala hinder matrix — schema MUST NOT preclude them (flags/ledger/kills
  already carry what they need).

## 10. Config / deployment / ops

- Config: env + file (`config.yaml` default, env override `VOX_*`); MUST
  include: PG DSN, WS bind, world constants path, tick rates,
  snapshot interval, rate limits, seed data paths, log level.
- `compose.yaml` (dev AND prod — single VPS per DECISION §13.6):
  - dev profile: `postgres:18-alpine` + `voxilian` local build target; named
    volume; healthcheck; `voxilian migrate up` as init step.
  - prod profile: `voxilian` image from GHCR (`ghcr.io/dlukt/voxilian`;
    published by CI) + connection to the **existing prod PG instance**
    (dedicated database + owner user, DSN via env `VOX_PG_DSN`); no PG
    container of its own. `voxilian migrate up` runs as a one-shot init
    container against that database.
- Observability: `/healthz` = process/sim liveness ONLY (never PG-gated);
  `/readyz` = world loaded + PG reachable + migrations compatible;
  `/metrics` (ticks, AOI fanout, intent rates/errors, saver lag, WS
  sessions, queue saturation, stale-snapshot writes);
  structured slog with `tick`, `cell`, `charID` fields.
- PG-outage behavior: outage flips `/readyz` to unready (NEVER `/healthz`
  — no supervisor restart loops). While unready: reject new logins and
  all critical persistence ops (trades, char create/delete, purchases);
  already-connected clients keep limited movement for a 60 s grace
  period, then are held (no state progression) until PG returns or the
  operator drains. Every gameplay service follows this policy — no local
  exceptions.
- Graceful shutdown (replaces best-effort panic cleanup): `SIGTERM` →
  `ready=false` → stop accepting sessions → stop new critical
  transactions → quiesce sim → flush dirty entities with deadline → close
  connections → exit. A real crash/panic is assumed to bypass ALL of this
  — which is exactly why §8.1 (txn + revision) must survive it.
- Admin (cobra `voxilian admin ...` + WS admin role): create account/character,
  grant/revoke, kick/ban, save-now, spawn/teleport (logged), give (logged,
  dev-only flag).
- Seed data (`voxilian seed`, DECISION §13.7): all spell/skill/mob/item
  protos load from versioned structured data files — one per school plus
  bestiary (e.g. `seed/shalille.yaml`, `seed/bestiary.yaml`) — upserted
  into the §8.2 catalog tables by stable `u16` ID, so the full-scope port
  is reviewable in chunks. Seed files are validated against the same
  CHECK constraints as live writes; re-running seed is idempotent.

## 11. Security

- Auth (D6): no local credentials — no password hashes, no reset/mails in the
  backend. Keycloak public client, Authorization Code + PKCE (S256), loopback
  redirect; scopes `openid profile email` (+ `offline_access` for refresh
  tokens surviving restarts). No client secret in the game binary; direct
  grants forbidden. Backend validates access JWTs against cached JWKS
  (`iss`/`aud`/expiry/signature; key rotation via cache TTL + backoff);
  rejects expired/misissued tokens with `202 error{session_expired}`.
  Staged delivery: M3-T2 validates against an immutable startup JWKS
  (§6.2.1); M11-T2 adds the cache/rotation/backoff layer.
- Hard re-auth deadline: token expiry does not drop the session, but a
  **90 s grace period** starts at expiry. After grace: new gameplay
  intents are rejected, then the session is disconnected. A connected
  session MUST NOT persist indefinitely without fresh authorization.
- OIDC login MUST require `state` AND `nonce` (not just PKCE), validated
  at the correct stages: `state` at the authorization callback BEFORE
  code exchange; `nonce` AFTER exchange, against the returned ID token
  as part of full client-side ID-token validation (signature via JWKS,
  `iss`/`aud`/expiry/`nonce`). The Godot client performs this validation
  itself — it MUST NOT treat a successful code exchange as proof of
  identity without checking the ID token.
- Refresh tokens (`offline_access`) MUST use platform-secure credential
  storage where available (OS keychain / credential manager) and MUST
  NOT be written plaintext to `user://` or equivalent.
- Pre-auth rate limiting: the `hello`/JWKS-validation path is rate-limited
  per connection/IP (token bucket), independent of per-character gameplay
  limits — unauthenticated JWT verification MUST NOT be a free
  CPU-amplification endpoint. Staged: M3-T2 proves authentication
  correctness; M11-T2 adds this abuse-resistance layer.
- WS admin role is authorized by a Keycloak client role claim
  (`vox-admin`), never by mere authentication.
- WS requires TLS in prod (terminate at proxy or Go — DECISION §13.6).
- Authoritative sim (§5 anti-cheat); per-intent in-memory rate limits;
  movement speed/teleport anomaly detection → correct + log, ban on repeat.
- No secrets in repo; `.env` local only; prod PG uses a dedicated database +
  owner user on the existing instance. Runtime app role is least-privilege
  (DML on game tables only); migrations run as owner (or a dedicated migrate
  role) via the one-shot migrate container.

## 12. Testing

- Unit: formulas (hit/damage/regen/advancement/learn-points) with golden
  vectors from `meridian59.md`; property tests for caps/clips.
- Integration (testcontainers PG 18): creation, trade atomicity,
  death/corpse/respawn, snapshot restore, double-accept race.
- Load: bot harness (N clients random-walk + attack) measuring tick p99,
  AOI fanout bytes, saver lag — gates sharding decision with data.
- Protocol robustness (hand-written codec): Go fuzz tests for every
  decoder; malformed/truncated/oversized packet tests; encode/decode
  round-trips; Go ↔ Godot golden binary fixtures (checked-in
  hex vectors both sides decode identically).
- Resilience: slow-client/backpressure tests; crash injection during
  trade/death/snapshot (assert §8.1 invariants hold); PG-loss/recovery
  tests; cell-boundary handoff races; reconnect/full-resync tests;
  stale/duplicate intent tests.

## 13. Open questions (please decide together)

1. **Chunk/cell constants**: DECIDED — voxel chunk **16³**, sim cell
   **32 m**, AOI radius **96 m** default (tunable to 128 m on min-spec
   evidence); 1 M59 square ≈ 1 m for range ports. Low-end clients targeted:
   conservative streaming/AOI budgets, validated on min-spec hardware.
   Still open: portal volume format (needs Godot-side agreement).
2. **Movement authority**: DECIDED — **server-authoritative** (M59 was
   client-authoritative with log-only enforcement; we do the opposite).
   `102 move {inputSeq, heldDirs, runFlag, yaw}` ≤ 10 Hz, never positions;
   header tick = sampling tick; server integrates at 20 Hz and echoes
   `lastProcessedInputSeq` in `205`; client rewinds+reapplies on error >
   0.5 m (§6.3). Speeds: walk ~3.5 m/s, run ~7 m/s with M59 vigor gate
   (run needs vigor ≥ 10). Melee validated by server-side range check at
   processing time. **Lag compensation: skipped for MVP but designed
   for** — sim keeps a per-entity position-history ring (2 s @ 20 Hz) from
   day one and hit validation lives in one isolated function, so rewind
   plugs in later without protocol changes.
3. **Characters/account limits**: DECIDED — **2/account** (slots 0/1,
   partial unique index — transactional, §8) **and one `IN_WORLD` session
   per account** (second `enter_world` kicks the old world session, §6.1:
   no same-account multiboxing/self-trade by construction — reviewed and
   locked). Deleted names reusable via partial unique index.
   Display names DECIDED (§9 creation rule: charset/NFC/3–16/blocklist).
4. **Auth**: DECIDED — **external Keycloak IdP, Authorization Code + PKCE
   via system browser** (no Godot OIDC package needed: `OS.shell_open` +
   `TCPServer` loopback callback + `HTTPRequest` exchange + `HashingContext`
   S256; direct grants forbidden as insecure). Registration/reset/mails stay
   in Keycloak. Access JWT ~5 min + `reauth` over live WS; refresh via
   `offline_access`. Still open: Keycloak realm/client names, access-token
   TTL, age/rating handling.
5. **World authoring**: DECIDED — **two modes**: `classic` (hand-authored,
   M59-faithful regions for returning players; more work, ships incrementally
   starting with the starter region) and `procedural` (deterministic
   seeded generation). Mode + seed are server config (`world.mode`,
   `world.seed`); sim runs against a `WorldSource` interface so both feed the
   same cells/volumes/flags pipeline. Layouts are M59-*inspired*, not copied
   (trademark/content note in `meridian59.md` header). Still open: authored
   format details (Tiled/custom → `world.toml`?); generator algorithm +
   versioning; whether classic regions can embed inside procedural worlds.
6. **Prod target**: DECIDED — **single VPS + compose**; app image from
   **GHCR** (`ghcr.io/dlukt/voxilian`, CI-published); PG is the **existing
   prod instance** (dedicated database + owner user, external to compose).
   Still open: TLS termination (reverse proxy vs Go); backups/PITR for PG
   (may ride on the existing instance's policy — confirm).
7. **MVP content scope**: DECIDED — **all spell schools, all skills, all
   mobs** (full M59 data port per `meridian59.md` §§5/7/8, not the cut-down
   slice). Consequence: seed data must cover every school/skill/mob proto +
   costs/effects/loot tables up front; phasing applies to meta-systems only
   (guilds/factions/justice stay phase 2).
8. **Protocol encoding**: DECIDED — **WebSocket + hand-packed binary,
   M59-style** (§6: opcode envelope, fixed-point positions, stdlib-only
   codecs both sides; JSON for admin/debug only). gRPC explicitly rejected
   for the game plane (wrong model for AOI fanout + immature native dep on
   low-end clients); protobuf is the documented escape hatch, envelope
   survives it.

## 14. Version history

- v0.3.12: freeze M3 outbound backpressure semantics — exact per-session
  queue budgets, critical/state lane behavior, physical-write-preserving
  SendFunc, non-blocking future-sim producers, terminal-control bypasses,
  post-world_ready cumulative ACK window, metric names, and split T5 into
  queue core T5a plus ACK/observability T5b.
- v0.3.11: freeze enter-world baseline semantics — atomic selected/in-world
  registry transitions, provisional-baseline rollback, exact world_ready
  barrier, full account-guard serialization, deterministic account-world
  discovery, and split M3-T4 into baseline T4a plus takeover/transport T4b.
- v0.3.10: freeze character CRUD semantics — character_op numeric values,
  NFC/name-policy handling, creation-content staging, transactional starter
  state, character-list level source, safe delete semantics, and split the
  oversized M3-T3 into persistence/domain T3a plus WS T3b; opcode 124 is
  owned wholly by T4.
- v0.3.9: freeze M3-T2 auth staging — jwx v4 baseline, immutable
  startup JWKS for M3 with rotation/cache hardening deferred to M11,
  race-safe account auto-provisioning, and exact token-expiry + 90 s
  reauth deadline semantics.
- v0.3.8: freeze M3 gateway wire semantics — runtime msg_version starts
  at 1, stable 202 error-code registry, coder/websocket v1.8.15 pin, and
  bounded WebSocket oversize handling via transport status 1009.
- v0.3.7: freeze M2-T3c wire layouts — complete inventory_delta
  entry v1, explicit inventory removal via 206 handle invalidation, and
  length-prefix chunk_fragment bytes so msg_version trailing extensions
  remain possible.
- v0.3.6: leftover consistency — INT proto FKs in §8 summary (incl.
  numeric `kills` mob FKs, `vendor_id → mob_protos`), display-name
  pointer fixed, plan header sync.
- v0.3.5: catalog coherence — migration-safe order note (catalogs before
  dependents), INTEGER 1..65535 stable IDs (never SMALLINT), numeric
  mob/vendor IDs + `entityEntry.proto`, exact seed version semantics,
  in-memory catalog registry rule.
- v0.3.4: planning blocker fix — §8.2 prototype catalog tables (seed
  upsert target; SMALLINT proto FKs), display-name rules decided (§9,
  §13.3 closed).
- v0.3.3: spec-cleanup freeze — real SQL CHECK syntax (`IS [NOT] NULL`,
  `num_nonnulls()`), account lifecycle guard + takeover ordering (§6.1),
  cell-owner non-blocking rule (§7.1), ledger-commit wording + revision-
  as-persisted-generation (§8.1). Architecture phase closed after this.
- v0.3.2: correctness pass — chunks to reliable lane + `world_ready`
  barrier (§7.1, §6.1), CAS-only aggregate revisions (§8.1, D7), XYZ +
  self-containment CHECKs (§8), post-commit reload rule + bounded opID
  dedupe (§5.1), `inputSeq` arithmetic + `205` zero rule + fixed-`u32`
  `105 use` (§6.3), serialized `enter_world` arbitration (§6.1).
- v0.3.1: correctness pass — reconciliation via `inputSeq`/`yaw`/
  `lastProcessedInputSeq` (§6.3, §13.2), aggregate revisions (§8.1, D7),
  cross-cell `DamageIntent`/`opID`/idempotency (§5.1), item-location
  per-kind CHECKs + container/vault fields, entry-length framing + 8 KiB
  JWT bound + chunk opcodes (§6), per-opcode lifecycle permissions +
  `leave_world` + `world_ready` boundary (§6.1), NetID no-reuse +
  namespaces, OIDC state/nonce staging + refresh storage (§11),
  Ping/Pong heartbeat + queue lanes (§7), one-`IN_WORLD`-per-account lock
  (§6.1, §13.3), partial-index DDL wording.
- v0.3: review hardening — session lifecycle + char CRUD (§6.1), exact wire
  layouts/ACK/resync/NetEntityID (§6), persistence ordering + revision guard
  (D7, §8.1), cell handoff invariants (§5.1), backpressure (§7.1), schema
  tightening (positions, item locations, slots, names, owners), auth
  hardening (§11), ops split + PG-outage + shutdown (§10).
- v0.2: drop Redis; sessions/presence/rate-limits in memory; PG-only stack.
- v0.1: initial spec (seamless + embedded interiors, WebSocket, single
  process, snapshot + write-through, PG 18 + Redis 8).
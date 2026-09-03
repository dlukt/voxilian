# Voxilian Backend SPEC (v0.3.6 — documentation only, no implementation)

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
| WS | `github.com/coder/websocket` (current) | Pick over archived gorilla; single maintained dep |
| OIDC/JWT | TBD at implementation (`lestrrat-go/jwx` v3 vs `golang-jwt/jwt` + JWKS fetch) | Access-token validation against Keycloak JWKS (D6) |
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
12-bit convention). Max frame 64 KiB — larger frames are rejected with
`202 error`, never parsed. Codecs use stdlib only — Go
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
  [[u16 entryLen]{slot u8, charName string, level u16}]...}`.
- `122 character_create {slot u8 (0/1), name string, gender u8,
  face {hairStyle u8, hairColor u8, skinTone u8, parts u8[5]},
  stats u8[6], spells {count u16 + u16[..]}, skills {count u16 +
  u16[..]}}` → `217 character_op {op u8, ok u8}` (failures also via
  `202 error` with codes: `name_taken`, `slot_occupied`, `bad_stats`,
  `bad_budget`). Slot + stats + budget validated server-side per §9; row
  created in the same PG txn (§8.1).
- `123 character_delete {slot u8}` → `217 character_op` (soft-delete;
  name becomes reusable, §8).
- `124 enter_world {slot u8}` → loads character, binds session to it
  (`AUTHENTICATED → CHARACTER_SELECTED`), then streams the baseline:
  `217 character_op{enter,ok}`, then `203 cell_snapshot` (+ `218
  chunk_fragment`s for classic mode, `220 shop_list`s for nearby
  vendors), then `219 world_ready {}` — and ONLY at `world_ready` does
  the session enter `IN_WORLD`. One AOI spans multiple cells, so the
  first snapshot is NOT the boundary; `world_ready` is.
- `126 leave_world {}` → unbinds the character (AOI cleared, presence
  dropped, dirty state flushed), session back to `AUTHENTICATED`. This
  is how characters are switched WITHOUT reconnecting.
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
  disconnect.

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
  target u32, pos}`,   `211 inventory_delta {count u16 + [[u16
  entryLen]{item u32 NetEntityID, proto u16 stable, qty u16, ...}]...}`
  (full entry layout frozen at implementation per §13 entry-schema task;
  entry-framed so the protocol is unaffected),
  `212 offer_update {with u32, state u8, count u16 + [[u16 entryLen]{item
  u32, qty u16}]...}`, `213 trade_result {ok u8}`, `214 death {victim
  u32}`, `215 respawn {pos}`, `218 chunk_fragment {cell, chunkIdx u32,
  fragIdx u16, fragCount u16, bytes (≤60 KiB so the frame stays under
  64 KiB)}` (classic mode voxel streaming; server paces ≤N
  fragments/tick/session, `N` in config; procedural mode never sends
  these), `219 world_ready {}` (baseline boundary, §6.1), `220 shop_list
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

### 7.1 Slow-client and backpressure rules

- Every session has a bounded outbound queue (e.g. 256 KiB / 1024
  messages; exact budgets in `config.yaml`, validated on min-spec).
- Movement/state deltas MAY be coalesced or dropped in favor of newest
  state. Critical ordered events (`trade_result`, `death`, `stat`
  milestones, `offer_update`, `enter_world` flow) MUST NOT be silently
  dropped — they queue behind the same bound.
- If a client cannot drain within budget, the server disconnects it;
  reconnect performs a full resync (§6.1). Saturation is a first-class
  metric (`vox_session_drops_total`, queue-depth histograms).
- Lanes: the outbound path is TWO lanes — a coalescible-state lane
  (`entity_move` and other absolute/latest-state deltas: newest wins) and
  a control/critical lane (everything else, ordered, never dropped).
  `218 chunk_fragment` is in the RELIABLE lane, never coalescible: a
  fragment is not replaceable state, dropping one corrupts the chunk with
  no replay protocol. Pace PRODUCTION instead — if the reliable queue
  cannot accept another fragment, stop producing chunk data for that
  session until it drains; past the configured limit/deadline, disconnect
  and let reconnect/full-resync recover. `219 world_ready` is a barrier:
  all baseline snapshots and required classic-mode fragments MUST have
  been written to the socket before `world_ready`. (WS order + reliability
  then needs no fragment ACK/retransmission for MVP.) If the critical
  lane itself fills, the affected sessions are disconnected rather than
  stalling the sim — blocking a cell worker on a slow client is a spec
  violation.
- Same principle internally: sim→gateway channels are bounded with
  defined overload behavior (shed newest movement first, never critical
  events; count + log). Cell-owner goroutines MUST NOT block indefinitely
  on gateway delivery; critical-lane saturation fails closed by
  disconnecting/resyncing the affected sessions rather than dropping the
  event or stalling the simulation. Unbounded queues are a spec
  violation.

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

## 9. Gameplay services (what sim MUST enforce; numbers in `meridian59.md`)

- Creation: `122 character_create` validates slot 0/1 (+ transactional
  uniqueness, §8), name — Unicode letters/marks/numbers plus space,
  apostrophe, hyphen; NFC-normalized FIRST, then length counted as 3–16
  Unicode CODE POINTS (not grapheme clusters, not bytes);
  case-insensitive global-live-unique (CITEXT partial index); blocklist
  (`seed/blocklist.yaml`) matched EXACT-NAME after normalization +
  case-folding (no substring matching); reserved names likewise — 6×(1–50) + sum ≤ 200, 45-pt
  ability budget (L2=25 else 10); grant Blink + Mace + 500
  (+leaving-newbie-zone package); karma seed. All in one PG txn (§8.1).
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
  CPU-amplification endpoint.
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
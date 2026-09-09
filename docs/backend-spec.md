# Voxilian Backend SPEC (v0.3.36 — documentation only, no implementation)

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

### 5.2 Deterministic sim skeleton (frozen, v0.3.14)

This section freezes the M4-T1 sim skeleton only: fixed-step tick loop,
XZ cell grid, in-process entity registry, injectable clock/RNG, and the
post-tick position-history ring. It adds no movement, handoff, AOI,
saver, collision, or world-content behavior.

#### 5.2.1 Fixed-step tick semantics

- `config.tick_hz`: default `20`, valid range `1..120` (existing config
  validation is unchanged), MVP production target `20 Hz`.
- The engine uses the configured tick rate as a FIXED simulation step:

```text
dtSeconds = 1.0 / tickHz
```

- Future movement/combat systems express rates per second and scale by
  this fixed `dt`. Physics `dt` is NEVER derived from actual
  wall-clock elapsed time.
- At the default 20 Hz the tick period is `50 ms`.
- The simulation is fixed-step. Production wall-clock scheduling
  decides WHEN a step runs; it does NOT decide the amount of simulated
  time represented by a step.
- One delivered ticker pulse executes exactly one simulation step. M4
  deliberately does NOT execute burst catch-up ticks based on elapsed
  wall time. If the process is overloaded and ticker delivery is
  delayed/coalesced, the simulation falls behind wall time rather than
  entering an unbounded catch-up spiral. No adaptive timestep behavior.

#### 5.2.2 Tick numbering

- Initial tick is `0`; the first executed simulation step is tick `1`.
- Each executed step increments the tick exactly once.
- Tick type is `uint32`. Wraparound is normal (`MaxUint32 -> 0`) using
  the same conceptual modulo-2³² serial domain as the protocol header
  tick. There is no "tick 0 means invalid" rule.

#### 5.2.3 Clock seam and Run semantics

- The engine depends on a minimal injectable ticker/clock seam,
  conceptually equivalent to:

```go
type Ticker interface {
    C() <-chan time.Time
    Stop()
}

type Clock interface {
    NewTicker(time.Duration) Ticker
}
```

- Exact Go names may differ. Production wraps `time.NewTicker`;
  tests use a manual ticker. No sleeps are required for tick-loop
  correctness tests.
- The engine also exposes (or internally provides) a synchronous
  fixed-step operation usable by unit tests without wall time,
  conceptually `Step()`. `Run` MUST call the SAME step core; there are
  no separate test and production simulation paths.
- `Run(ctx)` creates exactly one ticker, waits for pulses, executes
  one `Step` per pulse, stops the ticker on exit, and returns when
  `ctx` is cancelled. No ticker/goroutine leak. Cancellation does NOT
  execute an extra final tick. Graceful persistence/shutdown belongs
  to M4-T4, not here.

#### 5.2.4 Server geometry (`internal/world`)

- Server-side world vector (authoritative positions remain float64
  meters):

```go
type Vec3 struct {
    X float64
    Y float64
    Z float64
}
```

- Server code MUST NOT reuse protocol `Position` (fixed-point
  millimeters) for simulation: `proto.Position` is wire millimeters,
  `world.Vec3` is simulation meters. Protocol conversion belongs to
  later gateway/sim integration.
- Canonical server geometry constants owned by `internal/world`:

```text
ChunkEdgeVoxels        = 16
CellSizeMeters         = 32
DefaultAOIRadiusMeters = 96
```

- M4-T1 establishes the server canonical geometry API only. It does
  NOT create or parse `world.toml`, does NOT modify the Godot client,
  and the existing `WorldConstantsPath` remains staged for later
  world/content loading.
- Cell coordinates are 2D XZ:

```go
type CellCoord struct {
    X int32
    Z int32
}
```

- Y does NOT select the cell (matching the wire cell shape `i32 cx +
  i32 cz`). Embedded interiors may use Y/volume semantics later
  without changing the base XZ grid.
- Position-to-cell conversion uses mathematical floor, NOT Go integer
  truncation:

```text
cx = floor(position.X / 32)
cz = floor(position.Z / 32)
```

- Required boundaries (same for Z):

```text
X =   0.0      -> cell  0
X =  31.999    -> cell  0
X =  32.0      -> cell  1

X =  -0.001    -> cell -1
X = -32.0      -> cell -1
X = -32.001    -> cell -2
```

- Invalid simulation positions: any component `NaN`, `+Inf`, or
  `-Inf` is rejected; X/Z whose floored cell result cannot fit `int32`
  is rejected; Y must also be finite even though it does not select
  the cell. Rejection uses a stable world-domain error — never panic
  and never silent clamp.
- Canonical cell iteration is `X` ascending, then `Z` ascending. Any
  engine operation whose result can affect simulation state MUST NEVER
  depend on Go map iteration order; this ordering is part of the
  determinism contract.

#### 5.2.5 Entities and registry (M4-T1 skeleton)

- Internal sim entity identity is an opaque domain:

```go
type EntityID uint64
```

- Rules: `0` is invalid/reserved; the first allocated ID is `1`;
  allocation is monotonic and never reused during one engine lifetime
  (natural `uint64` space). This ID is NOT a PostgreSQL durable ID,
  NOT a session-local NetEntityID, and NOT sent on the wire. Later
  systems may associate durable/session identities with it without
  overloading those domains.
- The M4-T1 base entity contains only skeleton state: internal
  `EntityID`, authoritative `Vec3` position, current `CellCoord`, and
  position history. It MUST NOT add HP/mana/vigor/stats, combat state,
  character DB IDs, mob protos, inventory, velocity, or movement
  input before their owning tasks.
- Each active sim cell owns its entity collection, conceptually
  `CellCoord -> Cell -> EntityID -> mutable entity`. A global locator
  maps `EntityID -> CellCoord` for lookup. No global mutable entity
  pointer is handed to callers; inspection APIs return immutable
  snapshots/copies containing at least `EntityID`, `Position`, and
  `CellCoord`. Cell entity listings are deterministic in `EntityID`
  ascending order.
- Single-writer rule for M4-T1: ONE engine simulation loop owns
  mutation of ALL cells, so each cell has exactly one writer. M4-T1
  creates no per-cell goroutine, per-entity/per-cell mutex, worker
  pool, actor framework, or external bus. The layout keeps cell
  mutation encapsulated so M4-T3 can later split ownership/handoff
  without rewriting gameplay state.
- No cross-cell handoff in T1 (M4-T3a owns it): a position-mutation
  operation whose new position belongs to another cell MUST return a
  stable "handoff required" / cell-boundary error and leave the entity
  unchanged, never silently move it between cell maps.
- Add is all-or-nothing: validate position, compute cell, allocate a
  fresh `EntityID`, lazily create the active cell if needed, insert
  under that cell, update the global locator. An invalid position
  allocates NO ID and mutates nothing.
- Remove deletes the entity from its owning cell, deletes the global
  locator entry, and discards its history. Unknown IDs return a
  stable not-found error. Empty active cells are removed (M4-T1 cells
  represent active sim ownership, not terrain storage; world terrain
  cells arrive later from `WorldSource`).

#### 5.2.6 Position history

- History duration is `2` simulated seconds, so capacity is
  `2 * tickHz` samples (`40` samples at the default 20 Hz; changing
  dev/test `tickHz` preserves the 2-second simulated horizon).
- Sample type (simulation-time data, no wall-clock timestamp):

```go
type PositionSample struct {
    Tick     uint32
    Position world.Vec3
}
```

- Sampling phase: at the END of every successfully executed simulation
  tick, exactly one sample is appended for every live entity,
  representing that tick's final authoritative position. An entity
  added before tick 1 has an initially empty history; after tick 1 it
  holds one sample at tick 1. Future gameplay phases insert BEFORE
  history sampling; gameplay MUST NOT run after the same tick's
  historical sample.
- The ring has fixed capacity; the oldest entry is overwritten when
  full; steady state performs no allocations; inspection returns
  oldest -> newest copies. At 20 Hz after 45 ticks an entity present
  throughout holds 40 samples for ticks `6..45`. Wrapped tick numbers
  are NOT sorted numerically — ring order itself is chronological.
- Removing the entity removes its history. A recreated entity gets a
  new `EntityID` and a new empty history (no ABA via ID reuse).

#### 5.2.7 RNG seam

- Sim receives an injected RNG dependency over a deliberately narrow
  interface, conceptually:

```go
type RNG interface {
    Uint64() uint64
}
```

- M4-T1 needs no random gameplay behavior; future tasks expand/adapt
  internal random helpers only when they require bounded
  integer/floating-point rolls. No custom PRNG algorithm is
  introduced; tests may use deterministic stdlib `math/rand/v2`;
  production seeding/wiring belongs to executable/bootstrap work.
- Package-global randomness is forbidden in sim production code
  (`global rand` calls, time-based seeding, hidden `crypto/rand`
  inside gameplay). All gameplay randomness MUST eventually descend
  from the injected engine RNG.

#### 5.2.8 Deterministic step ordering and contract

- Each tick executes skeleton bookkeeping in this order:

```text
1. allocate/increment tick
2. process active cells in canonical CellCoord order
3. within a cell inspect/process entities in EntityID ascending order
4. append each live entity's post-step position-history sample
```

- T1 has no gameplay systems, so steps 2/3 perform only skeleton
  bookkeeping.
- Determinism contract: given the same initial world, the same
  sequence of submitted mutations, the same tick pulses, the same RNG
  seed, and the same config, the observable trace MUST be
  byte/logically identical independent of Go map iteration. The test
  trace includes tick, ordered cells, ordered entity snapshots,
  ordered history tails, and a deterministic RNG sample sequence when
  the test draws through the injected RNG.

#### 5.2.9 Explicit non-scope for M4-T1

- No gateway integration: no `102 move`, no `205 entity_move`, no
  gateway `TickFunc`, no session lifecycle, no AOI outbound producer.
  M3 gateway remains untouched.
- No `Store` dependency: sim/world production code imports stdlib
  plus `internal/world` as needed — no `store`, `pgx`, `sqlc/gen`,
  `gateway`, `session`, or `proto`. Architecture direction remains
  `gateway -> sim -> Store interface`; T1 needs no `Store` interface.
- No `WorldSource`: no classic loader, procedural generator,
  `world.toml` parser, volumes, portals, collision, or terrain. M10
  owns world content; M4-T2 owns the minimal `CollisionWorld` seam.
- No M4-T2 behavior: no movement integration, walk/run speeds,
  `inputSeq`, yaw, velocity, collision, `205 entity_move`,
  reconciliation, or speed anomaly detection.
- No M4-T3 behavior: no ownership generations, handoff queues,
  migration, cross-cell `opID`, dedupe, or retry routing.

#### 5.2.10 Runtime command ingress (frozen, v0.3.22)

Once `Engine.Run(ctx)` is active, mutable Engine state may be changed
ONLY by the Run goroutine itself — including movement submission,
entity add, and entity remove. External goroutines (gateway handlers)
never call the direct mutation methods concurrently with Run. The
existing `AddEntity` / `RemoveEntity` / `SubmitMove` / `Step`
signatures do NOT change: they remain owner-local synchronous
primitives, documented as safe only when the caller owns simulation
execution. The concurrent gateway-facing APIs below are separate.

Ingress mailbox: exactly one bounded channel per Engine, capacity
`SimIngressCapacity = 256` commands, constructed in `NewEngine` (no
lazy nil-channel path). No per-entity or per-command-type capacity.
The mailbox carries exactly three typed command kinds — add, remove,
move. Gateway MUST NOT gain a generic `func(*Engine)` callback
command: arbitrary sim mutations can never be injected. Future M5+
typed gameplay commands may extend the internal command union.

Conceptual concurrent API (exact Go names may differ):

```go
EnqueueAddEntity(ctx context.Context, pos world.Vec3) (EntitySnapshot, error)
EnqueueRemoveEntity(ctx context.Context, id EntityID) error
EnqueueMove(ctx context.Context, id EntityID, intent MoveIntent) (MoveDisposition, error)
```

`CurrentTick() uint32` remains direct concurrent observation and is
NOT routed through the mailbox.

Stable `errors.Is` errors (separate conditions, no string parsing):
`ErrSimIngressFull` (mailbox full at admission),
`ErrEngineNotRunning` (no Run owns the engine),
`ErrEngineAlreadyRunning` (second concurrent Run),
`ErrEngineStopped` (Run exited with the command still queued).

Run-state semantics: before Run owns the engine, `Enqueue*` returns
`ErrEngineNotRunning` with zero mutation. While one Run is active, a
second concurrent Run returns `ErrEngineAlreadyRunning` (no second
ticker owner). After Run exits, `Enqueue*` returns
`ErrEngineNotRunning`. A later sequential Run MAY start after the
previous one fully exited; no queued command survives into the new
generation.

Admission is immediate and bounded: a free slot publishes exactly one
command; a full mailbox returns `ErrSimIngressFull` immediately with
no command published, no sim mutation, and no `inputSeq` consumption.
If `ctx.Err() != nil` before publication, the call returns the
context error and publishes/mutates nothing — a cancelled caller is
never randomly admitted; the cancellation check before publication is
explicit and deterministic. Once admitted, the command is
authoritative: later caller cancellation does NOT retract it, and the
caller waits for that exact command's definitive result (mirroring
the outbound critical principle: abandon before publication,
complete-or-teardown after).

Each command owns exactly one buffered result channel of capacity 1
(or an equivalent bounded completion primitive): no goroutine per
command, no shared global waiter map, and the sim owner never blocks
because a caller stopped reading. When Run exits it marks the engine
non-running and fails every queued-but-not-executed command with
`ErrEngineStopped` — no waiter stays parked. Already-executing
owner-local work completes normally before Run exits that iteration.

Tick priority: ingress MUST NOT starve fixed-step ticks. Before
blocking for another command, Run checks whether a ticker pulse is
already ready; a ready tick wins before another queued command (e.g.
`select` ticker-ready first, then `select` ctx/ticker/command). When
a tick is already ready, the loop cannot drain an arbitrary backlog
of commands first. No command-burst catch-up replaces ticks. Run
executes one command to completion on the sim owner goroutine, then
returns to scheduling — no internal command worker, no parallel
execution.

Execution delegates to the SAME existing semantics: move commands
call `SubmitMove`, add commands call `AddEntity`, remove commands
call `RemoveEntity`. No duplicate movement/registry implementation
lives in ingress code. `EnqueueMove` only updates pending control;
positions still change only during `Step`. T5b1 adds no Prometheus
metric for command depth.

Only command admission/run-state coordination may lock across
goroutines (a short ingress/run-state mutex is allowed). That lock
MUST NOT become a broad mutex around `Step`/`SubmitMove`/entity
state: mutable sim remains single-owner.

### 5.3 Authoritative movement semantics (frozen, v0.3.15)

M4-T2 owns `102` movement SEMANTICS: movement intent ordering,
fixed-step integration, walk/run selection, yaw/held-direction
interpretation, collision resolution, reconciliation anchor generation,
the movement anomaly tripwire, and 205-COMPATIBLE sim movement output.

M4-T2 does NOT yet own real gateway → Engine wiring, character DB ID
→ EntityID binding, session-local NetEntityID allocation, `205`
binary transmission, AOI subscription/fanout, movement outbound
throttling, per-character inbound rate limiting, or presence. Those
runtime gateway-facing concerns belong to M4-T5b1 (owner-mailbox
ingress, 102 routing, rate enforcement, staged lifecycle) and M4-T5b2
(AOI fanout, heartbeat runtime): M4-T2 owns authoritative move
semantics; M4-T5b1 wires decoded gateway intents into sim; M4-T5b2
fans `205` updates to session-local NetEntityIDs/AOI.

#### 5.3.1 Wire independence and the neutral serial32 home

- `internal/sim` MUST NOT use `proto.Move`, `proto.EntityMove`, or
  `proto.Position` as runtime domain types. The gateway layer later
  translates `proto.Move → sim.MoveIntent` and
  `sim.MovementUpdate → proto.EntityMove`. This preserves
  `sim EntityID != session NetEntityID` and
  `world.Vec3 meters != proto.Position millimeters`.
- M4 needs the exact already-frozen modulo-2³² serial ordering
  (RFC 1982 style) without sim importing the wire protocol package
  merely to compare serials. The ONE canonical implementation lives
  in the neutral package `internal/serial32`:

```go
After(a, b uint32) bool
Before(a, b uint32) bool
```

  with `After(a,b)` true iff `(a - b) mod 2^32` is nonzero and
  `< 2^31`, and `Before(a,b) = After(b,a)`. Equality is false in
  both directions; the exact half-range (`2^31` apart) is false in
  both directions. `proto.Serial32After` / `proto.Serial32Before`
  remain source-compatible thin delegates to `serial32` with
  unchanged names and behavior. Sim MUST NOT implement a rival
  comparator. No protocol behavior changes.

#### 5.3.2 Held directions and yaw

- `heldDirs` lower four bits (matching the `102` bitmask):

```text
bit 0 = Forward
bit 1 = Backward
bit 2 = Left / strafe-left
bit 3 = Right / strafe-right
```

- Bits `4..7` are reserved for additive future use. M4-T2 ignores
  those reserved bits for movement semantics; it does NOT reject the
  whole intent merely because a reserved high bit is set.
- Opposing directions cancel: Forward + Backward cancels the
  longitudinal component; Left + Right cancels the lateral
  component. `Forward|Backward` yields no forward/back motion,
  `Left|Right` yields no lateral motion, all four yield a zero
  movement vector. Yaw still updates.
- Diagonal normalization: when exactly one longitudinal and one
  lateral component remain, the local XZ vector is normalized to
  length 1, so `forward+right` does NOT move at `sqrt(2) × speed`.
  Maximum horizontal speed is invariant in every direction.
- Yaw uses the 12-bit domain `0..4095` (`4096` units = one full
  turn) with the Godot-friendly XZ convention:

```text
yaw 0       -> forward -Z
yaw 1024    -> forward +X
yaw 2048    -> forward +Z
yaw 3072    -> forward -X
```

- Positive yaw turns clockwise when viewed from +Y. Mathematically:

```text
theta = yaw * 2π / 4096

forward = { X = sin(theta), Z = -cos(theta) }
right   = { X = cos(theta), Z = sin(theta)  }
```

- Y never changes from walking/running.
- Every newly processed movement input updates authoritative yaw
  even when `heldDirs` resolves to zero, collision prevents
  translation, run is denied, or handoff is required. Facing and
  translation are independent.
- The binary codec already enforces the wire angle range, but the
  sim-domain API MUST reject `Yaw > 4095` when invoked directly,
  returning a stable movement-domain error rather than silently
  modulo-wrapping arbitrary values.

#### 5.3.3 Walk/run speeds and the run gate

- Wire `runFlag` remains raw `u8`. Movement semantics: `0` means
  walk requested, nonzero means run requested (tolerating additive
  future flag bits without a protocol break).
- Exact horizontal target speeds: walk `3.5 m/s`, run `7.0 m/s`.
  At 20 Hz that is `0.175 m` / `0.350 m` per tick; at another
  configured TickHz, `distance = speedMetersPerSecond * fixed
  dtSeconds` — never wall-clock elapsed time.
- The existing `205` `u8 speed` wire semantic is frozen as
  decimeters per second: `0` = stopped/blocked, `35` = 3.5 m/s
  walk, `70` = 7.0 m/s run. This is a semantic clarification only:
  the `205` wire layout does NOT change and no `msg_version` bump
  occurs. M4-T2's internal movement update uses the same `u8`
  scale so M4-T5 does not invent a second conversion.
- M4-T2 does NOT implement the full vigor system. It introduces a
  narrow sim dependency, conceptually:

```go
type RunGate interface {
    CanRun(EntityID) bool
}
```

- Rules: `runFlag == 0` walks; `runFlag != 0` with `CanRun ==
  true` runs; `runFlag != 0` with `CanRun == false` falls back to
  WALK. The move is not rejected, movement is not stopped, and no
  vigor is mutated. The existing normative condition (run requires
  vigor ≥ 10) stands; a later vitals task supplies that real
  decision behind the gate. No HP/mana/vigor fields are added
  merely to implement movement.

#### 5.3.4 MoveIntent, sequencing, and sample-tick sanity

- Sim-domain movement control, conceptually:

```go
type MoveIntent struct {
    InputSeq   uint32
    HeldDirs   uint8
    RunFlag    uint8
    Yaw        uint16
    SampleTick uint32
}
```

  `SampleTick` comes from the C→S protocol header tick; the
  protocol Header itself never enters sim.
- The sim-owner API, conceptually
  `SubmitMove(entityID, intent) (MoveDisposition, error)`,
  queues/replaces authoritative movement CONTROL state. It does
  NOT immediately integrate position; positions change only on
  `Step`. The disposition is at least
  accepted/duplicate/stale; duplicate/stale are ordinary no-op
  results, not errors.
- First input: for an entity with no previously accepted movement
  input, ANY `u32` inputSeq is accepted, including `0` and
  `MaxUint32`. Zero is not a "no input ever" sentinel; an
  explicit boolean tracks that state.
- Subsequent inputs use only canonical `serial32` arithmetic
  against the newest ACCEPTED sequence: same value is duplicate,
  serially-before is stale, serially-after is accepted. Two
  unequal values exactly `2^31` apart are serially ambiguous and
  rejected with a stable movement-domain error — never randomly
  classified as stale.
- Movement inputs describe current held control state, not
  incremental deltas, so multiple accepted inputs before the next
  sim tick coalesce: `10 → 11 → 12` accepted in sequence leaves
  pending control `12` only; the next Step processes control `12`
  and `lastProcessedInputSeq` becomes `12`, letting the client
  discard/reconcile everything through 12 without replaying
  intermediate held-state snapshots that never reached a tick.
  Ordering of a later Submit compares against the newest accepted
  value even before Step processes it.
- Active control persists: once processed, `heldDirs`, run
  request, and yaw remain the entity's active movement control
  until a newer accepted input is processed — so one `102` input
  (capped at ≤10 Hz) can drive multiple 20 Hz sim ticks.
- A newly processed input whose direction mask resolves to zero
  stops translation, sets speed 0, updates yaw, and advances
  `lastProcessedInputSeq`; that step still emits a reconciliation
  movement update because the processed anchor/control changed.
- `lastProcessedInputSeq` is updated when the sim Step actually
  consumes the pending newest input, NOT when SubmitMove merely
  accepts it, and stays unchanged across later ticks that
  continue the same held control. Because `inputSeq = 0` is
  legal, `hasProcessedInput` is tracked separately; a first
  processed input of sequence 0 yields the wire-valid anchor 0
  without reserving sequence zero.
- C→S sample-tick sanity (the framing spec's "more than 5 seconds
  in the future" rule) is implemented here in the movement
  domain. At TickHz H, `maxFutureTicks = 5 * H`:
  `sampleTick == currentTick + maxFutureTicks` is accepted, while
  a sample tick serially after current by more is rejected with
  `ErrFutureInputTick` (exact name flexible). `uint32` wrap is
  handled; past sample ticks stay acceptable for MVP (no
  lag-compensation rewind — the 2-second history ring is retained
  for later use); a sample tick exactly `2^31` away is ambiguous
  and rejected as invalid.
- Validation (entity existence, yaw validity, sample-tick sanity,
  serial InputSeq classification) is deterministic: a
  malformed/future-tick request mutates nothing — no pending
  input, active control, accepted/processed sequence, position,
  or yaw change.

#### 5.3.5 CollisionWorld seam and volume flags

- M4-T2 owns the minimal consumer-defined interface,
  conceptually:

```go
type CollisionWorld interface {
    SolidAt(world.Vec3) bool
    VolumeFlagsAt(world.Vec3) world.VolumeFlags
}
```

  No richer terrain API, no world loader. The richer future
  `WorldSource` MUST implement/embed/satisfy this minimal
  collision/volume query seam; M10 MUST NOT replace movement
  with a second collision abstraction.
- `internal/world` gains the opaque base type
  `type VolumeFlags uint32` with `const VolumeNone VolumeFlags =
  0`. T2 assigns no gameplay flag bit numbers (safe-zone, no-PK,
  sanctuary, interior semantics belong to later content/gameplay
  work); T2 merely preserves/samples the opaque bitset.
- `SolidAt` / `VolumeFlagsAt` are hot-path in-memory simulation
  queries: no network/PG I/O, no transient persistence errors.
  World loading failures happen before the sim is made ready.
- Normal walking/running computes X/Z translation only; Y is
  unchanged exactly. No gravity, jump, stairs, step-up, falling,
  or flying in T2.

#### 5.3.6 Integration, collision substeps, and cell staging

- Point-sampling tunneling is prevented by the fixed ceiling
  `MaxCollisionStepMeters = 0.25`. For intended displacement
  distance D: `substeps = max(1, ceil(D / 0.25))`,
  `subDelta = intendedDelta / substeps`, each substep testing
  the next candidate with `CollisionWorld.SolidAt`.
- From the authoritative position, each substep computes
  `candidate = current + subDelta`; on solid, movement stops,
  keeps the last non-solid position, marks blocked, sets final
  speed 0, and tests no later substeps. No sliding, no
  axis-separated resolution, no capsule sweep, no step-up, no
  bounce — intentionally minimal. Partial progress is retained:
  the final update carries the actual final authoritative
  position with `speed = 0`, `blocked = true`.
- M4-T3a still owns cell handoff. Before applying an intended
  movement step, if its intended final position belongs to
  another cell: do NOT transfer, do NOT partially creep toward
  the boundary in T2, leave X/Y/Z unchanged for that tick, mark
  the movement outcome HandoffRequired with `speed = 0`. Yaw and
  `lastProcessedInputSeq` still update normally. Per-tick
  ordering: compute the intended final position; if the
  destination cell differs, apply handoff-required staging with
  no collision traversal into the destination; else perform
  collision substeps within the current cell. While active
  controls point across a boundary, every tick holds position
  with `speed 0`, `handoffRequired true` — no silent teleport,
  no fake cross-cell ownership.
- Volume flags are sampled at entity insertion and after every
  movement-processing tick at the final authoritative position,
  stored as sim state. No gameplay behavior depends on them yet.

#### 5.3.7 Movement state, output, and Step order

- M4-T2 may extend the internal entity with ONLY movement-owned
  state such as yaw, speed, volumeFlags, active heldDirs/run
  request, hasAcceptedInput/lastAcceptedInputSeq, pending latest
  intent, and hasProcessedInput/lastProcessedInputSeq. No
  combat/vitals/inventory fields.
- `EntitySnapshot` may expose movement-owned authoritative
  `Yaw`, `Speed`, `VolumeFlags`, and `LastProcessedInputSeq`
  for later M4-T5/baseline inspection; pending-control
  internals stay private.
- The internal sim output (NOT `proto.EntityMove`),
  conceptually:

```go
type MovementUpdate struct {
    Tick                  uint32
    EntityID              EntityID
    Position              world.Vec3
    Yaw                   uint16
    Speed                 uint8
    LastProcessedInputSeq uint32
    Blocked               bool
    HandoffRequired       bool
    VolumeFlags           world.VolumeFlags
}
```

- Updates in one Step use canonical engine ordering (CellCoord
  X/Z, then EntityID ascending). An update emits when at least
  one holds: nonzero active movement vector, a new movement
  input processed this tick, authoritative position changed,
  collision blocked, or handoff required. A never-controlled
  stationary entity needs no 20 Hz movement update (its T1
  history sample still applies). Held movement may therefore
  emit at 20 Hz; M4-T5 later owns ≤10 Hz per-entity AOI fanout
  and state-lane coalescing — the authoritative simulation is
  never throttled to 10 Hz.
- With movement, one Step runs: tick++ → canonical cells →
  EntityID-sorted entities → consume newest pending movement
  input → update active yaw/control/processed anchor →
  integrate horizontal movement → collision / cross-cell staging
  → sample final volume flags → emit MovementUpdate if
  applicable → append final position-history sample. No gameplay
  runs after the history append.

#### 5.3.8 Sink, deps, and the anomaly tripwire

- The narrow non-blocking future-fanout seam, conceptually
  `MovementSink{ OnMovement(MovementUpdate) }` (or a function
  adapter), receives already-authoritative results: it cannot
  change position, approve run, resolve collision, or alter the
  anchor. Implementations MUST be non-blocking/bounded (M4-T5
  satisfies this via the existing non-blocking outbound state
  path); T2 tests use a synchronous bounded recorder, never
  per-session queues.
- `EngineDeps` extends with at least `Collision` and `RunGate`
  as required decisions (fail fast when missing); movement and
  anomaly observers MAY default to no-ops. `NewEngine` remains
  pure with no background movement goroutine.
- The narrow hook `MovementObserver{ MovementAnomaly(...) }`
  carries low-cardinality domain data (entity, tick, kind,
  expected/observed distance); no Prometheus, gateway, or
  session objects here.
- Anti-teleport clarification: opcode `102` carries NO client
  position, so direct client teleporting is impossible by
  protocol construction. The "speed/teleport anomaly"
  responsibility is a defensive authoritative-integration
  tripwire: the computed candidate must remain finite and its
  horizontal displacement may not exceed
  `selectedSpeed * fixedDT + epsilon` (epsilon `1e-9` m for
  floating arithmetic only — NOT 0.5 m gameplay slack). On
  violation the engine corrects to the pre-step position with
  `speed = 0` and reports through the observer. Ordinary
  collision blocks, handoff staging, and run-gate denials are
  authoritative corrections, never anomaly events. The 0.5 m
  rule stays client-side: the client compares its prediction
  against `205` and rewinds/reapplies; the server only supplies
  authoritative position/yaw/speed/`lastProcessedInputSeq`.
- Internal movement output carries `EntityID uint64`, never a
  session-local `NetEntityID u32` (no truncation/cast); M4-T5
  maps per-session handles and zeroes the anchor for non-owner
  viewers while the owner's `205` carries the real anchor.

### 5.4 Cell ownership and entity handoff (frozen, v0.3.16)

This section refines §5.1 for M4-T3a: per-entity cell ownership
generations, deterministic movement-triggered handoff, and bounded
migration-intent routing. It adds no cross-cell operations, no PG
work, and no gateway wiring.

M4-T3a owns ONLY: per-entity cell ownership generation, resident vs
migrating ownership state, movement-triggered cross-cell handoff,
deterministic handoff tick semantics, source quiescence, destination
installation, bounded movement-intent queue during migration,
generation validation/idempotent handoff delivery, and
handoff race/determinism tests.

M4-T3a does NOT own: cross-cell opID generation, cross-cell
damage/trade operations, generic opID dedupe caches, post-commit PG
reconciliation, the snapshot saver, gateway opcode-102 production
wiring, NetEntityID mapping, AOI, presence, outbound 204/205/206
fanout, or inbound token buckets. Ownership remains: M4-T3b →
opID/dedupe/retry infrastructure; M4-T3c → post-commit durable
reconciliation; M4-T4 → saver; M4-T5 → real gateway/AOI/rate-limit
integration.

At M4-T3a the only implemented sim gameplay intent is `MoveIntent`,
so T3a implements migration queue/routing semantics for movement;
future gameplay tasks MUST reuse the same ownership/migration
concept rather than inventing a second handoff mechanism. No fake
Attack/Trade implementations exist merely to make the queue
"generic". M4-T3a does NOT send wire `202` itself: §5.1's "sender
receives `202 error{retry}` on migration-queue saturation" becomes
the domain result `ErrMigrationQueueFull`, which M4-T5 later maps to
`proto.ErrorCodeRetry`. T3a imports neither `internal/proto` nor
`internal/gateway` for this.

#### 5.4.1 Ownership identity

Every resident entity carries an ownership reference,
conceptually:

```go
type OwnerRef struct {
    Cell       world.CellCoord
    Generation uint64
}
```

Rules: Generation 0 is reserved/invalid; a new entity starts at
Generation 1; each successful cell handoff increments Generation
exactly once; the EntityID never changes; generation never
decreases and never wraps to 0. The pair `{Cell, Generation}` is
the ownership epoch. The immutable inspection snapshot exposes the
current ownership generation alongside the existing `Cell`; no
mutable ownership pointer escapes.

If an entity at `Generation == math.MaxUint64` would need another
handoff: do NOT wrap, transfer, change cell, or create destination
membership. A stable internal `ErrOwnershipGenerationExhausted`
(or equivalent) results; for movement processing, yaw and a newly
processed reconciliation anchor may advance while translation
holds at the source position with `speed = 0` and
`HandoffRequired = true`. This practically-unreachable internal
exhaustion is NOT migration queue saturation and MUST NOT become
`202 retry`.

#### 5.4.2 Resident vs migrating states

A live entity is conceptually `RESIDENT` or `MIGRATING`. At normal
tick boundaries in the local MVP all healthy live entities are
`RESIDENT`. `MIGRATING` is the explicit handoff state between
source quiescence and destination installation: the old source
`OwnerRef` remains the logical authoritative owner but is QUIESCED
and performs no further gameplay mutation; the destination accepts
no entity gameplay yet; new movement intents go to the bounded
migration queue. There is never a moment where both source and
destination mutate the entity.

Because all cells live in one process, begin-migration and
destination install normally occur within the same simulation Step
— but they MUST be distinct internal operations/state so tests
can prove source quiescence, mid-migration intent routing,
generation validation, queue saturation behavior, and destination
install. Migration MUST NOT be faked by merely assigning
`entity.cell = dest` inside movement.

#### 5.4.3 Tick-start worklist and at-most-once processing

At the start of every Step, after incrementing the tick, the
engine captures the resident entity worklist in canonical order
(CellCoord X ascending, then Z ascending, then EntityID
ascending), each item retaining enough ownership identity to
prove it still names the same resident epoch when processed
(conceptually EntityID + source CellCoord + source Generation).
The worklist is ownership identity/order only, never copied
entity state.

An entity in the tick-start worklist is processed AT MOST ONCE
per tick: migrating into a later-sorting (or earlier-sorting)
cell grants no second processing; a newly installed destination
entity becomes eligible for normal cell processing on the NEXT
tick only. Before processing an item the engine verifies it is
still resident under the expected cell and generation; a
legitimately removed/migrated item is skipped, never resurrected.

#### 5.4.4 Collision-aware handoff flow

v0.3.15's handoff-before-collision staging was temporary and is
superseded: T3a computes the intended displacement, performs the
normal deterministic `0.25 m` collision substeps ACROSS the path
(the source owner MAY query `CollisionWorld` for candidates past
the boundary — world query data, not mutable neighbor-entity
state — but MUST NOT mutate the destination's entity collection
except through handoff install), obtains the ACTUAL final
authoritative position, and routes on
`CellForPosition(actualFinal)`: same cell stays resident with no
generation change; a different cell hands off to that computed
cell (never a hard-coded ±1 neighbor, so diagonal corner
crossings into `X±1, Z±1` and future portal/admin remaps work).

Collision before the boundary yields the normal blocked result
with no migration and unchanged generation. Collision after
crossing retains the last-free position and, when that position
is destination-side, hands off with `speed = 0`, `Blocked =
true`. Collision never forces rollback to the source merely
because blocking happened after crossing.

Begin-handoff preconditions (entity resident, resident cell and
generation match the expected source, destination differs,
destination position valid, next generation representable and
nonzero) mutate nothing on failure. All source-owned gameplay
calculation for the tick (pending-input consumption,
yaw/control/anchor update, run-gate decision,
movement/collision/final-position calculation) completes BEFORE
the handover point; afterwards only destination-owned
installation/finalization mutates the entity.

#### 5.4.5 Handoff record, install, and idempotence

The internal handoff/migration record carries at minimum the
`EntityID`, the From/To `OwnerRef`s, and the migrating
entity/state — never database IDs, NetEntityIDs, session IDs, or
proto messages. Handoff preserves EntityID,
position-history ring (transferred, never recreated: no gap, no
loss, no ABA), yaw, speed, volume flags, active heldDirs/run
request, accepted/processed sequence state, any pending
intent, and the recent-opID dedupe cache (§5.5). Nothing resets
merely because a cell changed.

Destination installation atomically, under the one sim writer:
remove source resident membership / quiesced migration
ownership, lazily create the destination active cell, install the
same EntityID exactly once, set destination Cell and Generation
`G+1` (exactly one increment per successful handoff; one
authoritative increment site, never begin+install double bumps),
update locator/route, preserve all gameplay state, restore the
resident route, drop the migration record, and remove an emptied
source cell. EntityCount is unchanged; the same EntityID is never
resident in both cell maps at once.

Installation validates the ownership token (From `G` → To
`G+1` against the same migration; wrong source/destination,
wrong EntityID, or generation gap is a stable `errors.Is`
ownership invariant error, never a silent repair). A duplicate
delivery of an already-installed transfer inserts nothing,
bumps nothing, resets nothing, and replays no queue (ownership
generation suffices — no T3b opID). A stale (older-generation)
delivery mutates nothing and never rolls ownership backwards; a
future-generation gap is an invariant error.

#### 5.4.6 Healthy movement output and history

A successful cross-cell transfer reports the final destination
position, the actual final movement speed (or 0 when collision
blocked), the actual collision outcome, and
`HandoffRequired = false` — that marker now means only a
handoff that could NOT be performed (e.g. generation
exhaustion/internal inability), and stays an internal field,
never a wire field. Destination volume state is
`VolumeFlagsAt(final destination position)`; snapshot, update,
and history-final state agree on the same final position/flags.

Tick-N crossing appends exactly ONE `PositionSample` for tick N
holding the final destination position — never the source
position, the boundary point, or two samples. Held movement
continues without pause: tick N crosses one full normal
distance, tick N+1 moves another full normal distance in the
destination (no pause ticks, no same-tick double movement).

The manual `SetPosition` API stays same-cell-only:
cross-cell `SetPosition` still returns
`ErrCellHandoffRequired` with zero mutation. Movement handoff
uses its dedicated ownership path; portal/admin remaps belong
later.

#### 5.4.7 Bounded migration MoveIntent queue

Each migrating entity owns a true bounded FIFO movement queue
with `MigrationMoveQueueCapacity = 64` — generous against the
≤10 Hz input rate and the normally sub-tick local window, with
no unbounded growth, no external broker, no goroutine.

Resident `SubmitMove` behavior is byte-for-byte v0.3.15
(yaw/sample-tick validation, first-seq-any, dup/stale, RFC1982
ambiguity, newest-accepted tracking, pending coalescing).
While migrating, intents do NOT touch active control, the
processed anchor, or the quiesced source; they route into the
migration queue after stateless validation (`Yaw <= 4095`,
sample-tick sanity vs the current sim tick — invalid movement
never consumes queue capacity). Migration routing keeps its own
sequence frontier seeded from the entity's existing accepted
state: duplicates return `MoveDuplicate`, stale return
`MoveStale`, half-range gaps return `ErrAmbiguousInputSeq`,
and only newer accepted intents queue — without yet mutating
`lastAcceptedInputSeq`, pending, active controls, or the
processed anchor.

A valid/newer intent arriving at a full 64-entry queue returns
`ErrMigrationQueueFull` with ZERO mutation (no append, no
frontier advance, no InputSeq consumption, queued content
untouched), so the same InputSeq stays retryable once capacity
frees or the handoff completes. Only this saturation maps (in
M4-T5) to `202 error{retry}` — never mere migrating status,
generation changes, or completed handoffs.

After destination installation the queue drains FIFO through
the SAME resident sequencing/coalescing semantics (never a
manual final-payload copy): queued `10, 11, 12` leaves
`lastAcceptedInputSeq == 12` with pending `12` while the
processed anchor stays at the crossing tick's value; the queued
newest control is processed on the NEXT simulation tick, never
as same-tick second movement. Queueing/draining alone never
touches position history.

T3a adds no generic opID dedupe, no PG involvement, no
per-cell goroutines/actors/buses, and no mutex-protected
concurrent `Step`/`SubmitMove` claims: the single-sim-writer
model stands, and M4-T5 owns real ingress serialization. The
ownership seam is shaped so future transport can replace local
delivery without changing gameplay rules.

### 5.5 Cross-cell operation delivery and deduplication (frozen, v0.3.17)

This section freezes M4-T3b cross-cell operation infrastructure:
the opaque `OpID` domain, its deterministic Snowflake-style
generator, owner-addressed routing metadata, the retry-safe
delivery contract, receiver-side apply-once semantics, the bounded
recent-`OpID` cache, handoff preservation of dedupe state, and a
test-only synthetic aggregate proof.

M4-T3b owns ONLY: the opaque `OpID` domain, the
deterministic/testable `OpID` generator, target ownership routing
metadata, the retry-safe delivery contract, the receiver-side
exactly-once application guard, the bounded recent-`OpID` cache,
handoff preservation of dedupe state, and the synthetic aggregate
proof.

M4-T3b does NOT own: real damage, combat formulas, trade, give,
inventory transfer, the PG transaction, post-commit reload,
gateway behavior, wire messages, AOI, presence, or the saver.
Ownership remains: M4-T3c → post-commit durable reconciliation;
M4-T4 → saver; M4-T5 → real gateway/AOI/rate-limit integration;
M5 → combat; M8 → real trade.

Synthetic means synthetic: T3b MUST NOT implement fake
production trade/combat semantics. Tests prove delivery with a
test-only synthetic aggregate (for example a deterministic
integer/counter mutation: one op, multiple deliveries, exactly
one mutation). Production infrastructure stays domain-neutral.

#### 5.5.1 OpID domain

The cross-cell operation identity is an opaque domain,
conceptually:

```go
type OpID uint64
```

Rules: `0` is invalid/reserved; one generator never reuses an
ID; an `OpID` is NOT a PostgreSQL ID, NOT an `EntityID`, NOT a
session-local NetEntityID, and NOT a protocol sequence. There is
no numeric relationship between those domains.

#### 5.5.2 Exact Snowflake-style layout

The 64-bit layout is frozen as:

```text
[ 41-bit timestamp-ms delta ][ 10-bit worker ][ 12-bit sequence ]
```

The high bit remains zero. Constants: timestamp bits `41`,
worker bits `10`, sequence bits `12`; max worker `1023`; max
sequence `4095`. Packing is:

```text
id = (timestampDelta << 22) | (workerID << 12) | sequence
```

#### 5.5.3 Custom epoch

The custom epoch is frozen as `2026-01-01T00:00:00.000Z`. The
timestamp portion is `unixMillis(now) - unixMillis(epoch)`. The
usable 41-bit window is roughly 69 years.

#### 5.5.4 Worker IDs

Worker `0` is reserved/invalid; valid workers are `1..1023`.
This guarantees generated `OpID` 0 is impossible even at the
exact epoch with sequence zero. No random worker ID, no hostname
hashing. Later executable/deployment configuration chooses a
worker ID; the T3b constructor receives it explicitly.

#### 5.5.5 Generator clock seam

The generator uses an injected millisecond clock, conceptually:

```go
type OpIDNow func() int64
```

returning Unix milliseconds (exact type may differ). No hidden
`time.Now()` in generator tests. A production helper may wrap
`time.Now().UnixMilli()`, but no `cmd/serve` wiring happens yet.

#### 5.5.6 Sequence behavior and exhaustion

For a new millisecond `sequence = 0`; each additional ID in the
SAME millisecond increments the sequence, up to `4095` — thus up
to 4096 IDs per worker per millisecond. The 4097th requested ID
in the same millisecond returns a stable
`ErrOpIDSequenceExhausted` error with zero generator-state
corruption. The generator does NOT sleep, busy-spin, wait for
the clock, wrap the sequence, or borrow a future timestamp; the
caller may retry once time advances. This keeps tests and sim
ownership deterministic.

#### 5.5.7 Clock regression

If observed milliseconds move backward below the last emitted
timestamp, generation returns a stable
`ErrOpIDClockRegression` error: no ID is emitted, no state rolls
back. Once the clock catches up to the last timestamp or
advances, generation may resume safely.

#### 5.5.8 Timestamp validity

Before the custom epoch the generator returns a stable
`ErrOpIDTimeBeforeEpoch` error; beyond the 41-bit representable
delta it returns a stable `ErrOpIDTimestampExhausted` error. No
truncation, no wrap.

#### 5.5.9 Generator concurrency model

T3b keeps the current sim architecture: one owning writer per
generator. No generator mutex is required. One generator
instance is NOT claimed safe for arbitrary concurrent callers;
future workers each use their own worker ID/generator.

#### 5.5.10 OpID ordering

Generated IDs are roughly sortable by timestamp, worker, and
sequence, but gameplay correctness MUST NOT depend on numeric
`OpID` ordering. `OpID` means identity/idempotence, not causal
order.

#### 5.5.11 Cross-cell envelope

The minimal routing metadata is frozen conceptually as:

```go
type CrossCellOp struct {
    ID          OpID
    Target      EntityID
    TargetOwner OwnerRef
}
```

No production payload or operation kind is required yet; the
synthetic test supplies its apply behavior separately. A sender
makes its routing decision against a committed ownership
snapshot, so the envelope names both the target `EntityID` and
its expected `TargetOwner {Cell, Generation}`. A receiver MUST
NOT mutate an entity merely because the `EntityID` still exists
if ownership no longer matches.

#### 5.5.12 Delivery ownership gate

Before apply, the target must be RESIDENT and its current
`OwnerRef` must exactly equal the envelope's `TargetOwner`;
otherwise no mutation occurs:

- Target currently MIGRATING and op not already known-applied: a
  stable retryable `ErrCrossCellTargetMigrating` domain error
  (or equivalent). No apply, no dedupe insertion. The caller
  retries the SAME `OpID` later. No wire `202`.
- Target resident but current owner differs from the envelope's
  `TargetOwner`: a stable `ErrCrossCellStaleRoute` domain error
  (or equivalent). No mutation. The caller refreshes the current
  `OwnerRef` and retries the SAME `OpID`; it does NOT allocate a
  replacement `OpID`.
- Unknown/removed `EntityID`: `ErrEntityNotFound`. No dedupe
  record. This is not silently converted into route-stale.
- Invalid `OpID` (`OpID(0)`): a stable `ErrInvalidOpID` error.
  No mutation, no cache insertion.

#### 5.5.13 Receiver-side exactly-once primitive

Production sim exposes one internal apply-once primitive,
conceptually:

```go
deliverCrossCellOp(
    op CrossCellOp,
    apply func(*entity) error,
) (OpDisposition, error)
```

Exact signature/name may differ; it remains internal to sim if
appropriate. For an ownership-matching resident target the
ordering is binding:

```text
1. validate OpID
2. check recent-op cache
3. if seen -> duplicate, apply not called
4. invoke apply
5. only after successful apply -> record OpID
6. return applied
```

If `apply` returns an error, the `OpID` MUST NOT enter the
dedupe cache; a retry with the SAME envelope/`OpID` may invoke
apply again, and the infrastructure does not invent a new ID.
Because T3b is in-memory infrastructure, the supplied apply
function must obey: on error, no partial authoritative mutation
— future real sim operations validate first and make an
in-memory mutation that cannot subsequently fail. T3c separately
handles durable PG commit/reconciliation; T3b does not roll back
arbitrary partially-mutating callbacks.

Lost acknowledgement: if apply succeeds, dedupe records the
`OpID`, and the sender fails to observe success and retries the
same envelope, the retry is a duplicate and apply is not called
again. This is the core exactly-once receiver property.

Dispositions are at least `Applied` and `Duplicate`; a normal
duplicate is not an error. Route/migrating/invalid/apply
failures remain errors.

#### 5.5.14 No automatic background retry scheduler

T3b's `retry` contract means retry by resubmitting the exact same
`CrossCellOp` with the same `OpID` after a transient apply
failure, target migration completion, route refresh, or
transport/lost-ack uncertainty. T3b does NOT add a retry
goroutine, timer wheel, exponential backoff, external bus, or
persistent outbox. Future sharding transport may schedule
retries while preserving this exact identity contract.

#### 5.5.15 Bounded recent-op cache

Each entity that receives cross-cell operations owns a bounded
recent-op cache with `RecentOpIDCapacity = 256`. No unbounded
set. The cache tracks the most recent 256 SUCCESSFULLY APPLIED
distinct `OpID`s for that entity. Duplicate delivery does not
reorder the cache and does not insert a second entry; failed
applies do not enter it. Implementation uses O(1) membership
with bounded FIFO/ring eviction (no full linear scan per
cross-cell operation); lazy initialization is preferred so
entities that never receive cross-cell operations do not
allocate a 256-entry structure, and steady-state operation does
not grow without bound after initialization/fill.

This bounded cache covers the internal retry, lost
acknowledgement, handoff, and short transport redelivery window
only — not permanent event sourcing. Once an `OpID` has aged out
of the bounded recent cache, T3b no longer promises indefinite
replay suppression, and internal transports MUST NOT retain
retry deliveries indefinitely. The ledger remains audit/durable
facts but is NOT an event-sourced replay log; cross-cell op
dedupe is ephemeral sim safety, and a restart restores
materialized PG state rather than rebuilding an infinite
cross-cell op history.

#### 5.5.16 Handoff interplay

The entity's recent-op cache is part of handoff-owned state.
Because T3a transfers the same entity object locally, the cache
transfers automatically with that entity — it is not reset, and
the handoff contract (§5.4.5) lists recent op dedupe state among
preserved state. When T3a's local handoff is someday serialized
across workers, the recent-op cache (or semantically equivalent
dedupe state) MUST travel in the handoff payload, preserving
duplicate suppression across ownership transfer.

Required semantics:

```text
op X applied in cell A
entity hands off A -> B
same op X redelivered with stale owner A
    -> route stale, no apply
sender refreshes owner B, retries SAME OpID X
    -> Duplicate, no second mutation
```

If op X has NOT yet applied and the target is MIGRATING, the
delivery fails with the migrating error (no apply, no cache
insert); after handoff completes the sender refreshes the owner
and retries the SAME `OpID`, which applies exactly once.

The synthetic aggregate operation targets an entity, so T3b
instantiates the bounded cache per entity; the cache type should
be reusable by future cell-scoped receivers, but no unused
per-cell cache is created before a real cell-scoped operation
exists. Ownership-generation duplicate handoff delivery and
cross-cell `OpID` duplicate operation delivery stay distinct:
T3a handoff idempotence remains generation-based; T3b operation
idempotence is `OpID`-based.

#### 5.5.17 Synthetic aggregate proof

Tests use a synthetic mutable aggregate (for example `value
int64` plus an apply-call count) associated with the target
`EntityID`; a synthetic operation conceptually performs `value
+= delta`. Production entities gain NO fake synthetic-value
field, and no production `SyntheticDamage` / `SyntheticTrade` /
`SyntheticCounter` types are necessary unless they live
exclusively in `_test.go`. Production infrastructure remains
reusable; because no real cross-cell gameplay operation exists
yet, T3b invents no operation-kind registry (`OpKindDamage`,
`OpKindTrade`, and the like arrive with their owning tasks).

The synthetic sender/coordinator obtains the target `OwnerRef`
and a fresh `OpID`, then creates a `CrossCellOp`; it never
obtains a mutable neighbor entity pointer, and the receiver-owned
delivery primitive performs the authoritative mutation. This
preserves: the coordinator cell NEVER mutates the neighbor
directly. No API such as `GetMutableEntityInOtherCell(...)` is
added, and `*entity` is not exported; all mutation goes through
the owner-side apply primitive. The infrastructure need not
artificially reject an operation whose source and target happen
to resolve to the same process/cell — T3b proves receiver
idempotence, while later coordinator rules decide when a
cross-cell envelope is necessary.

#### 5.5.18 Removal, history, and timing

Removing an entity discards its recent-op cache with the entity;
a newly allocated `EntityID` begins with an empty cache, and
`EntityID` is never reused, so no ABA collision with old `OpID`s
occurs. Cross-cell dedupe operations that do not alter position
do not append position history by themselves — history remains
Step-owned. A synthetic operation may be delivered synchronously
between Steps in tests; T3b invents no separate operation tick
queue, and the exactly-once primitive itself is independent of
that scheduling decision (future gameplay systems may route
intents into their owner-step phase as needed).

#### 5.5.19 Generator properties

Receiver dedupe treats `OpID` as opaque equality identity: it
does not rely on unpacking timestamp/worker/sequence from an
`OpID`. `serial32` arithmetic is NOT used for `OpID` (no wrap
semantics; generator exhaustion/error is explicit), and clock
regression or sequence exhaustion must never cause duplicate
IDs — after an error and valid forward time, generation resumes
from safe state. Same timestamp and sequence on different
workers must produce different IDs (no collision). A test-only
or production inspection helper may unpack `OpID` fields for
tests, but gameplay never depends on unpacked
timestamp/worker/sequence. The generator uses time plus the
explicit worker plus a counter only: no RNG (it does not consume
engine RNG), no sleeps, no busy waits, no goroutines. Two
generators with the same worker and the same explicit time
script produce byte-for-byte identical ID sequences; a different
worker produces different IDs.

### 5.6 Post-commit durable reconciliation (frozen, v0.3.18)

This section freezes M4-T3c post-commit reconciliation: the
fencing that marks in-memory aggregates stale after their PG
transaction commits but before their in-memory commit
notifications are confirmed, the persisted-revision tracking
that detects dropped notifications, and the mandatory staged
PG reload before the aggregate's next mutation.

M4-T3c owns ONLY: post-PG-commit reconciliation fencing,
per-in-memory-aggregate persisted revision tracking,
commit-notification application ordering, dropped-notification
detection state, mandatory PG reload before the next mutation,
reload failure behavior, revision-gap handling, integration with
T3b same-OpID delivery semantics, the synthetic unit proof, and
the real PG18 reconciliation proof.

M4-T3c does NOT own: real trade, give, bank gameplay,
inventory transfer gameplay, combat, death, new persistence
schema, the snapshot saver, dirty queues, periodic saves,
gateway, AOI, presence, rate limits, wire messages, background
retry, or an external bus. Ownership remains: M4-T4 → snapshot
saver / periodic persistence; M4-T5 → gateway/AOI/runtime
ingress; M8 → real trade/bank/vault gameplay transactions.

#### 5.6.1 PG remains authoritative

D7 / §8.1 stand sharpened: PG materialized state is the
recovery source of truth; the ledger is audit only.
Post-commit reconciliation MUST reload materialized aggregate
rows from PG, and MUST NOT replay ledger rows, replay old
operation notifications as an event log, guess the missing
mutation, or reverse the PG commit.

A critical operation executes validate, then a PG transaction
commit, then in-memory commit notification delivery — and step
2 and step 3 cannot be one atomic distributed operation. A PG
commit success combined with a missing/dropped/failed
notification must therefore leave an explicit in-memory
condition saying the aggregate may be stale relative to PG, and
the owning sim state MUST NOT accept another mutation for that
aggregate until it has reconciled. There is no
PG-transaction-plus-in-memory-notification pseudo-transaction:
the PG transaction is never held open while waiting for a cell
callback, channel delivery, or future acknowledgement. Correct
recovery is commit PG, mark the reconciliation requirement,
attempt notifications, and reload PG when required.

#### 5.6.2 Reconciliation state

The sim-domain reconciliation state is small and reusable,
conceptually:

```go
type ReconcileState struct {
    knownRevision    int64
    pending          bool
    requiredRevision int64
}
```

Exact field visibility/names may differ. It is single-writer,
in-memory, and ephemeral: not persisted, not a ledger, and not
an OpID cache. No mutex is required; it is not arbitrarily
concurrent-safe.

`knownRevision` means the persisted aggregate revision that the
current in-memory aggregate contents are known to represent. It
is NOT the ownership generation, `EntityID`, `OpID`, tick, or
`inputSeq` — those domains remain completely separate and are
never compared against persisted revisions. Persisted root
revisions are `int64 >= 0` (newly persisted rows normally start
at `0`); a negative reconciliation revision is invalid and
returns a stable internal error, with no unsigned cast.

When a durable aggregate is initially loaded from PG, the
in-memory contents equal the loaded materialized snapshot with
`knownRevision` set to the persisted revision and `pending`
false. A known persisted aggregate revision is never
initialized to zero unless PG actually says zero, preserving
§8.1 restart semantics.

#### 5.6.3 Commit identity and fencing order

A successful critical PG transaction returns or otherwise makes
known the NEW persisted revision for every mutated aggregate
root — conceptually participant A → revision RA, participant B
→ revision RB, and so on. Different roots may carry different
revision numbers; no transaction-wide revision is assumed. The
durable multi-owner operation reuses the SAME T3b `OpID` for
its in-memory commit notifications: T3c invents no `CommitID`,
`TransactionID`, or `NotificationID` competing idempotence
domain. T3b `OpID` remains the operation identity; the
persisted root revision remains the recovery/version domain.

After the PG transaction successfully COMMITs, the fencing
order is binding: FIRST mark every affected live in-memory
aggregate as requiring at least its newly committed revision,
and ONLY AFTER ALL participant fences are installed attempt
any in-memory commit notifications. Notifying A before fencing
B is forbidden, because an intervening B mutation could observe
stale state. Under the current single sim writer, fencing all
participants is one deterministic synchronous phase. If the PG
transaction fails validation, fails CAS, fails SQL, rolls
back, or fails COMMIT, then there is NO reconciliation fence,
NO commit notification, and NO in-memory committed-state
update — the old in-memory state remains authoritative because
PG did not advance. If the entire process crashes after PG
commit but before the fence installs, restart recovery loads
PG materialized state and the stale-memory problem disappears
with the process; a surviving coordinator MUST fence
synchronously before returning control to gameplay. No outbox
is introduced for this MVP case.

The mark primitive, conceptually `MarkCommitted(revision
int64) error`, obeys: revision < 0 is an invalid-revision
error with zero mutation; revision <= knownRevision needs no
reconciliation (stale/already-observed mark); revision >
knownRevision sets `pending = true` with `requiredRevision =
max(requiredRevision, revision)`, so several committed
revisions marked before reload never lower the requirement
(known 5, then marks 6 and 7, leaves pending with required 7).
Marking changes reconciliation metadata only: no gameplay
fields, no in-memory persisted revision, no OpID dedupe, no
delta, no history touch.

#### 5.6.4 Commit notifications

A commit notification carries generic revision metadata,
conceptually:

```go
type DurableCommitNotice struct {
    ID       OpID
    Revision int64
}
```

The actual gameplay payload is owned by the future real
operation; T3c adds no production trade, inventory,
bank-transfer, or damage payload. The notification ID is
exactly the T3b operation's OpID: no replacement ID on
redelivery, after route refresh, or after reload.

Reconciliation does NOT replace T3b dedupe — the layers answer
different questions. T3b: did this in-memory operation OpID
already apply? T3c: does this in-memory durable aggregate
reflect at least the persisted PG revision required after
committed transactions? A bounded T3b recent-OpID hit does not
prove an unrelated newer durable revision was loaded, and a
durable revision does not replace OpID idempotence.

A commit notification may be applied as an incremental
in-memory update ONLY when `notice.Revision == knownRevision +
1` — this ordering is binding. For known 5 with required >= 6
and notice 6, the apply callback may execute, and only after
successful apply does `knownRevision` become 6; the fence
clears when `knownRevision >= requiredRevision`, otherwise it
remains pending for the later required revision. When
`notice.Revision <= knownRevision` the notice is a
stale/already-applied no-op: the payload is not applied again
(disposition at least `CommitNoticeApplied` /
`CommitNoticeStale`; a revision gap remains an error requiring
reload), with no state rollback — this covers especially a
notification arriving late AFTER a PG reload already brought
memory to that revision or beyond. When `notice.Revision >
knownRevision + 1`, the missed intermediate state cannot be
reconstructed incrementally: the apply callback is NOT
invoked, `requiredRevision >= notice.Revision` is
marked/retained, and a stable `ErrReconcileRequired` /
revision-gap error returns; recovery is a complete
materialized-PG reload, never replaying missing notifications.
If the apply callback returns an error, `knownRevision` is
unchanged, pending remains true, and `requiredRevision` stays
at least `notice.Revision`; the notification OpID is not
treated as successfully applied by any T3b apply-once wrapper,
and the same notification may be retried. Like T3b's apply
callback, a notification callback must leave the in-memory
aggregate unmodified on error (validate first, then make an
in-memory replacement/mutation that cannot subsequently fail);
T3c rolls back no partially-mutating callback.

#### 5.6.5 Mandatory reload before the next mutation

While `pending == true` the owning aggregate MUST NOT accept a
new mutating intent before successful PG reconciliation: the
reload happens BEFORE validation whose correctness depends on
aggregate state, BEFORE mutation, and BEFORE generating a new
durable write based on that state. Mutating first and hoping
CAS catches it later is forbidden; read-only inspection may
still be served with the understanding it may be stale, but
further authoritative mutation is prohibited. The mutation
guard is a small operation, conceptually `EnsureReconciled(ctx,
reload)`, called as reconcile-first-then-validate-then-mutate,
with no hidden goroutine and no background polling (an optional
`WithMutation` convenience wrapper may exist if it enforces
the same order). While `pending == false` the guard performs
ZERO PG reads — reconciliation reload is exceptional recovery
only, never a per-mutation database read.

Reloads use full materialized state: the root persisted
revision, root gameplay state, and root-owned child state
where applicable (for a future character aggregate that means
character plus guarded children; the T3c PG proof
intentionally uses the simpler existing bank root, which has
no child rows). The reload callback MUST NOT reconstruct from
the ledger; the ledger may be inspected for audit/debug but is
never recovery input.

A PG reload stages its candidate before touching memory,
conceptually:

```go
type ReloadCandidate struct {
    Revision int64
    Apply    func() error
}

type ReloadFunc func(context.Context) (ReloadCandidate, error)
```

Exact names may differ. The loader reads PG into temporary
values and returns the loaded revision plus an apply closure;
reconciliation validates the revision BEFORE invoking
`Apply`, so a stale load can never overwrite newer memory.
Required before replacement: `candidate.Revision >=
knownRevision`, `candidate.Revision >= requiredRevision`, and
`candidate.Apply != nil`; otherwise `Apply` is not invoked. A
loader revision below `requiredRevision` yields a stable
`ErrReconcileRevisionBehind` with pending true and memory
unchanged (guarding read-replica lag, buggy fake loaders, and
stale reads). A loader revision below `knownRevision` yields a
stable invariant/revision-regression error with no apply —
in-memory persisted revision never moves backward. A PG load
failure returns/wraps the reload error with pending true and
no mutation invoked; the next mutation attempt may retry, with
no local fallback mutation. A valid candidate whose `Apply`
succeeds sets `knownRevision = candidate.Revision`, clears
pending, and resets the required revision; a valid candidate
whose `Apply` fails leaves everything unchanged and keeps the
mutation blocked. PG is authoritative, so reload may leap
forward: a fence requiring revision 6 with PG at revision 8
correctly applies the complete revision-8 snapshot and clears
the fence, never demanding an exact revision match.

#### 5.6.6 Required ordering scenarios

Sequential pending notifications need no reload: with known 5
and fences 6 then 7, notice 6 applies (known 6, still pending
required 7) and notice 7 applies (known 7, pending clears).
A missing middle notice always gaps: with known 5 and
required 7, notice 7 alone is a gap — payload not applied,
reload required (mandatory test coverage). A late notice after
reload is a no-op: commit revision 6, notice dropped, reload
brings memory to revision 6, and the late revision-6 notice
applies no second gameplay mutation (routed through T3b, the
successful no-op may then become a seen OpID without changing
durable contents). A notice older than a newer reload is
likewise stale: required 6, reload obtains 8, late notice 6 is
a no-op that never rolls revision 8 back to 6.

#### 5.6.7 Cross-layer invariants

Reconciliation metadata never enters the 256-entry T3b OpID
ring: revisions and requirements live in `ReconcileState`,
operations live in `recentOpIDs`. Within one live sim owner the
T3b dedupe cache remains ephemeral and independent — reloading
durable fields must not accidentally reset it. After a dropped
notification T3c MAY remain pending until the next mutation;
the guarantee is reload-before-next-mutation, never an
immediate background reload: no reload goroutine, no polling.
When sequential notifications all arrive and apply, memory
advances and pending clears with no PG reload (the healthy
fast path). If reconciliation needs PG and PG is unavailable,
mutation stays blocked with pending intact — consistent with
§10's no-local-critical-state-exception rule. Reconciliation
state never appears in protocol/entity snapshot messages, and
cell-handoff ownership generations never compare against
persisted revisions: either domain may advance while the other
does not. T3c attaches no fake durable aggregate to every sim
entity, but any real durable aggregate owned by an
entity/cell's reconciliation state MUST conceptually travel
with its owner exactly like the aggregate state: a handoff
clears neither pending reconciliation nor known/required
persisted revisions.

M4-T4 later owns periodic snapshot CAS; T3c establishes that
after successful reconciliation `knownRevision` equals the
actual PG revision, giving future saver/critical write code
the correct CAS generation — without implementing dirty
queues, snapshot intervals, write-through schedulers, or
shutdown flushes. M8 real trade will later validate
participants, ensure each is reconciled, commit ONE PG
transaction, fence affected owners, and deliver same-OpID
commit notifications; T3c provides that recovery mechanism
without implementing trade validations now.

#### 5.6.8 PG proof fixture

The PostgreSQL integration proof uses the existing
`banks(character_id, system, balance, revision)` root ONLY as
a convenient already-existing revisioned aggregate — not new
bank gameplay, trade semantics, or a money-transfer API, and
no production synthetic bank transfer method is introduced.
The bank root already carries balance, revision, CAS save
semantics, and the existing `GetBank` sqlc query with no child
rows, making it the smallest real §8.1 aggregate with which
to prove that the PG committed revision is authoritative, a
dropped memory notification is detected, and a reload precedes
the next mutation. The store gains one explicit operation,
conceptually `LoadBankBalance(ctx, characterID, system) →
(BankSnapshot, error)`, implemented on `PGStore` with the
existing `gen.GetBank` (no new SQL, no regenerated sqlc, no
migration); the returned `ExpectedRevision` is the persisted
`banks.revision`, making the snapshot immediately CAS-ready,
with no write and no auto-create. A missing row is an error
(wrapping the existing `pgx.ErrNoRows` missing-row
convention): no fallback balance zero, no zero-valued fake
snapshot treated as success. To prove multi-party ordering
without implementing M8 trade, a STORE TEST ONLY may compose
the existing private `saveBankBalance(...)` twice under one
test-owned PG transaction (bank A CAS, bank B CAS, COMMIT);
no production `TransferBanks` / `SyntheticTrade` /
`CommitTwoBanks` API is exposed.

#### 5.6.9 Forced reload without a known newer revision

`MarkCommitted(revision)` covers a KNOWN successful PG commit
revision. A saver stale CAS (§8.3.8) is different: memory and PG
have diverged, but the actual persisted revision may NOT be known
yet. The fence therefore freezes a second primitive,
conceptually `RequireReload()` (exact name flexible):

```text
pending = true
requiredRevision remains at least knownRevision
an existing higher requiredRevision is preserved, never lowered
knownRevision unchanged
no gameplay mutation, no guessed known+1, no overflow arithmetic
```

It is idempotent: repeated calls change nothing further. After
`RequireReload`, `EnsureReconciled` MUST invoke the loader even
when `requiredRevision == knownRevision` (a pure verify/reload is
a valid forced reconciliation: a candidate equal to known may
apply). All other candidate rules stand: below known is
regression, below required is behind. The full saver-side
recovery ordering that consumes this primitive is frozen in
§8.3.13.

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
  semantics (§6.2.2) do not change. Both hard-expiry paths — the
  synchronous on-message expiry gate and the scheduled idle deadline
  callback — use the SAME bounded direct terminal-write budget
  (context.Background() plus the deadline write budget): neither may
  inherit an effectively unbounded request context. Both bypass normal
  queue capacity and share the one physical writer gate (§7.1.7).

#### 7.1.10 Slow-client disconnect and full-resync semantics

After any backpressure/slow-client disconnect, the old session's seq
state, queue contents, coalesced state, and ACK state are discarded; a
reconnect starts a brand-new session and a full baseline (`new WS →
hello → fresh enter_world → fresh full baseline`). Queued frames from
the old socket are never replayed — there is no replay buffer (§6.1).

#### 7.1.11 ACK flow control — frozen for M3-T5b (clarified v0.3.13)

Opcode `125 ack {ackSeq u32}` remains cumulative: the highest S→C
sequence fully applied by the client (v0.3.12 semantics). No replay
buffer exists and no success reply is ever sent for a valid, stale, or
duplicate ACK.

**Session-scoped ephemeral flow state.** Each live session owns exactly
one ephemeral ACK-flow epoch:

```text
flowActive   bool
lastAck      u32
lastFlowSent u32
```

This state is never persisted, never appears as a separate protocol
message, and is discarded with the session on removal/disconnect. No
replay payloads are stored — the epoch is three counters only.

**The flow epoch begins atomically with IN_WORLD.** `CompleteEnterWorld`
validates `CHARACTER_SELECTED` plus its matching binding/index, obtains
the current S→C server sequence — the already physically-written `219
world_ready` sequence — and in ONE registry mutation sets:

```text
State        = IN_WORLD
flowActive   = true
lastAck      = currentServerSeq (the 219 seq)
lastFlowSent = currentServerSeq (the 219 seq)
```

The lifecycle transition and the flow baseline are one atomic
session-registry operation: there is never an observable IN_WORLD
interval with an uninitialized ACK epoch, and no second public
"initialize ACK state" step exists. The epoch does not synthesize a
client ACK; it establishes that the baseline itself is outside the
steady-state lag window. The `219` sequence is captured as-is (never
`worldReadySeq + 1`; no additional sequence is allocated).

**The flow epoch clears atomically on leave.** `CompleteLeaveWorld`
moves `IN_WORLD/bound → AUTHENTICATED/unbound` and in the SAME registry
mutation sets `flowActive = false; lastAck = 0; lastFlowSent = 0`. This
covers both a normal `126 leave_world` and a forced takeover
old-session leave. A later enter starts a completely new epoch at its
new `world_ready` sequence — old ACK debt never survives leave/re-enter
or full resync.

**Begin/abort invariants.** `BeginEnterWorld` starts the
`CHARACTER_SELECTED` phase with `flowActive = false`; a stale active
flow epoch at BeginEnterWorld time is an internal invariant failure
(zero lifecycle mutation). `AbortEnterWorld` returns to AUTHENTICATED
with the flow state inactive and cleared — no partial epoch survives a
failed baseline. Baseline `CHARACTER_SELECTED` traffic is therefore
never ACK-gated: M3 has one synchronous per-connection read loop, so
the client cannot process an inbound ACK concurrently while its
server-side `124 enter_world` handler streams the baseline, and
baseline `217/203/218/220/219` traffic is governed by transport/queue
backpressure only — never by the application ACK window. No baseline
deadlock is possible.

**Normal IN_WORLD sequence reservation is flow-controlled.** For every
NORMAL queued S→C frame, ONE logical registry operation runs while the
physical writer slot is held (atomic against ACK application and
lifecycle transitions):

```text
inspect session state
if not IN_WORLD:
    allocate the normal S→C seq; no ACK-window accounting
if IN_WORLD:
    require flowActive (else internal invariant failure)
    currentLag = serial distance(lastAck → lastFlowSent)
    if currentLag >= max_unacked_messages:
        return the ack_lag slow-client failure;
        DO NOT allocate a sequence; DO NOT write the frame
    allocate the next S→C sequence; lastFlowSent = that sequence;
    the frame may now be physically written
```

Max-unacked semantics: `lag <= max_unacked_messages` is legal. When lag
already equals the configured maximum, the NEXT normal IN_WORLD frame
is rejected as `ack_lag` — no new seq, no write, fail-closed
disconnect, reconnect/full resync as the only recovery. Example with
`max = 2`: baseline leaves lag 0; frame A written → lag 1; frame B
written → lag 2; frame C attempted → ack_lag disconnect and C receives
no sequence and is never written.

**Why ACK accounting happens at physical-write selection.** Messages
that are `queued`, `coalesced-away`, `evicted`, `dropped`, or failed at
admission-timeout never consume ACK state: the flow window advances
only when the writer has SELECTED a NORMAL frame for physical
transmission and is about to allocate its S→C sequence. This preserves
the T5a invariant `no physical frame → no seq → no ACK debt`.

**Direct terminal frames are outside the ACK window.** The two terminal
exceptions of §7.1.9 (`202 kicked`, hard-deadline `202
session_expired`) remain outside ACK-window accounting: they still
allocate the session's normal S→C serial number through the direct
physical writer path, but the session terminates immediately afterward,
so they never create continuing ACK debt.

**Exact ACK classifications (IN_WORLD).** Ordering uses the M2 serial
helpers (`Serial32After`/`Serial32Before`; no rival arithmetic). `0` is
legitimately after `MaxUint32` within the valid serial window:

```text
ack == lastAck                          duplicate  → no-op, no reply
ack serially before lastAck             stale      → no-op, no reply
lastAck < ack <= lastFlowSent (serial)  valid cumulative advance
                                        → lastAck = ack, no reply
ack serially after lastFlowSent         future/invalid → 202 protocol_error
exact 2^31 ambiguous distance           future/invalid → 202 protocol_error
```

The configured window is far below 2³¹, so half-range ambiguity never
occurs during valid operation; an ACK at exactly the half-range
distance is treated as future/invalid, never as stale. The lifecycle
permission table (§6.1) runs before ACK handling — e.g.
`AUTHENTICATED + 125 → 202 bad_state` — and the ACK handler never
duplicates that state logic.

**CHARACTER_SELECTED ACK.** A structurally valid `125` in
`CHARACTER_SELECTED` is a no-op with no reply and no flow-epoch
initialization or mutation; a malformed payload still maps to `202
protocol_error`. This prevents baseline/read-loop deadlocks.

T5b enforces: the number of sent IN_WORLD frames not cumulatively ACKed
MUST stay ≤ `max_unacked_messages`. No payloads are retained. If
sending the next frame would exceed the limit, the session is
classified slow, disconnected/fail-closed, and that next frame is not
written. No replay. Reconnect (new WS + `hello`) ALWAYS performs a full
resync (fresh snapshots); previous seq state is discarded (§7.1.10).

#### 7.1.12 Saturation metrics — frozen for M3-T5b (types clarified v0.3.13)

Exact Prometheus names AND types (no session/account/character labels;
no high-cardinality identifiers):

```text
vox_session_drops_total{reason}           CounterVec
vox_outbound_queue_depth_messages{lane}   HistogramVec
vox_outbound_queue_depth_bytes{lane}      HistogramVec
vox_outbound_state_drops_total{reason}    CounterVec
vox_outbound_state_coalesced_total        Counter
vox_outbound_ack_lag_messages             Histogram
```

**Why depth/lag are histograms.** The T5a observer emits event
snapshots and deliberately carries no session ID. `queue_depth_*` and
`ack_lag_messages` are therefore event-sampled DISTRIBUTIONS, not
per-session gauges: they MUST NOT be implemented as "last session to
update wins" global gauges, and no session label is ever added.

**Queue-depth observation semantics.** Each T5a
`QueueDepth(lane, messages, bytes)` callback adds ONE observation to
the corresponding histogram. The values continue to mean queued lane
depth only — not the currently-active physical write. Resident budget
enforcement (§7.1.2) remains unchanged and DOES include the active
write; metrics and budget accounting intentionally describe different
things (`budget = queued + active write`, `depth histogram = queued
lane depth`).

**Deterministic MVP histogram buckets:**

```text
messages: prometheus.ExponentialBuckets(1, 2, 17)
          → 1 .. 65536 (+Inf), matching the message ceiling
bytes:    prometheus.ExponentialBuckets(1024, 2, 17)
          → 1 KiB .. 64 MiB (+Inf), matching the byte ceiling
ack lag:  prometheus.ExponentialBuckets(1, 2, 21)
          → 1 .. 1,048,576 (+Inf), covering the 1,000,000 window cap
```

Zero-valued observations naturally fall into the first bucket.

**Frozen label values.** Queue-depth lanes are exactly `lane="critical"`
and `lane="state"` — no opcode label, no entity label, no other lane
value. `vox_session_drops_total` reasons are exactly
`critical_queue_saturated`, `reliable_enqueue_timeout`, `write_timeout`,
and `ack_lag`; forced `kicked` and `session_expired` are NOT
backpressure drop reasons for this metric. `vox_outbound_state_drops_total`
reasons are exactly the T5a internal classifications `evicted`,
`saturated`, and `closed`; no opcode/entity/session labels.

**ACK-lag observations.** `vox_outbound_ack_lag_messages` observes the
current lag after: a successful NORMAL IN_WORLD sequence allocation, a
valid cumulative ACK application, a duplicate ACK, a stale ACK, and a
flow-epoch initialization (0). A future/invalid ACK does not mutate
flow state and observes nothing; an ACK-lag rejection observes no
fictional next lag because the rejected frame was not sent, and a frame
whose physical write fails after reservation is not reported as
delivered ACK lag (the session is closing anyway).

**No arbitrary label creation.** The Prometheus adapter whitelists the
frozen `lane`, session-drop-reason, and state-drop-reason values.
Unexpected internal telemetry strings MUST NOT create arbitrary new
label series — ignoring an unknown internal value is preferable to
accidental high-cardinality metric creation. No user-provided
diagnostic string ever becomes a metric label.

#### 7.1.13 Targeted outbound state cancellation (frozen, v0.3.24)

The visibility contract (§7.4.5) requires a non-blocking way to
invalidate ONE queued `205` before admitting its terminal `206`, so a
stale queued `205` can never physically appear after a later `206`
for a retired handle. The existing outbound producer therefore
exposes a narrow operation over the EXISTING per-session outbound
queue — conceptually `CancelState(key)`, exact Go naming may differ.
It is NOT a new queue, NOT a coarse queue flush, and NEVER cancels
unrelated entity state.

Result classification (exact Go names may differ):

```text
Canceled  — a matching queued state item existed; it was removed
InFlight  — the matching item is the current physical in-flight write
NotQueued — neither queued nor writing
Closed    — the queue is closed
```

Semantics:

```text
MUST return immediately
MUST NOT wait for writer ownership
MUST NOT write a socket
MUST use only the outbound queue's existing short mutex
```

A matching queued state item is removed, its message + byte
residency is released, capacity waiters are woken, and normal
post-mutation queue-depth observations MAY be emitted. The
currently-writing item is NEVER cancelled: an in-flight match
returns `InFlight` and is allowed to finish, which is safe because
it already owns physical writer serialization and therefore MUST
physically complete before any later `206` can write. `NotQueued`
means no queued or writing match. `Closed` means the queue is
closed; the recipient already requires resync.

A canceled queued state frame receives no S→C sequence, creates no
ACK debt, and is never physically written, because sequence
allocation still occurs only at physical writer selection (§7.1.8).

Targeted cancellation is intentional semantic invalidation, NOT
backpressure: a successful cancellation MUST NOT increment
`vox_outbound_state_drops_total` and MUST NOT increment
`vox_outbound_state_coalesced_total`. No new Prometheus family and
no new reason label are added; the frozen state-drop reason
whitelist (`evicted`, `saturated`, `closed`) remains unchanged.

### 7.3 Gateway-to-sim ingress and world-presence lifecycle
(frozen, v0.3.22)

T5b1 composes the T5a presence core with the §5.2.10 owner mailbox:
real `102` routing, inbound rate enforcement, and staged
world-presence lifecycle. It sends no AOI fanout (no 204/205/206, no
MovementSink adapter, no 10 Hz throttler) and runs no heartbeat
runtime (no Ping/Pong timers, no stale sweep, no raw-disconnect
cleanup — those are T5b2).

#### 7.3.1 Gateway sim seam

Gateway depends on a narrow structural interface (conceptual; exact
Go names may differ):

```go
type SimIngress interface {
    EnqueueAddEntity(context.Context, world.Vec3) (sim.EntitySnapshot, error)
    EnqueueRemoveEntity(context.Context, sim.EntityID) error
    EnqueueMove(context.Context, sim.EntityID, sim.MoveIntent) (sim.MoveDisposition, error)
    CurrentTick() uint32
}
```

`*sim.Engine` satisfies it. Gateway does NOT expose sim registry
internals.

#### 7.3.2 Gameplay ingress handler

A gateway handler/wrapper (suggested name `GameplayIngressHandler`)
owns `102` transport/routing plus the general inbound rate gate for
`103..120` only — it implements NO gameplay semantics for
`103..120`. It requires `PresenceRegistry`, `SimIngress`, and an
injected `NowFunc` wall clock (never a hidden `time.Now`, never the
sim tick, never the header tick); `Next` may be nil; a nil required
dependency fails construction.

Chaining: `102` is consumed completely. `103..120` charge the
general intent bucket and, if allowed, delegate the UNCHANGED
header/decoder/send to `Next`. All other opcodes delegate unchanged.
Opcode ownership: `100/101` Server, `121/122/123/126`
CharacterHandler, `124` EnterWorldHandler, `125` Server ACK, `102`
T5b1 transport into M4-T2 movement semantics, `103..120` future
gameplay owners. M4-T2 remains the sole owner of authoritative
movement rules; M4-T5b2 owns `205` AOI transport fanout.

For `102` the order is fixed: `DecodeMove` → resolve active
Presence → charge the movement bucket → build `sim.MoveIntent` →
`EnqueueMove` → map disposition/error. A malformed `102` yields
`202 protocol_error` with no movement token consumed and no sim
submission. The conversion is exact:

```go
sim.MoveIntent{
    InputSeq:   move.InputSeq,
    HeldDirs:   move.HeldDirs,
    RunFlag:    move.RunFlag,
    Yaw:        move.Yaw,
    SampleTick: header.Tick,
}
```

Never use C→S header `seq` as `InputSeq`; never overwrite
`SampleTick` with the server tick; never accept client positions.
Routing uses the active `PresenceSnapshot`'s controlled
`sim.EntityID` — never a characterID cast, NetEntityID cast, or
global client-data lookup. A missing presence for an allowed
IN_WORLD `102` is an INTERNAL invariant failure (not
`invalid_handle`, not a silent no-op).

Every structurally valid `102` consumes one movement token BEFORE
sim submission — accepted, duplicate, stale, future-tick-invalid,
RFC1982-ambiguous, migration-full, and ingress-full alike. Only
malformed payloads fail before charging, so semantically bad but
structurally valid requests cannot bypass ingress limits. Denial
yields `202 error{rate_limited}` (numeric 11) with no `EnqueueMove`;
the connection stays alive.

For `103..120` T5b1 charges `PresenceRegistry.AllowIntent` WITHOUT
decoding them (owning M5/M7/M8 tasks keep semantic handling):
denied → `202 rate_limited` with `Next` never invoked; allowed →
delegate the original decoder unchanged. `100/101/121..126` are
never charged, so rate limiting cannot block reauth, ACK, or
leave_world.

Result mappings: `MoveAccepted`/`MoveDuplicate`/`MoveStale` are all
silent (no success reply, connection alive; duplicate/stale stay
normal no-ops). `sim.ErrMigrationQueueFull` → `202 retry` (numeric
6; the same `inputSeq` stays retryable per zero-mutation guarantee;
ordinary migrating state is NOT retry). `sim.ErrSimIngressFull` →
`202 retry` (distinct gateway-to-owner overload domain).
`ErrEngineNotRunning`/`ErrEngineStopped` → `202 retry` as
operational availability failures while the connection stays valid.
`ErrInvalidMoveYaw`/`ErrFutureInputTick`/`ErrAmbiguousInputSeq`/
`ErrAmbiguousSampleTick` → `202 protocol_error` with no disconnect.
`sim.ErrEntityNotFound` for an IN_WORLD presence's EntityID means
gateway/sim diverged: internal fail-closed, never `invalid_handle`.

#### 7.3.3 Spawn seam

The staged entry needs an authoritative initial position, but
`character.Descriptor` carries no complete persisted position and
M10 has no world source yet. No Store import, no silent `{0,0,0}`.
The narrow injected seam (conceptual):

```go
type SpawnResolver interface {
    ResolveSpawn(ctx context.Context, accountID, characterID int64) (world.Vec3, error)
}
```

Tests use deterministic fakes; future real world/durable
composition supplies it. The resolver decides ONLY the initial
`world.Vec3` — it never adds the entity, binds presence, sends a
baseline, or changes session state. No Store/query/schema changes.

#### 7.3.4 World session runtime

One gateway world-session runtime (suggested name
`WorldSessionRuntime`) composes `SimIngress`, `PresenceRegistry`,
`SpawnResolver`, `NowFunc`, and the existing downstream `WorldExit`
quiesce/flush seam. EnterWorldHandler needs a staged entry
interface (conceptual):

```go
type WorldEnter interface {
    PrepareEnter(ctx context.Context, sid session.ID, accountID, characterID int64) error
    CommitEnter(sid session.ID) error
    AbortEnter(ctx context.Context, sid session.ID) error
}
```

The SAME concrete runtime also implements the existing `WorldExit`,
so normal leave and takeover share one composition.

`PrepareEnter` stages but does NOT activate Presence. The runtime
retains bounded ephemeral pending metadata (`sid`, `accountID`,
`characterID`, `entityID`, spawn/current cell) — at most one pending
entry per session, no history, no persisted table. The pending map
may use a small mutex that is NEVER held across `SpawnResolver`
calls, sim `Enqueue*`, downstream `WorldExit` calls, socket writes,
or `PresenceRegistry` calls. Binding order: reserve the pending
slot → `ResolveSpawn` → `EnqueueAddEntity` through the sim owner →
retain the returned `EntityID` + authoritative cell. No 204/205/206
is sent.

Prepare failure: spawn resolution failure removes the reservation
(no entity, no presence); sim-add failure before mutation likewise;
invalid trusted spawn rejected by `AddEntity` is an internal
trusted-data/world invariant (never silently rewritten);
operational resolver/engine-unavailable/ingress-saturation failures
are world-entry retry classifications (stable errors, no message
parsing). A second `PrepareEnter` for the same sid while pending is
a stable invariant/conflict error with no second entity.
`AbortEnter` after a prepared entry does `EnqueueRemoveEntity` then
drops pending metadata (no Presence existed, so none is
deactivated); if sim removal fails before mutation, pending state
is retained for retry/cleanup; no prepared entry → nil (frozen
idempotent choice for rollback simplicity).

`CommitEnter` runs only after the physical `219` write AND
`CompleteEnterWorld`, then `PresenceRegistry.Activate(sid,
characterID, entityID, cell, Now())` and drops pending metadata.
Buckets therefore start at COMMIT time, not prepare time. No wire
message is emitted. This ordering is safe because the WebSocket read
loop handles one message synchronously: while the enter_world
handler still runs, the same client cannot dispatch a `102` through
that loop — so `219` → `CompleteEnterWorld` → Presence commit →
handler return creates no playable gap, and a baseline failure
never leaves an active Presence.

T5b1 does NOT rewrite the `203`/`218`/`220` baseline: M10-T4 owns
the real sim/world baseline. Integration tests combining real sim
entry with the existing M3 `BaselineProvider` use an EMPTY entity
baseline. T5b2 adds post-world-ready incremental live visibility;
M10-T4 later unifies the real baseline with the same Presence
handle namespace. Never duplicate fake baseline entity IDs.

#### 7.3.5 EnterWorldHandler final ordering

Takeover arbitration stays before new world staging. Successful
order for the new session: account guard acquired → character
lookup → old-session takeover/`WorldExit` if needed →
`BeginEnterWorld` → `WorldEnter.PrepareEnter` → `217 enter OK` →
`BaselineProvider.StreamBaseline` → `219 world_ready` physically
written → `CompleteEnterWorld` → `WorldEnter.CommitEnter` →
account guard released → handler returns. If `PrepareEnter` cannot
stage the entity: `AbortEnterWorld` + `202 retry` BEFORE any
successful `217` — the client never hears enter OK without a
staged runtime entity.

After `BeginEnterWorld`, every pre-commit failure cleans BOTH the
staged world entry and the session binding via one explicit
rollback helper (no rollback goroutine): `217` write failure →
`AbortEnter` + `AbortEnterWorld` + original error, connection
terminates; operational baseline failure → both aborts + `202
retry` (baseline write failure → both aborts + write failure,
connection terminates); `219` write failure → both aborts, no
Presence; `CompleteEnterWorld` failure after a physically written
`219` → `AbortEnter` + `AbortEnterWorld` where still applicable,
fail closed with NO contradictory retry sequence (the client may
have seen `219`; reconnect/full baseline recovers);
`CommitEnter` failure after completion → `AbortEnter` +
`CompleteLeaveWorld` if the session still owns the just-completed
IN_WORLD binding, fail closed, no enter success. Presence
activation conflicts (`ErrSessionAlreadyPresent`,
`ErrCharacterAlreadyPresent`,
`ErrControlledEntityAlreadyPresent`, `ErrAOICellRange` on trusted
spawn) at commit are internal invariants — fail closed and clean
up, never `character_in_use` (takeover arbitration already settled
ownership).

#### 7.3.6 Runtime ExitWorld

The runtime's `WorldExit` for a healthy active presence: verify
sid/account/character match the active Presence → call the existing
downstream `WorldExit` quiesce/flush seam → on failure STOP and
return (sim entity + Presence intact) → `EnqueueRemoveEntity`
through the sim owner → on pre-mutation failure return with
Presence intact → `PresenceRegistry.Deactivate` → nil. The
flush/quiesce barrier stays FIRST: if downstream fails, the old
session remains IN_WORLD with binding, presence, and entity, and
replacement does not proceed. For normal success: remove sim
entity, then deactivate Presence, then the existing caller runs
`CompleteLeaveWorld`. Presence-maps-to-X-but-sim-says-missing is
an internal fail-closed invariant, never silent success. Raw
transport disconnect / heartbeat-timeout / stale-sweep cleanup is
explicitly T5b2: after T5b1, enter/leave/takeover lifecycles are
implemented while raw disconnect cleanup is not. No Ping/Pong, no
Server-teardown changes here.

#### 7.3.7 T5b1 non-scope (binding)

No `cmd/serve.go` bootstrap (stays a stub: no PG/Keycloak,
WorldSource, supervision, or SIGTERM work). No new config values
(existing `VOX_RATE_MOVE_PER_SEC`/`VOX_RATE_INTENT_PER_SEC` only;
ingress capacity is a frozen internal constant). No protocol
changes (`proto` and `testdata/protocol` unchanged; the existing
102 codec suffices). `outbound.go` unchanged. T5b1 sends no
204/205/206 and implements no MovementSink adapter, viewer index,
presentation lookup, fanout throttler, heartbeat, or sweep.

### 7.4 AOI fanout and transport liveness (frozen, v0.3.23; closure corrected v0.3.24)

T5b2 completes the M4 runtime: the Presence viewer reverse index,
one bounded fanout pump consuming authoritative `sim.MovementUpdate`
events, post-world-ready `204` bootstrap with a readiness barrier,
live AOI reconciliation with recipient-local handles, `205`
movement fanout capped at 10 Hz per recipient/entity, and
flush-first raw-disconnect/heartbeat teardown. The sim is never
throttled; T5b1 ingress, movement semantics, and the outbound queue
are reused unchanged.

#### 7.4.1 Presence viewer reverse index

PresenceRegistry gains the gateway-owned ephemeral reverse index
`entity -> currently-visible session IDs` alongside the existing
per-session `entity -> NetEntityID` / `NetEntityID -> entity`
tables, all mutated atomically under the SAME registry lock:
Activate adds the owner sid as viewer of its own EntityID (the owner
is therefore a viewer of its own entity and receives reconciliation
205s); EnsureVisible adds sid on each new mapping; HideVisible
removes sid; Deactivate removes sid from EVERY entity it sees.
Empty reverse sets are deleted; no ghost viewer survives
Deactivate. New read-only APIs (exact names flexible) —
`Viewers(entity)`, `VisibleEntities(sid)`, `VisibleHandle(sid,
entity)`, `Controller(entity)` — return sorted numerical copies;
no internal map escapes. Every 204/205/206 uses the recipient
session's current NetEntityID; `uint32(netID)` conversion happens
ONLY at the protocol encoder boundary, never derived from
sim.EntityID, characterID, or array/cell indexes.

#### 7.4.2 Entity presentation and wire positions

Kind/Proto/current presentation come from a narrow injected HOT
IN-MEMORY seam (`EntityPresentation` + `EntityPresentationSource`
with `Entity(entity)` and `EntitiesInCell(cell)`; race-safe,
non-blocking, non-PG). `EntitiesInCell` returns unique EntityIDs
sorted ascending whose positions actually map to that cell, as
copies. There is NO production fallback kind/proto (no `0`, no
`player`, no characterID mapping): missing presentation is an
internal world/presentation invariant, and T5b2 never queries Store
or PG for fanout. M9/M10 later supply real content. The single
frozen gateway conversion is `world.Vec3` float64 meters to
`proto.Position` int32 millimeters via `wireMM =
math.Round(meters * 1000)` per axis — no truncation, saturation, or
wrap; coordinates that do not fit int32 (including NaN/±Inf) are a
stable internal error (e.g. `ErrWirePositionRange`) that fails the
affected session closed/resync, never clamping authoritative state.

#### 7.4.3 Fanout runtime and event queue

One gateway `FanoutRuntime` implements `sim.MovementSink` and owns
visibility transport composition (no gameplay or movement rules).
Exactly one bounded event queue with `FanoutEventCapacity = 1024`,
one fanout pump goroutine, no per-event goroutine, no worker pool,
no arbitrary `func(*FanoutRuntime)` callback events: the private
typed union carries movement updates, bootstrap-session controls,
and remove-presence controls. `OnMovement` admits non-blockingly
from the sim owner goroutine (free slot admits, full drops the
NEWEST update immediately, closed drops) and never waits, sleeps,
spins, writes sockets, or takes Presence locks/calls
PresentationSource. A dropped movement event alters no sim,
disconnects nobody, allocates no seq/handle; a later authoritative
update or reconnect/baseline corrects state. Drops keep a bounded
atomic diagnostic count (e.g. `DroppedMovementUpdates() uint64`)
with low-rate sampled logging (`queue_full`/`closed`), but T5b2
adds NO Prometheus series and never logs every flooded packet.
Bootstrap/Remove controls are reliable: callers may wait a bounded
`FanoutControlTimeout = 1s` admission budget (internal constant, no
config); BEFORE successful publication, context cancellation /
admission timeout / Close may reject; AFTER successful publication
the event is authoritative — the caller waits ONLY for that event's
cap-1 completion signal (no waiter goroutine) and a later Close
never changes that event's definitive result. The pump/drain
remains responsible for producing exactly one result for every
admitted control. One queue is the visibility barrier: controls
order after earlier movements and before later ones, so no 205
precedes its bootstrap 204 and no stale 205 follows a 206. Shutdown
is idempotent: movements discarded, queued-but-never-admitted
control waiters receive `ErrFanoutClosed`, future movement dropped,
future control rejected, no send-on-closed panic, no restart
required. No control may be published after the closing runtime has
completed its final drain: admission holds a short admission-vs-close
synchronization (recommended: a small `sync.RWMutex` with a `closed`
flag used ONLY around event admission/Close transition —
admission takes read ownership, checks closed, waits up to
`FanoutControlTimeout` for queue publication, releases after
publication, then waits only for event completion; Close takes
write ownership, marks closed, closes done, releases, then waits
for the pump — so Close waits at most the already-frozen bounded
control-admission interval and concurrent admissions still share
read ownership instead of serializing behind one 1-second mutex
wait). `OnMovement` stays NON-BLOCKING and MUST NOT acquire that
gate in a waiting mode (use `TryRLock` or another race-free
immediate mechanism): runtime open + immediate admission
ownership → existing select/default queue send; closing/closed or
gate unavailable → drop immediately counted as closed; queue full
→ drop immediately counted as queue_full. After `Close()`
completes, no future movement event may be left stranded in the
event channel.

#### 7.4.4 Readiness and bootstrap

Presence activation is NOT visibility readiness: the pump owns an
ephemeral ready-session set, and a committed Presence starts NOT
ready (its AOI subscriptions exist, but the M10 baseline may carry
zero live entities, so the client may not even know its own handle
1). Until BootstrapSession completes, movement fanout ignores that
session — otherwise 205 could precede 204. Successful enter order
is 217, baseline, physical 219, CompleteEnterWorld, Presence
Activate, `Fanout.BootstrapSession`, ready=true, handler return;
bootstrap 204s are ordinary post-IN_WORLD traffic under outbound
budgets, ACK flow, and slow-client handling (not baseline
exemptions). The staged interface may therefore take bootstrap
context (`CommitEnter(ctx, sid)`); WorldSessionRuntime owns the
composition with no second public bootstrap step. Bootstrap reads
the PresenceSnapshot, unions PresentationSource over the 49 cells
in canonical order plus the mandatory own entity (missing own
presentation is internal), and emits `204 entity_create` (never
203/218/220; M10-T4 still owns the real baseline) in EntityID
ascending order — own first only if naturally first; own keeps
pinned handle 1 and still gets its 204 because the client has not
seen the mapping. Non-owned entities use EnsureVisible handles
(new allocations use that exact handle). 204 is reliable
`TryCritical` traffic, never TryState/SendCritical/raw writes, and
the pump never waits for physical completion. Joining-session
bootstrap failure (missing presentation, wire range, handle
exhaustion, critical saturation, closed outbound, other invariant)
fails that session closed with ready=false and a BootstrapSession
error; WorldSessionRuntime rolls back Presence + staged entity
through the post-219 path with no contradictory 202, and no
compensating 206s are sent to the dying socket. Only AFTER the
joining bootstrap succeeds is the new controlled entity exposed to
other ready AOI subscribers (EnsureVisible + 204 each); one
existing viewer's failure closes/resyncs only that viewer, never
the joined source. Non-ready subscribers receive nothing.

#### 7.4.5 Movement fanout

Each MovementUpdate derives its authoritative cell from
`world.CellForPosition(update.Position)` (invalid = internal
invariant; no client data). The controlled-entity reverse index
(`EntityID -> owner sid`, no scans, no casts) routes ownership: a
cell change runs `UpdateCenter(owner, newCell)` BEFORE new viewer
computation so owner AOI follows authoritative position, while a
same-cell move causes no subscription churn. The owner's full
visible set then reconciles against the NEW 49-cell desired set
(desired-but-invisible → 204, visible-but-undesired → 206 with the
OLD handle captured before `HideVisible`, intersection retained,
own always retained; cell crossings are 32 m-apart, so full-set
rebuild stays deterministic and PG-free). Movement-driven viewer
reconciliation uses ready Subscribers(newCell) vs ready
Viewers(entity) with the same 204/206/retain rules (owner included
via its center update). The update that creates visibility sends NO
redundant 205 — the 204 already carries authoritative
position/yaw/speed. New 204s use PresentationSource Kind/Proto
with MovementUpdate.Position/Yaw/Speed (never older snapshots);
missing presentation for a required create is an internal
invariant — fail the recipient closed, never 205 without 204. All
set operations are deterministic (session IDs / EntityIDs
ascending, cells canonical); every frame is encoded per-recipient
with that session's handle (same entity may be 4 to A and 17 to B);
Presence locks are never held across TryCritical/TryState/source
calls; one pump serializes ready/throttle/ordering state.

204 uses TryCritical (visibility transitions are reliable); every
visibility-retiring 206 MUST first invalidate the queued predecessor
state, then admit, then retire — exact order with the OLD handle
captured before `HideVisible` (frozen v0.3.24):

```text
capture old NetEntityID H
key = StateKey{ Kind: OpcodeEntityMove, ID: uint64(H) }
CancelState(key)
TryCritical(206 using H)
HideVisible(...)
clear throttle epoch
```

Interpretation: `Canceled` → the stale queued 205 is gone and the
206 may proceed; `InFlight` → that 205 already owns physical writer
serialization and MUST physically complete before the later 206 can
write, so the 206 may proceed; `NotQueued` → the 206 may proceed;
`Closed` → retire the mapping because the recipient already
requires resync. Because one FanoutRuntime pump is the sole M4
producer of 205 visibility state for that recipient/entity, no new
same-key 205 can race between cancellation and 206 admission. No
coarse queue flush; no cancellation of unrelated entity state.
Critical failure of the 206 still retires — the recipient is
already failed closed and reconnects/resyncs; never preserve a
stale handle for a dying socket. `204` ordering is unchanged
(v0.3.24): `204` is critical and any later `205` is state, so
critical-first scheduling already guarantees a `204` cannot be
overtaken by a later `205` — only the terminal `205 → 206`
direction needs the targeted invalidation above. New-handle 204 admission failure retires the handle via
HideVisible (never reused) and fails the recipient; own-handle
failure closes/resyncs the owner without HideVisible(own). 205 is
coalescible state via `TryState(sid, StateKey{Kind:
OpcodeEntityMove, ID: uint64(recipientNetID)}, ...)` — never
sim.EntityID/characterID keys. `StateDropped` changes nothing
(session alive, mapping kept, later movement corrects; metrics
record it); `StateClosed` stops targeting that recipient (transport
cleanup owns Presence removal). 205 carries the converted
authoritative position, update yaw/speed, and
`LastProcessedInputSeq` equal to the update anchor for the
CONTROLLING session but 0 for every other viewer. MovementUpdate
Position/Yaw/Speed/Tick always come from the sim event, not the
presentation snapshot. 205 emission is capped at
`MovementFanoutMaxHz = 10` per recipient/entity visibility epoch
via tick stride `max(1, (tickHz+9)/10)` (20→2, 60→6, 120→12,
7→1; no wall clock): first-205-allowed state per epoch, then
`currentTick - lastSentTick >= stride` in wrap-safe modulo-u32
arithmetic. A 204 does not count as a 205 (first later update may
still 205); epochs reset on handle retirement; the owner obeys the
same 10 Hz cap with cumulative anchors across skipped ticks.

#### 7.4.6 Lifecycle with fanout

`WorldSessionRuntime` requires the same `FanoutLifecycle`
(BootstrapSession/RemovePresence; no second instance; recording
fakes where fanout is not under test). `CommitEnter(ctx, sid)`
activates Presence (still after 219 + CompleteEnterWorld) then
bootstraps; bootstrap failure deactivates Presence, clears
source-session fanout bookkeeping with ready=false, retains staged
entity metadata, and returns — the existing post-219 path then
aborts the entity and rolls back the binding with the connection
failed closed. Existing viewers learn a new entity only after its
source bootstrap succeeds, so failed joins pollute nobody. Normal
exit is verify-Presence → downstream flush → sim remove →
`Fanout.RemovePresence` → Presence deactivate → caller
CompleteLeaveWorld, with the durable barrier still first:
downstream failure changes nothing (no 206, retry); pre-mutation
sim-remove failure skips fanout entirely (retry-safe). RemovePresence
is ordered after earlier movements: the source goes not-ready,
other ready viewers get 206 with mapping retirement, the source
gets no own-206 and its throttle clears. No future MovementUpdate
can exist after sim removal, so no stale 205 follows. The healthy
queued RemovePresence path remains the SOLE normal cleanup owner:
source not-ready, source/entity throttle cleared, 206 to ready
observers, old observer handles retired, source ready cleared.

Fanout local-state locking (v0.3.24): the normal pump remains the
sole DECISION executor of visibility, but the pump-only `ready` and
`throttle` maps gain ONE short fanout-local metadata mutex because
the emergency primitive below may run outside the pump. The mutex
protects ONLY `ready` and `throttle` and is NEVER held across
Presence calls, PresentationSource calls, TryCritical/TryState,
socket close, or event admission waits — no broad fanout lock.

Emergency fanout-state invalidation (v0.3.24): the fanout runtime
exposes a narrow synchronous local-state primitive — conceptually
`ForgetSession(sid)` or `FailClosedSession(sid)`, exact naming
flexible — that is idempotent, performs NO socket I/O, NO Presence
operation, NO PresentationSource call, NO event enqueue, removes
`ready[sid]`, removes every throttle entry whose recipient sid ==
sid, and works even while the fanout runtime is closing/closed. It
does NOT replace the normal queued RemovePresence path.

If sim removal SUCCEEDED but the reliable `Fanout.RemovePresence`
FAILED before cleanup could complete, the `WorldSessionRuntime`
fallback MUST snapshot currently indexed viewers and, for each
affected viewer including the source, fail/CloseNow the transport
(as already required) plus call the emergency invalidation for
that viewer sid; for each non-source viewer it additionally retires
the removed entity's Presence mapping; then it continues with
source `Presence.Deactivate`. After the fallback no affected
closed session retains `ready=true`, 205 throttle metadata, or an
addressable removed-entity handle. The removed sim entity is never
resurrected. Takeover order is
old flush → old sim remove → old fanout remove → old deactivate →
old CompleteLeaveWorld → old kick → new stage/219/Presence/fresh
bootstrap epoch; old handles never transfer, and flush/remove
failure leaves old fanout/presence/binding unkicked with the new
bootstrap never begun.

#### 7.4.7 Raw disconnect and heartbeat

ServeHTTP teardown replaces unconditional `Registry.Remove` with
reaper-aware cleanup. One narrow `SessionReaper{Registry, Presence,
WorldExit}` (same WorldSessionRuntime composition as
leave/takeover; never normal-126 handling) owns unexpected
disconnect, stale cleanup, and detached-session retry: stop
outbound, guard on `LockAccount` for authenticated sessions with
re-read serialization (safe against concurrent leave/takeover/
delete/enter/read-error/sweep/kick causes; no double destructive
WorldExit, no panic), then for no-Presence sessions (CONNECTED,
AUTHENTICATED, rolled-back SELECTED) plain `Registry.Remove`; for
still-owned IN_WORLD + character + active Presence, `WorldExit`
over a fresh `context.WithTimeout(Background,
DisconnectCleanupTimeout = 5s)` (never the dead request context).
Successful raw cleanup order is stop outbound, cancel auth timer,
guard, WorldExit, CompleteLeaveWorld, Remove, release. WorldExit
failure before destructive cleanup retains session binding,
Presence, entity, and fanout with the dead transport for the next
sweep or legitimate takeover — never orphaning world state. If
CompleteLeaveWorld then fails with world already gone, force
Remove and log the invariant. Heartbeat preserves Ping/Pong 15 s,
`PresenceHeartbeatTimeout` 30 s, 30 s sweep cadence, plus bounded
`HeartbeatPingTimeout = 15s` (no config fields). A gateway-local
`Pinger{ Ping(ctx) error }` seam (NOT on session.Connection)
backs `wsConnection.Ping` via coder/websocket v1.8.15, whose
control frames bypass opcode/seq/tick/ACK/queue/writer-gate. One
ping loop per accepted WebSocket, ending with the connection: each
15 s pulse pings only WITH active Presence (pre-world baseline
never pings); Pong success touches heartbeat at injected Now
(application frames do not count; auth TokenExp/deadline untouched;
rate buckets untouched); ping failure CloseNows without touching,
leaving authoritative cleanup to the reaper; post-deactivation
TouchHeartbeat-not-found is normal convergence. The 30 s sweep uses
sorted `StaleSessions(Now())`: CloseNow live conns, reap each,
retain-then-retry on WorldExit failure. Presence without a session
entry is a loud internal invariant. One `TransportLiveness`
(Presence + Reaper + NowFunc + ticker factory) owns the sweep AND
every started per-connection Ping loop strongly enough that
`Close()` can stop them (v0.3.24; exact representation flexible —
e.g. a `pingers` map of idempotent `{stop, done}` loop handles).
`StartPinger` under the existing liveness mutex: if closed, start
NO ticker, start NO goroutine, and return an idempotent no-op
stop; otherwise register the loop, account for it before releasing
the mutex, and start exactly one goroutine. A session never has
two simultaneously registered Ping loops; a completed/stopped loop
removes itself from the registry safely. `Close()` marks liveness
closed atomically under the mutex, stops the stale-sweep loop,
snapshots/stops EVERY active registered Ping loop, prevents any
future `StartPinger` from creating another loop, then waits for
the sweep + all Ping loops to exit, and returns — without
requiring the WebSocket transport itself to close first, and
without reaping/closing the active sessions merely to stop
liveness. The per-connection returned stop and global `Close()`
may race; both are idempotent with no close-of-closed-channel,
no WaitGroup misuse, no double ticker-stop panic, and no goroutine
leak. After global Close begins, no positive WaitGroup Add may
race a zero-count Wait: mutex/lifecycle ordering prevents
`StartPinger` from adding after closed (splitting sweep and pinger
wait groups is allowed). Server takes it as optional/explicit composition
(nil keeps frame/auth/ACK tests world-free), gains idempotent
`Close()` for the sweep goroutine, stops the per-conn ping loop in
teardown, and takeover CloseNow ends the old ping loop (heartbeat
keyed by old sid cannot touch the replacement).

#### 7.4.8 T5b2 non-scope (binding)

No `internal/proto` or `testdata/protocol` changes (existing
204/205/206 layouts exactly); no store/persist/migrations/queries
changes (in-memory injected presentation); no config changes
(constants are `FanoutEventCapacity` 1024, `FanoutControlTimeout`
1 s, `MovementFanoutMaxHz` 10, `HeartbeatPingTimeout` 15 s,
`DisconnectCleanupTimeout` 5 s); no `cmd/serve.go` bootstrap
(Keycloak/PG/WorldSource/SIGTERM stay M10–M12); no new
dependencies; no new Prometheus family (existing metrics only);
no M5 gameplay; no M10 real-baseline replacement (204 bootstrap
bridges to M10-T4); `outbound.go` gains ONLY the narrow v0.3.24
targeted state-cancellation operation (§7.1.13) — no second queue,
no new metric, no seq/ACK change.

T5a exposes only a narrow no-op-by-default observer seam — sufficient
to observe queue depth messages/bytes per lane, state dropped, state
coalesced, session slow-drop reason, and (T5b) current ACK lag — so T5b
attaches Prometheus without rewriting queue internals; the core queue
does not import Prometheus.

The internal-delivery rule is unchanged in spirit: sim→gateway channels
are bounded with defined overload behavior (shed newest movement first,
never critical events; count + log). Cell-owner goroutines MUST NOT
block indefinitely on gateway delivery; critical-lane saturation fails
closed by disconnecting/resyncing the affected sessions rather than
dropping the event or stalling the simulation. Unbounded queues are a
spec violation.

### 7.2 Presence, AOI subscriptions, handles, and inbound limits
(frozen, v0.3.21)

This section freezes the gateway-owned ephemeral world-session core
(M4-T5a). It is transport-independent infrastructure: registry/index
primitives only. Real gateway↔sim runtime integration (movement
ingress, Ping/Pong scheduling, 204/205/206 fanout, lifecycle hooks)
belongs to M4-T5b, which composes these primitives without
reimplementing them.

#### 7.2.1 Gateway ownership boundary

Sessions, presence, visibility handles, AOI subscriptions, heartbeat
state, and inbound rate limits are gateway-owned ephemeral state. The
sim owns `EntityID`, authoritative positions/cells, and
movement/gameplay rules. The gateway owns `SessionID`,
`CharacterID ↔ active world session` binding, session-local
`NetEntityID`, which cells each session subscribes to, which entities
each client currently knows, rate-limit state, heartbeat state, and
wire fanout. The gateway MUST NOT implement movement/gameplay rules.

#### 7.2.2 Presence vs session registry

The existing `session.Registry` remains authoritative for session
lifecycle, connection, sub/account/character binding, and `IN_WORLD`
state. Presence does NOT duplicate `session.Connection`, `TokenExp`,
auth state, or account state. Although older §7 prose describes
presence as `charID → {conn, currentCells, heartbeatAt}`, the
implementation-normalized shape is frozen as
`charID → {sessionID, entityID, AOI cells, heartbeatAt, visibility,
rate state}`. Connection lookup remains through `session.Registry`;
there is no torn duplicate connection ownership.

#### 7.2.3 Presence epoch

A presence record exists only for one active world-presence epoch.
Conceptually:

```go
type PresenceSnapshot struct {
    SessionID   session.ID
    CharacterID int64
    EntityID    sim.EntityID
    CenterCell  world.CellCoord
    Cells       []world.CellCoord
    HeartbeatAt time.Time
    OwnNetID    NetEntityID
}
```

Exact Go names may differ. Snapshots are immutable copies: mutating a
returned slice MUST NOT mutate registry state, and no API returns a
mutable internal map, slice backing array, handle-table pointer, or
token-bucket pointer.

#### 7.2.4 Activation identity and atomicity

Activation requires `SessionID != 0`, `CharacterID > 0`,
`EntityID != 0`, and a valid center `CellCoord`. Malformed identity is
rejected before mutation with stable `errors.Is`-compatible errors.
While active, at most one presence exists per `SessionID`, per
`CharacterID`, and per controlled `sim.EntityID`; the three indexes
are maintained atomically under one registry lock. A conflict is an
error with zero partial mutation — the registry MUST NOT silently
replace another presence. M4-T5b owns lifecycle/takeover ordering
before activation. `Activate` is only the ephemeral registry
primitive: it does NOT change `session.Registry` state, add a sim
entity, load PG, send a baseline, send `world_ready`, or write a
WebSocket.

#### 7.2.5 Deactivation

`Deactivate(sessionID)` atomically removes the session, character,
and controlled-`EntityID` index entries, all AOI subscriptions, all
session-local visibility mappings, heartbeat state, and rate-limit
state. It returns/copies enough prior state for tests or later
cleanup if useful. Unknown sessions report a stable
`ErrPresenceNotFound` (frozen choice: explicit not-found, never a
silent divergent no-op in one path and an error in another). No
partially retained subscriber entries may survive.

A new world-presence epoch gets a fresh `NetEntityID` namespace and
fresh rate-limit buckets: handles from an old/disconnected/replaced
session NEVER carry into the new session, and rate state is never
persisted across reconnect/restart (accepted MVP ephemeral-state
tradeoff).

#### 7.2.6 Base AOI rule

The §4 MVP AOI is frozen as `CellSizeMeters = 32`,
`DefaultAOIRadiusMeters = 96`, cell radius 3. The T5a base AOI is the
Chebyshev 3-cell neighborhood around the center cell
(`dx = -3..+3`, `dz = -3..+3`): exactly 7×7 = 49 cells. This matches
the existing "3-cell radius" language. `INTERIOR` volumes, portal AOI
remaps, classic/procedural world sources, and terrain visibility are
NOT implemented here (M10 owns those); T5b/M10 may later provide an
AOI policy override while preserving the same subscription-registry
API, for which the exact-49 base policy keeps portal/remap
composition possible.

All returned AOI cell slices use canonical ordering (`CellCoord.X`
ascending, then `CellCoord.Z` ascending — never Go map order). For
center `{0,0}`: first `{-3,-3}`, last `{3,3}`, length 49. Negative
coordinates behave identically. A center within 3 cells of
`math.MaxInt32`/`math.MinInt32` cannot represent the full AOI: the
registry MUST NOT wrap coordinates and MUST return a stable
`ErrAOICellRange` (or equivalent) from activation/update with zero
registry mutation, even though real world coordinates should never
approach it.

#### 7.2.7 Subscriptions and deltas

The registry maintains both directions atomically:
`session → subscribed cells` and `cell → subscribed session IDs`,
with no duplicate membership. Conceptually:

```go
UpdateCenter(sid session.ID, center world.CellCoord) (SubscriptionDelta, error)

type SubscriptionDelta struct {
    Entered []world.CellCoord
    Exited  []world.CellCoord
}
```

The session's full new cell set is the 49-cell base AOI around
`center`. When the center does not change, both lists are empty and
the reverse index is untouched (no churn). Churn reference points
(mandatory tests): `{0,0} → {1,0}` yields exactly 7 entered / 7
exited (42 overlap); `{0,0} → {1,1}` yields exactly 13 entered / 13
exited (36 overlap); a sufficiently distant jump yields 49 entered /
49 exited. `Entered` and `Exited` are individually canonical
(X ascending, Z ascending).

`Subscribers(cell)` (or equivalent) returns unique live presence
session IDs sorted numerically ascending, as a copy: no mutable
subscriber map escapes, and mutation of the result cannot affect the
registry.

#### 7.2.8 Session-local NetEntityID handles

The gateway owns:

```go
type NetEntityID uint32
```

with `0` invalid/reserved. Handles are session-local only, never
persisted, never equal-by-definition to `sim.EntityID`, and never
obtained by `uint32(simEntityID)` truncation. Protocol codecs continue
to use raw `uint32`; T5b converts `NetEntityID` to `uint32` only at
the wire boundary. At presence activation the controlled
`sim.EntityID` is bound immediately to `NetEntityID(1)`, pinned for
the lifetime of that presence epoch (AOI churn never removes it).
Subsequent visible entities receive 2, 3, 4, … per session,
monotonically, with no reuse during one presence/session lifetime:
when a non-owned entity leaves visibility its forward and reverse
mappings are removed but its numeric handle is retired forever, so a
later re-entry allocates a NEW larger handle. This prevents a delayed
old packet from aliasing a new visibility epoch.

`MaxUint32` MAY be allocated once; the next required allocation
returns a stable `ErrNetEntityIDExhausted` (or equivalent) — never
wrap to 0, never reuse. T5b fails/resyncs the affected session rather
than inventing an alias.

Conceptually:

```go
EnsureVisible(sid session.ID, entity sim.EntityID) (net NetEntityID, created bool, err error)
HideVisible(sid session.ID, entity sim.EntityID) (NetEntityID, bool, error)
ResolveHandle(sid session.ID, net NetEntityID) (sim.EntityID, bool/error)
```

Rules: already-visible → same handle, `created=false`; new →
fresh handle, `created=true`; unknown presence or invalid `EntityID`
→ error. Hiding a visible non-owned entity removes the mapping and
returns the retired handle; an absent entity is a no-op/not-visible
disposition (frozen clear semantics, no partial mutation). Hiding the
session's own controlled `EntityID` while presence is active is a
stable `ErrOwnEntityVisibility` (or equivalent); the own mapping
disappears only on full `Deactivate`. `ResolveHandle` maps `0` to
invalid, currently-visible handles to their entity, and
retired/stale handles or other sessions' handles to not-found — the
future target-handle gate for opcodes such as attack/use/trade (T5b
maps stale client handles to `202 invalid_handle`; T5a has no wire
use). The SAME sim `EntityID` visible to two sessions normally maps
to different handles per session (mandatory test): there is no global
wire entity ID. The handle table is the authoritative list of
entities the client may currently address; T5b MUST NOT resolve
target handles through a global cast/index bypassing it.

Incremental visibility ordering for T5b: on becoming visible,
allocate the handle, then `204 create` comes before any later `205`
for that visibility epoch; on visibility end, `206 remove` uses the
still-known old handle, then the mapping is retired. T5a sends none
of 203/204/205/206 and owns the API shape that makes this ordering
possible: `203 cell_snapshot` remains the M3/M10 baseline/full-cell
snapshot, `204 entity_create` is incremental entry into a live
session's visibility, `205 entity_move` is authoritative movement for
an already-visible entity, `206 entity_remove` ends visibility and
invalidates the handle. T5a does NOT invent entity-presentation
metadata (`204 EntityEntry` kind/proto/position/angle/speed
registries remain with the real entity-presentation/world layer).

#### 7.2.9 Per-character inbound rate limits

The frozen Store/config-independent gateway policy is:

```go
type RateLimitPolicy struct {
    MovePerSec   int
    IntentPerSec int
}
```

Production T5b derives it from the existing
`config.RateLimits.MovePerSec`/`IntentPerSec` (defaults 10/10, env
`VOX_RATE_MOVE_PER_SEC`/`VOX_RATE_INTENT_PER_SEC`); T5a adds no new
config field and imports no config package. Policy validation:
`MovePerSec > 0` and `IntentPerSec > 0` (no new upper bound — config
defines none). Each active presence owns exactly two independent
token buckets (movement; general gameplay-intent): a movement flood
never consumes intent budget and vice versa. For rate R, capacity is
R tokens and buckets start full, so the maximum immediate burst is
one second of configured traffic. Refill is continuous at R
tokens/second up to capacity, computed lazily from an explicitly
supplied `time.Time now` (no goroutine/ticker; fractional refill held
internally; tests stay deterministic). One accepted request consumes
exactly 1 token; below 1 token the request is denied without going
negative and without disconnect (T5b maps denial to
`202 error{rate_limited}`).

Reference behavior at `MovePerSec = 10`: at T0 ten immediate moves
are allowed and the 11th denied; at T0+100 ms exactly one token has
refilled so one request is allowed and the immediate next denied
(mandatory test). If `now` precedes the bucket's previous timestamp,
elapsed is 0: no tokens are minted and the internal timestamp never
moves backwards. Frozen opcode ownership for T5b wiring: `102 move`
charges the movement bucket; `103..120` gameplay intents charge the
general intent bucket; `100/101` auth, `121..124`
character/lifecycle, `125` ACK, and `126` leave_world are NOT charged,
so rate limiting can never block reauthentication, leaving the world,
or ACK flow. Rate denial precedes downstream gameplay: denied → `202
rate_limited` with no sim/gameplay handler invocation (T5a supplies
only the decision primitive). Migration-queue saturation stays
separate: bucket deny → later `202 rate_limited`, while
`sim.ErrMigrationQueueFull` → later `202 retry`; ordinary `MIGRATING`
state is never `retry`.

#### 7.2.10 Heartbeat core

Presence stores `HeartbeatAt time.Time`, with the initial value
supplied at activation; T5a runs no heartbeat goroutine. Frozen
runtime constants (§7 preserved): WebSocket Ping/Pong cadence 15 s,
dead-presence sweep 30 s; the core timeout constant is
`PresenceHeartbeatTimeout = 30 * time.Second` (T5b wires scheduling).
`TouchHeartbeat(sid, now)` advances only when `now` is strictly after
the current value — equal/older timestamps are no-ops, so a regressed
clock never moves heartbeat backwards. `StaleSessions(now)` reports
presences with `now >= HeartbeatAt + 30s` (boundary inclusive; `now <
HeartbeatAt` is never stale), returning session IDs sorted ascending
as a copy. The query performs no teardown and deletes nothing — T5b
owns close/delete. Heartbeat is transport liveness only: it advances
no sim tick, extends no auth token (the M3 90 s authorization
deadline stays independent), and touches no rate, AOI, or gameplay
state.

#### 7.2.11 Concurrency and determinism

Unlike sim entity mutation, the registry MAY be touched concurrently
by WebSocket read handlers, the future heartbeat runtime, the future
AOI fanout, and disconnect cleanup, so it MUST be race-safe (a single
short mutex/RWMutex over metadata is appropriate). The lock MUST NOT
be held over socket writes, sim calls, PG calls, sleeps, or external
callbacks (T5a performs none of those anyway). Every public
collection derived from maps MUST be sorted (AOI cells, entered,
exited, subscriber IDs, stale IDs, visible-handle snapshots if
exposed) — no map-order flakes.

#### 7.2.12 T5a non-scope (binding)

T5a adds no Redis/NATS/Kafka/gRPC/protobuf; the single-process
in-memory design stands. T5a builds registry/index primitives only:
no sim→gateway fanout channel, no `Engine.Run`/`SubmitMove`
ownership change (no broad Engine mutex shortcut), no
`sim.MovementSink`/`gateway.OutboundProducer`/`TryState` wiring, no
WebSocket changes (`server.go`, `outbound.go` unchanged; no Ping/Pong
runtime, no handler-chain change), no protocol changes
(`internal/proto` and `testdata/protocol` unchanged; no new
fields/opcodes/`msg_version`), no `session.Registry`/`session.entry`/
`session.Snapshot` mutation (linked by `session.ID` only), no
`store`/`persist`/migration/query changes, no sim production changes
(may import `sim.EntityID`, `world.CellCoord`, `session.ID` only),
no config changes, no new Prometheus series, and no new third-party
dependencies (`go.mod`/`go.sum` unchanged). T5a sends no 204/205/206
and implements no 102 handler, no 202 mapping, and no gameplay.

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

### 8.3 Snapshot saver runtime semantics (frozen, v0.3.19)

This section freezes the snapshot-saver runtime contract and splits
M4-T4 into saver core (M4-T4a) and persistence/operations proof
(M4-T4b). §8.1 remains authoritative for the PG CAS form, the
aggregate rule, and restart semantics; this section clarifies how the
saver schedules and serializes those CAS writes without violating
§8.1.

M4-T4a owns ONLY the generic saver machinery: a tracked durable
aggregate registry, one latest immutable full snapshot per dirty
aggregate, persisted-revision ownership, dirty coalescing, serialized
per-aggregate CAS attempts, a periodic 60 s scheduling seam, a
synchronous critical write-through path, stale/reconciliation
blocking, a context-bounded flush/shutdown primitive, and
deterministic/concurrency tests. M4-T4a does NOT own real character
gameplay state, vitals, abilities, inventory semantics, trade,
combat, death, gateway, AOI, presence, rate limits, PG adapter
wiring, Prometheus saver metrics, crash injection against real PG,
`cmd/serve` wiring, or SIGTERM process orchestration. Those remain
later tasks / M4-T4b. M4-T4a MUST NOT import `internal/store`,
`pgx`, `pgtype`, or `sqlc/gen`; its tests use synthetic
`SnapshotWrite` closures with no testcontainers and no Prometheus.

#### 8.3.1 Persisted revision is the CAS domain

Saver revisions are `int64 >= 0`: the PostgreSQL aggregate-root
revisions of §8.1. They are NOT `EntityID`, `OwnerRef.Generation`,
`OpID`, tick, `inputSeq`, or a dirty generation — those domains are
never compared against persisted revisions. A saver aggregate MUST
first be registered from a known authoritative load, conceptually
`Track(key, knownRevision)`: the known revision MUST be the actual
persisted revision from PG or another already-authoritative
operation. Unknown durable state is never silently initialized to
zero; a negative revision is an error with zero registration.

#### 8.3.2 Aggregate identity

The saver coordinates persistence with a small internal comparable
key, conceptually:

```go
type AggregateKind uint8

const (
    AggregateCharacter AggregateKind = ...
    AggregateItem
    AggregateBank
)

type AggregateKey struct {
    Kind  AggregateKind
    ID    int64
    Scope string
}
```

Exact numeric constants may differ. Semantics: character uses the
durable character root ID with empty `Scope`; item uses the durable
item root ID with empty `Scope`; bank uses the durable character ID
with a non-empty bank-system `Scope`. Rules: `ID > 0`, unknown kinds
rejected, character/item with non-empty `Scope` rejected, bank with
empty `Scope` rejected. This key is INTERNAL persistence
coordination only — NOT `EntityID`, `NetEntityID`, or wire data —
and MUST NOT become a high-cardinality metric label. Trusted string
names for future T4b metrics are exactly `character`, `item`,
`bank`; no IDs, names, bank systems, session IDs, or user data
become metric labels.

#### 8.3.3 Immutable full snapshots; execution-time ExpectedRevision

The saver MUST never retain a pointer to live mutable sim state. A
queued job captures a COMPLETE immutable aggregate snapshot behind a
write seam, conceptually:

```go
type SnapshotWrite func(
    ctx context.Context,
    expectedRevision int64,
) (newRevision int64, err error)
```

The closure captures snapshot CONTENT but does NOT freeze
`ExpectedRevision`: the saver supplies the current authoritative
known persisted revision only when that write actually owns the
aggregate's save slot. Example: PG at revision 4, snapshot A goes
dirty, periodic save A starts with `expected=4`; while A is
in-flight a later mutation produces full snapshot B; A commits so PG
moves to 5; B later saves with `expected=5`. B is a COMPLETE newer
snapshot containing all state that should survive; the saver does
NOT re-read PG before B, it uses the revision acknowledged from A.
The invariant stands: never re-read-and-blind-write, while avoiding
a guaranteed stale CAS for every mutation during an in-flight save.
This is safe ONLY because queued jobs are COMPLETE resulting
snapshots — no patch semantics. Future real producers MUST copy
JSON bytes, slices, child rows, and location values before
enqueueing; no live slice/map/pointer may be mutated after capture
in a way that alters the queued write.

#### 8.3.4 Dirty generation, coalescing, and queue bound

Each tracked entry owns an internal monotonically increasing dirty
generation (enqueue sequence). It is NOT the PG revision: it exists
only to tell whether a newer dirty snapshot arrived while an older
save was in flight. It is never persisted or sent anywhere; on
explicitly handled `uint64` exhaustion the saver fails closed, never
wraps. Per aggregate the saver retains at most ONE queued pending
snapshot plus at most ONE snapshot currently being saved.
`MarkDirty(key, snapshot)` increments the dirty generation, replaces
any queued-but-not-in-flight older snapshot, and returns promptly
without PG I/O — 10,000 mutations to one aggregate do NOT queue
10,000 snapshots; only the newest complete pending state matters.
The dirty queue is therefore bounded by the number of currently
tracked dirty aggregate roots, not the number of mutations. No
unbounded slice/channel of snapshots; no goroutine per dirty mark.

#### 8.3.5 Per-aggregate serialization; no global lock over I/O

For one `AggregateKey`, periodic save, manual flush, critical
write-through, and shutdown flush MUST NEVER execute its
persistence callback concurrently: one aggregate has exactly ONE
saver write owner at a time, via a context-aware per-key gate or
equivalent. A short global metadata mutex is allowed for entry
lookup, dirty replacement, and revision bookkeeping, but it MUST
NOT remain held while the `SnapshotWrite` callback runs: a slow save
for A MUST NOT prevent `MarkDirty` for unrelated B, and a blocked
save for A MUST NOT prevent a synchronous critical write-through
for B.

#### 8.3.6 Periodic scheduling

Production target is exactly 60 seconds, taken from the existing
`config.SnapshotIntervalSeconds` (default 60, validation `>= 1`,
env `VOX_SNAPSHOT_INTERVAL_SECONDS`); the T4a saver receives a
duration value and MUST NOT import config into sim. Constructor
validation: `interval > 0`, `clock != nil`. The existing sim
`Clock`/`Ticker` seam is reused; no `Now` seam is introduced —
scheduling is ticker pulses only, and T4b owns operational lag
instrumentation. `Saver.Run(ctx)` blocks, creates exactly one
ticker, maps one delivered pulse to one `FlushDirty` pass with no
catch-up bursts, stops the ticker on exit, and performs no extra
flush on context cancellation. `Run` starts no hidden worker pool;
the caller runs it in one owned goroutine. One transient snapshot
failure MUST NOT terminate periodic saving; `Run` continues to later
pulses. A narrow optional error-observer/sink may exist; no
Prometheus import in sim.

#### 8.3.7 Save execution and revision validation

At save selection: `expected = entry.knownRevision`, with the
selected immutable pending snapshot and its dirty generation. The
call `newRevision, err := job(ctx, expected)` succeeds ONLY when
`newRevision == expected + 1`, because every current §8.1 aggregate
CAS advances exactly one revision. If seq N succeeds and no newer
dirt arrived, `knownRevision = newRevision`, pending clears, the
aggregate is clean. If seq N succeeds but seq N+1 arrived while N
was in flight, `knownRevision = newRevision` while the newer pending
snapshot remains; the next save uses the new revision. If a
`SnapshotWrite` returns nil error but `newRevision !=
expectedRevision + 1`, the saver cannot infer durable state: it
returns a stable `ErrSaverRevisionInvariant` (or equivalent) and
reconcile-blocks the aggregate — never accepting the strange
revision, retrying blindly, or pretending success. If
`knownRevision == math.MaxInt64`, a new CAS `+1` cannot be
represented: fail closed before invoking the writer. An ordinary
transient (non-stale) write error leaves `knownRevision` unchanged,
retains the latest pending snapshot, keeps the aggregate dirty and
unblocked; the next periodic/manual flush may retry, with the
underlying error discoverable via `errors.Is`. An ambiguous commit
(transport error after PG committed but before success was
observed) is handled by NOT guessing: a retry at the same expected
revision surfaces as stale CAS and transitions into
reconciliation-required state. No outbox is invented.

#### 8.3.8 Stale classification and reconciliation blocking

Because T4a MUST NOT import `store`, the saver domain freezes its
own sentinel, `ErrSnapshotStale` (or equivalent); a future T4b
composition closure maps `store.ErrStaleRevision` into it while
preserving the original cause if useful. When a selected save
returns `ErrSnapshotStale`: `knownRevision` does NOT advance, the
entry becomes reconcile-blocked, automatic periodic retries stop
for that key, and pending state is not written again — the saver
MUST NOT hammer PG with the same stale CAS every 60 s. Saver
stale-blocking is persistence coordination; T3c (§5.6) remains the
gameplay mutation fence. While reconcile-blocked,
`MarkDirty`/`WriteThrough` return `ErrSaverReconciledRequired`-class
reconcile-required errors (exact name flexible); stale-memory
snapshots are never silently accepted for later persistence. The
owning layer reconciles (reloads/replaces memory from authoritative
PG under the T3c fence) and then calls, conceptually,
`ResolveReconciled(ctx, key, authoritativeRevision)`: it waits for
this key's saver write ownership, requires `authoritativeRevision
>= knownRevision` (no backward revision), sets `knownRevision` to
the authoritative revision, clears the stale block, discards ALL
pre-reconciliation pending jobs (they describe stale pre-reload
memory and MUST NOT overwrite freshly reconciled PG state), and
marks the aggregate clean. The owning sim MUST serialize PG reload,
memory replacement, `ResolveReconciled`, and the next mutation so
no new mutation races the discard window; T4a does not implement
that owner orchestration.

#### 8.3.9 Critical write-through

The synchronous single-root primitive, conceptually `WriteThrough(ctx,
key, snapshot) (newRevision, error)`, serves future critical
operations (logout flush, death, milestone advancement). It
serializes through the SAME per-key write gate as periodic saves —
no second persistence path. On invocation it allocates a dirty
generation for the critical snapshot. On success it clears pending
snapshots with generation `<=` the critical generation and retains
any newer snapshot that arrived while it waited/ran; an older queued
snapshot is thereby superseded and MUST NOT later overwrite the
critical state. If the critical write fails transiently with no
newer pending snapshot, the critical full snapshot
remains/re-enters pending dirty state so it is not lost; if a newer
full snapshot already exists, the newer one supersedes it.
`ErrSnapshotStale` from `WriteThrough` reconcile-blocks the key,
returns the stale error, advances nothing, and auto-retries
nothing. Multi-party transactions (trade, multi-bank transfer,
multi-item, death composition, ledger composition) remain M5/M8;
T3c supplies the post-commit multi-owner recovery model.

#### 8.3.10 Manual flush, shutdown flush, and lifecycle

The deterministic manual operation, conceptually `FlushDirty(ctx)
error`, captures the currently dirty unblocked key set, sorts keys
canonically (`Kind` ascending, `ID` ascending, `Scope` lexical
ascending — never Go map order), attempts each key at most once per
pass, continues to unrelated keys after one ordinary failure, and
returns `errors.Join(...)` or equivalent preserving `errors.Is`. A
reconcile-blocked aggregate is NOT automatically written during
periodic/manual/shutdown flush (no PG call is burned); its
reconcile-required condition is returned/reported as appropriate.
The shutdown primitive, conceptually `FlushAll(ctx)`, runs under the
caller contract: producer/sim mutation is quiesced first, then
`FlushAll` with a deadline context (matching §10: quiesce sim, then
flush dirty entities with deadline). T4a implements the bounded
primitive, not process SIGTERM wiring. Every per-key gate wait,
`SnapshotWrite` callback, and remaining iteration MUST respect the
caller context; `FlushAll` attempts each eligible dirty key once —
no infinite retry spin, no tight loop until the deadline. Failures
remain dirty and are returned; process-level policy belongs later.
Lifecycle: `Untrack(key)` (or equivalent) removes a clean tracked
aggregate and rejects dirty, in-flight, or reconcile-blocked keys
unless first flushed/reconciled, so unsaved state is never silently
abandoned. At each periodic pulse ONLY dirty aggregates save; clean
tracked roots produce zero callbacks. T4a MUST NOT modify
production `entity` with character IDs, PG revisions, vitals,
inventory, or bank fields. T4b reuses the existing
`voxilian_store_stale_revision_total{aggregate}` counter for actual
Store CAS stale writes and adds the saver-lag Prometheus
instrumentation frozen in §8.3.14 below.

#### 8.3.11 Persistence composition boundary (frozen, v0.3.20)

The production composition layer is `internal/persist` (or another
equally clear name only with a compelling repository reason).
Dependency direction is binding:

```text
sim     -> stdlib/world/etc (never store/pgx/sqlc)
store   -> pgx/sqlc/Prometheus stale metric (never sim)
persist -> sim + store (never pgx/generated sqlc directly)
observe -> Prometheus (never persist)
store   ->/-> persist, store ->/-> sim
```

`persist` depends on a narrow Store seam, approximately:

```go
type SnapshotStore interface {
    SaveCharacterSnapshot(context.Context, store.CharacterSnapshot) (int64, error)
    SaveItemSnapshot(context.Context, store.ItemSnapshot) (int64, error)
    SaveBankBalance(context.Context, store.BankSnapshot) (int64, error)
}
```

with bank reload on a separate narrow `BankLoader` seam over the
existing `LoadBankBalance`. The full `store.Store` API is never
required where the narrow seam suffices. T4b reuses the existing
`SaveCharacterSnapshot` / `SaveItemSnapshot` / `SaveBankBalance`
operations and the existing §8.1 CAS semantics (success returns
exactly `expected+1`; stale/missing/conflict surfaces
`store.ErrStaleRevision`; character saves carry spells+skills
atomically; item saves carry the location atomically; bank CASes
the balance). No new schema, queries, generated code, snapshot-job
tables, outboxes, or persisted OpID caches are required.

#### 8.3.12 Store snapshot adapters (frozen, v0.3.20)

`persist` exposes one job factory per aggregate family, returning
a correctly matched saver key plus write closure together so
callers cannot pair a snapshot with the wrong key, conceptually:

```go
type SnapshotJob struct {
    Key   sim.AggregateKey
    Write sim.SnapshotWrite
}

NewCharacterSnapshotJob(store SnapshotStore, snap store.CharacterSnapshot)
NewItemSnapshotJob(store SnapshotStore, snap store.ItemSnapshot)
NewBankSnapshotJob(store SnapshotStore, snap store.BankSnapshot)
```

Bank keys are `{AggregateBank, CharacterID, System}`. Factories
reject invalid durable identities (`character/item ID <= 0`,
`bank CharacterID <= 0`, empty bank system) with
`sim.ErrInvalidAggregateKey` — no rival key-validation domain.

The `ExpectedRevision` carried in the input snapshot is IGNORED:
it may hold any stale/hostile value. At execution the closure
copies/sets `snapshot.ExpectedRevision = expectedRevision` from
the saver immediately before invoking Store. The Saver is the
revision owner.

Capture is deep and immutable. Character snapshots copy `Vitals`
/ `Advancement` bytes and the complete `Spells` / `Skills` slices
(scalars copy normally). Item snapshots copy `Enchants` bytes and
the pointed-to VALUES of every location pointer field
(`CharacterID`, `CorpseID`, `ContainerItemID`, `VaultRegion`,
`PosX/Y/Z`, `Slot`) into fresh storage. Bank snapshots copy by
value. Mutating the caller's originals after factory return MUST
NOT alter the future Store request.

At the adapter boundary `store.ErrStaleRevision` becomes
discoverable as BOTH `sim.ErrSnapshotStale` AND
`store.ErrStaleRevision` (`errors.Join`, multi-`%w`, or
equivalent) — the ONLY stale-CAS translation point. Non-stale
Store errors (connection, cancellation, FK/check, container
cycle, other persistence failures) MUST NOT become stale; their
causes are preserved and the saver treats them as ordinary
transient/error outcomes per T4a.

#### 8.3.13 Saver-stale reconciliation bridge (frozen, v0.3.20)

A saver stale result means the saver-known persisted revision can
no longer be trusted as current. Recovery is a full materialized
PG reload before another authoritative mutation — never blind
retry, local revision increment, assumed `expected+1`, ledger
replay, or notification replay.

`persist` freezes a production bridge helper, conceptually:

```go
func ReconcileSaver(
    ctx context.Context,
    state *sim.ReconcileState,
    saver *sim.Saver,
    key sim.AggregateKey,
    reload sim.ReloadFunc,
) error
```

with binding order:

```text
1. state.RequireReload()
2. state.EnsureReconciled(ctx, reload): the loader stages PG
   values into temporaries, the candidate revision is validated,
   and Apply replaces the full in-memory durable aggregate
3. read the resulting known persisted revision
4. saver.ResolveReconciled(ctx, key, that revision)
5. owning gameplay continues only after both layers are clear
```

The bridge runs while the owning sim aggregate is otherwise
serialized. Failure handling is fail-safe: if the T3c reload
fails, the saver stays blocked, `ReconcileState` stays pending,
and `ResolveReconciled` is never called. If the T3c reload/apply
succeeds but `Saver.ResolveReconciled` fails or is cancelled, the
bridge calls `RequireReload()` AGAIN before returning the error —
memory may already hold PG state while the saver remains
blocked, and that split state MUST NOT look jointly ready to
gameplay. A later retry performs another full staged PG
replacement, which is safe because reloads apply COMPLETE
materialized state. Success requires `ReconcileState` pending
false at the loaded PG revision AND saver unblocked, clean, and
at the same revision; all pre-reload saver jobs are discarded by
T4a.

Bank is the proven reload family: a small staged helper,
conceptually `BankReload(loader, characterID, system, apply)
sim.ReloadFunc`, reads through the existing `LoadBankBalance`
into a temporary store-domain snapshot, returns the candidate
revision, and defers memory replacement to
`ReloadCandidate.Apply` (no live mutation during SELECT). No
speculative `LoadCharacterSnapshot` / `LoadItemSnapshot` APIs are
added; character/item adapters still prove real save composition,
stale translation, and saver blocking.

#### 8.3.14 Saver-lag observability (frozen, v0.3.20)

The §10 "saver lag" metric is exactly:

```text
vox_saver_lag_seconds{aggregate}   HistogramVec
```

Label `aggregate` takes EXACTLY `character`, `item`, `bank`
(produced only via `AggregateKind.MetricName()`); no IDs,
systems, names, sessions, cells, or entities. Buckets are
`prometheus.ExponentialBuckets(0.25, 2, 12)` (≈ 0.25 s … 512 s +
Inf), covering fast write-through, normal 60 s periodic saves,
and temporary outages.

One observation occurs ONLY when a `SnapshotWrite` returns nil
error with exactly `expectedRevision+1` accepted by the saver.
The observed value is snapshot age at successful persistence
acknowledgement, in seconds:

```text
successful acknowledgement time - that job's capture/enqueue time
```

The saver records a capture timestamp per pending job (new
operational seam `SaverNow func() time.Time` on `SaverConfig`;
nil means production `time.Now`; it affects metrics ONLY, never
correctness or revision ordering). A transiently failed snapshot
that remains pending RETAINS its original timestamp, so lag
grows across retries; a superseding newer snapshot carries its
own newer timestamp; a `WriteThrough` snapshot is stamped at
invocation (preserved if it becomes pending). No observation
occurs for transient, cancelled, stale, invariant, MaxInt64, or
reconcile-blocked outcomes. A regressed metric clock clamps the
observation to 0 — metrics never fail persistence.

The sim-domain observer seam uses base/stdlib types only,
conceptually `SaverObserver { SaverLag(aggregate string, lag
time.Duration) }`, optionally on `SaverConfig`. The saver
commits persistence bookkeeping BEFORE invoking it and MUST NOT
hold its global metadata mutex during the call; the observer has
no error return and cannot veto success.

The Prometheus adapter lives in `internal/observe/saver.go` as
`SaverMetrics` / `NewSaverMetrics(registerer)` /
`SaverLag(aggregate, lag)`, satisfying the sim observer
structurally without importing `internal/sim`. It whitelists
exactly the three aggregate strings (unknown ignored),
pre-creates all three series (stable zero-series, no dynamic
labels), and is registered on the `observe.Server`-owned
dedicated registry (field + accessor, mirroring
OutboundMetrics — never global registration). The registry
therefore exposes `vox_saver_lag_seconds` alongside
`voxilian_store_stale_revision_total` with no collision. T4b
adds NO other saver metric: the Store stale counter stays the
single stale-CAS counter.

#### 8.3.15 Shutdown / crash / commit-ambiguity proof (frozen, v0.3.20)

Graceful shutdown persistence is the existing T4a `FlushAll`
primitive: quiesce producers, then flush with a bounded context.
T4b proves the bounded context reaches real Store calls and can
abort blocked persistence (row-lock + cancel), with dirty state
retained and a later healthy flush succeeding. One `FlushAll`
attempts each eligible key at most once — no added retry loop.
No SIGTERM handler, process-exit policy, or production timeout
choice belongs to T4b.

Mid-save process/connection death is proved against the REAL
`SaveCharacterSnapshot` transaction: with the worker blocked
after its root CAS but before child replacement commits (table
lock barrier, backend identified via `pg_stat_activity` /
`pg_locks`, terminated with `pg_terminate_backend`), PG MUST
roll back root, spells, and skills completely; the Store call
returns a normal non-stale error (no stale-metric increment);
the saver keeps its known revision, retains the dirty snapshot
unblocked, and a retry with the same snapshot and expected
revision commits.

Commit ambiguity is proved with a REAL bank CAS whose first
successful revision is deliberately hidden behind a synthetic
"lost acknowledgement" error: after the first flush PG holds
the new revision while the saver stays dirty at the old known
revision (no guessing); the retry surfaces `ErrStaleRevision`,
maps to saver-stale, and blocks; reconciliation reloads the
committed revision, resolves both layers, and the next mutation
persists against it — no double increment, blind overwrite,
lost revision, or invented rollback.

#### 8.3.16 T4b boundaries (frozen, v0.3.20)

T4b does NOT own real gameplay entity persistence ownership,
`sim.entity` durable/gameplay fields, combat, death, trade, bank
or inventory gameplay, AOI, presence, rate limits, gateway
ingress, runtime `cmd/serve` wiring, SIGTERM orchestration,
PG-outage grace policy, `WorldSource`, protocol changes, config
changes (existing `SnapshotIntervalSeconds` suffices), Store
production changes, migrations/queries/generated code, or new
dependencies. `persist` production code MUST NOT import
pgx/generated sqlc; tests may use pgx/raw SQL for fixtures only.

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
- Death: TWO-PHASE lifecycle per source `Killed`/`ApplyDeathPenalties`
  (normative freeze §9.5): immediate real-death entry (disposition
  avoided/cheap/normal, corpse, ordered droppable drop with PK-protection
  plan, advancement reset/halve, post-death vitals, Underworld-phase entry)
  and delayed Underworld-exit penalties (resolved DeathCost scaling,
  BaseMaxHP penalty, per-ability losses, justice hooks). "Single-txn
  state+ledger" (D7/§8.1) applies WITHIN each phase's durable mutation —
  never across the whole lifecycle.
- Phase 2 (reserved, not MVP): guilds, factions/territory, justice, assassin
  game, Jala hinder matrix — schema MUST NOT preclude them (flags/ledger/kills
  already carry what they need).

### 9.1 M5-T1 weapon-combat math/domain core (frozen, v0.3.25)

M5-T1 builds the deterministic weapon-combat math/domain core as pure
sim-domain functions over immutable input structs/value objects. Normative
source: this section. Research reference: `docs/meridian59.md` §3, §4.3,
§7.1–§7.4 (corrected where the vendored-source audit required it; all
corrections are noted inline below). No GPL KOD/C text is copied: mechanics
only are reimplemented. All combat integer division truncates toward zero
(C semantics); every T1 combat operand is non-negative, so truncation equals
floor. Intermediate products use 64-bit widths; comparisons that could
overflow a doubled operand use the safe order form stated below.

#### 9.1.1 Ownership: T1 owns vs defers

T1 OWNS: combat rating arithmetic; player offense; player defense base
arithmetic; the generic monster Off/Def rating primitive; hit chance; hit
roll; generic weapon-family tables; generic weapon-quality tables;
pre-mitigation physical weapon damage math; the one-swing-per-second timing
primitive (simulation time); the weapon-attack vigor/exertion cost contract;
the final player-hit cap primitive; the damage severity classification hook;
deterministic RNG use; golden/property tests.

T1 does NOT own (and MUST NOT implement): armor damage reduction (M5-T2);
shield block implementation (M5-T2); resistance implementation (M5-T2);
spell damage/casting (M5-T3); real HP/mana/vigor entity storage (M5-T4);
regen/rest/hunger (M5-T4); `LoseHealth`/HP mutation (M5-T4/T5 composition);
death/corpse/respawn (M5-T5); actual weapon/item inventory lookup (M7/M9);
named weapon prototype catalog content (M9-T13); mob AI (M7); trade (M8);
gateway opcode-103 runtime wiring (NOT in T1 — see §9.1.2); protocol
changes (NONE); PG/store/persist (NONE).

The live sim entity gains NO new fields in T1 (`entity.go` untouched): no
HP/MaxHP/mana/vigor/stats/inventory/characterID/PG-revision/weapon-pointer.
Formula inputs that need such numbers receive them as immutable calculation
inputs, never as authoritative live vitals.

#### 9.1.2 No opcode-103 gateway wiring in T1

T1 does NOT modify `internal/gateway`. Existing behavior stands: opcodes
`103..120` are rate-gated, then delegated unchanged to `Next`. T1 defines
no `DecodeAttack`, resolves no `NetEntityID`, adds no gateway→sim attack
mailbox command, and sends no `202/207/208/210/etc.` Transport integration
waits until the M5 runtime vitals/mutation state exists; no half-functional
wire attack is created.

#### 9.1.3 Player offense

```text
Offense = Stroke*3 + Proficiency*2 + Aim*4 + (BaseMaxHP*3)/2
        + weaponHitMod + extraScalarMods
Offense = bound(Offense, 1, 1000)
```

- `Stroke`/`Proficiency` are the already-resolved ability values for the
  wielded weapon (`GetStroke`/`GetProf`; unarmed: Punch/Brawling abilities).
  `Aim` is the effective attribute (`bound(base+mod,1,70)`).
- `(BaseMaxHP*3)/2` evaluates left-to-right with one truncation
  (`((BaseMaxHP*3)/2)`), i.e. `floor(BaseMaxHP*1.5)`.
- `weaponHitMod` is the resolved family+quality hit modifier (§9.1.7–§9.1.8)
  plus any already-resolved numeric enchant `HitBonus`.
- `extraScalarMods` carries already-resolved additive attack modifiers
  (future attack-modifier/buff sources compose here without changing the
  core formula). Faction hit-roll bonus, ranged line-of-sight halving, and
  the universal `piFlags3` percent modifier are phase-2/future systems and
  contribute 0 in T1 (LOS needs room topology owned by later work).
- Negative impossible inputs (negative Stroke/Proficiency/Aim/BaseMaxHP)
  are deterministic domain errors, never silent clamps. Zero values are
  legal arithmetic inputs (the 1..1000 bound still applies).

#### 9.1.4 Player defense (arithmetic only)

```text
Defense = Parry*2 + Block + Dodge*3 + Agility*4 + (BaseMaxHP*3)/2
        + extraDefensePowerMods
Defense = bound(Defense, 1, 1000)
```

- T1 owns the arithmetic ONLY. Armor/shield sources belong to M5-T2.
- Unavailable components are already-zeroed numeric inputs (source
  behavior: Parry is 0 with no weapon, Block is 0 with no shield, Dodge is
  0 when its cost check fails). T1 takes plain numbers — no capability
  flags, no armor/shield objects, no duplicate of the future T2 decision
  layer. `Agility` is the effective attribute (`bound(base+mod,1,70)`).
- `(BaseMaxHP*3)/2` truncates exactly as in §9.1.3. `extraDefensePowerMods`
  carries already-resolved scalar terms only (future T2 armor
  `ModifyDefensePower`/faction/flag sources compose here later).
- Negative impossible inputs are deterministic domain errors.

#### 9.1.5 Monster combat rating primitive

```text
rating = 3*Level + 60*Difficulty
rating = bound(rating, 1, 1500)
```

- One primitive serves both monster Offense and Defense (source
  `monster.kod` uses the identical expression for both; per-mob
  `piOffense`/`piDefense` overrides and the Palsy ×3/4 effect are
  status/prototype systems owned by later work, not T1).
- Golden: Orc (Level 45, Difficulty 6) → `135+360` = 495. Yeti (Level 170,
  Difficulty 9) → `510+540` = 1050.
- Negative Level/Difficulty are deterministic domain errors. Zero is a
  legal arithmetic input (the 1..1500 bound still applies).

#### 9.1.6 Hit chance and hit roll

```text
chance = (Offense*55) / Defense        (integer truncation)
chance = bound(chance, 10, 95)
hit iff chance >= d100,  d100 in 1..100 inclusive
```

- `55` is source `EQUAL_CHANCE_HIT`. The product uses a 64-bit
  intermediate; ratings are bounded by their constructors so the normal
  formula cannot overflow, and the bound is enforced, never assumed.
- `HitChance` rejects `Defense <= 0` and `Offense <= 0` with stable domain
  errors (never divide-by-zero, never panic). Upper inputs need no
  rejection: the 10..95 clamp is total.
- The roll consumes the existing sim `RNG` interface (`Uint64() uint64`)
  through a private inclusive bounded helper. No global `math/rand`, no
  `crypto/rand`, no wall clock, no panic on a valid range. The helper is
  deterministic under an injected scripted RNG.
- Boundaries (frozen): chance 10 hits on rolls 1..10 and misses on 11;
  chance 95 hits on roll 95 and misses on 96..100.

#### 9.1.7 Generic weapon-family table

Verified against source `weapon.kod` tuning constants (the source
Low/Mid/High labels are confusing — Bludgeon uses the MID hit constant,
Slash the LOW one, etc. — so T1 freezes resolved numbers, not labels):

```text
Family    HitMod  Damage(incl)  Disarm  SpellMod  Range
Bludgeon  +75     4..8          -5      +0        2
Thrust    +125    3..8          +10     -10       3
Slash     +0      5..11         0       -15       2
```

- Typed enum/domain values only; unknown family is a deterministic domain
  error. No stringly-typed switch, no stable catalog IDs, no named
  prototypes (M9-T13 owns content IDs).

#### 9.1.8 Generic weapon-quality table

Verified against source `weapon.kod` quality constants; values compose
additively with the family row. `Normal` (the source default
`WEAPON_QUALITY_NORMAL`, which has no modifier branch) is frozen as
explicit zeros, never as accidental enum-zero:

```text
Quality   HitMod  DamageMod  DisarmMod  SpellMod  RangeMod
Low       +0      -1         -5         +5        +0
Normal    +0      +0         +0         +0        +0
High      +50     +1         +5         -5        +0
Nerudite  +25     +1         +0         +5        +0
```

- Unknown quality is a deterministic domain error. No named prototypes.
- `DamageBonus` (generic enchant/item numeric bonus) adds after the
  quality modifier: `w = roll + qualityDmgMod + damageBonus`, where the
  API receives the already-resolved numeric bonus (no inventory/enchant
  storage in T1).

#### 9.1.9 Base weapon damage roll

Family damage ranges are inclusive: `random(min,max)` in source is
`low + rand28 % (high-low+1)` (verified in `blakserv/ccode.c C_Random`),
so both endpoints are reachable. Boundary tests force min and max rolls.

#### 9.1.10 Pre-mitigation physical weapon damage (source-audited)

Source order (`stroke.kod FindDamage` → `DamageFactors`; `weapon.kod
GetDamage`) is frozen exactly — note the trap: `DamageFactors` starts its
accumulator at ZERO and the Might term re-includes the full scaled damage
at `(100+bonus)%`, so the base is counted exactly ONCE (inside the Might
term), plus a small flat proficiency bonus. There is no doubling:

```text
w      = baseRoll + qualityDmgMod + damageBonus   (all integers; §9.1.8)
s      = (w * damageFactor) / 100                  (truncation)
         damageFactor: Slash 80, Fire/bow 90, default 100
profFlat = ((proficiency + 1) * maxProfDamage) / 100   (truncation)
         maxProfDamage: 5 (source viMaxProficiencyDamage default)
attrBonus = bound(attr - 25, 0, 40)               (attr = Might melee, Aim Fire)
m      = ((100 + attrBonus) * s) / 100            (truncation)
raw    = profFlat + m
preMit = bound(raw, 1, $)                         ($ = no upper cap)
```

- Might effect: +0% at Might ≤ 25, up to +40% at Might ≥ 65. Fire/ranged
  substitutes Aim for Might with the identical bound (source `fire.kod`
  `DamageFactors`); the T1 API takes one already-resolved `attr` value and
  documents which attribute the caller passes — no weapon-content lookup.
- `generic attack modifier placement` (`ModifyDamage` bonuses, faction
  damage) applies AFTER this stage (source `player.kod GetDamage` adds
  `iDamageBonus` after `FindDamage`); those sources are future systems and
  enter through resolved numeric inputs, not T1 objects.
- Unarmed (`d4` + factors, Brawling as proficiency) is NOT a T1 weapon
  path; T1 freezes weapon paths only.

#### 9.1.11 Damage-stage boundary with M5-T2

T1 produces pre-mitigation physical weapon damage (§9.1.10). Normative
conceptual order (source `AssessDamage`):

```text
T1 raw physical weapon damage
  -> T2 armor damage modification (ModifyDefenseDamage)
  -> T2 resistance (ResistanceCheck/GetDamageFromResistance)
  -> future already-resolved post-mitigation bonuses (attmods AFTER resist)
  -> T1 final player damage caps (§9.1.12)
  -> later vitals LoseHealth (M5-T4/T5 composition)
```

T1 MUST NOT expose a single `ResolveAttack` that applies caps before the
T2 stage. Caps and raw damage are separately testable stage functions.

#### 9.1.12 Minimum damage and final player caps (source-audited)

Source `player.kod AssessDamage` order is frozen (non-absolute path; the
absolute/Illusionary-Wounds path skips all of this and belongs to M5-T3):

```text
1. if damage <= 0 -> damage = 1
2. one-third cap (below), then 3. 30 cap (below)
```

- Minimum 1 is guaranteed by step 1 at the cap stage; T2 armor/resistance
  work MUST NOT invent a contradictory minimum (an intermediate zero from
  mitigation still becomes at least 1 here).
- One-third cap (CORRECTED from the research summary, verified in source):
  the victim quantity on BOTH sides is `BaseMaxHP`, compared STRICTLY:

```text
if victimHP < 2*BaseMaxHP (strict <, overflow-safe compare)
   AND (NOT outlaw AND NOT murderer
        OR murdererProtectionSettingEnabled)
then damage = min(damage, ceil(BaseMaxHP/3))
where ceil(BaseMaxHP/3) = (BaseMaxHP + 2) / 3   (source: (base + (F-1)) / F, F=3)
```

  The default `murdererProtectionSettingEnabled` is FALSE (source
  `settings.kod pbDamageCapProtectionMurderersEnable = FALSE`), so by
  default outlaws/murderers are exempt from the one-third cap ONLY. The T1
  API takes the setting as an already-resolved boolean input.
- 30-per-hit cap: `damage = min(damage, 30)` (`MAX_DAMAGE_PER_HIT`),
  applied AFTER the one-third cap. Ordinary player weapon damage therefore
  satisfies `1 <= final <= 30`, and when the one-third cap applies also
  `final <= ceil(BaseMaxHP/3)`. The outlaw/murderer exemption affects ONLY
  the one-third cap, never the 30 cap. Monsters use NO player caps
  (source `monster.kod AssessDamage` floors at 1 only).
- Phase-2 faction +15% (`damage*115/100`) applies ABOVE these caps (source
  order) and is NOT implemented in T1; no faction system exists.
- Cap inputs use an immutable victim snapshot `{HP, BaseMaxHP, outlaw,
  murderer}` (no live vitals). Invalid snapshots (`BaseMaxHP < 1`,
  `HP < 0`) are deterministic domain errors. All comparisons use
  overflow-safe order operations (no raw `2*maxHP` / `damage*3` products);
  property tests cover large legal integers with no panic, wrap, or
  bypass.

#### 9.1.13 Severity classification hook (source-audited)

T1 owns a domain enum, NOT UI text (source `GetDamageDesc` returns per-type
prose; T1 returns the classification and future presentation maps it):

```text
Nick | Wound | Damage | Slay
```

- Non-lethal thresholds (source `DAMAGE_THRESHOLD_WOUND = 5`,
  `DAMAGE_THRESHOLD_DAMAGE = 15`): `damage > 15` → Damage; `damage > 5` →
  Wound; `damage > 0` → Nick.
- Lethal (source `damage = $` kill path) → Slay, always (overrides all).
- One-third forced-Damage (CORRECTED from the research summary, verified
  in source `battler.kod AssessHit`): applies ONLY to player victims, ONLY
  when non-lethal, uses the BUFFED `MaxHP` (source `GetMaxHealth`, NOT
  `BaseMaxHP`), with `>=` against FLOOR division, and classifies the
  POST-cap actually-applied damage:

```text
if victimIsPlayer AND NOT killed
   AND appliedDamage >= floor(victimMaxHP / 3)
then severity = Damage   (source forces iDmg = 16, i.e. just over the Damage line)
```

- The T1 API takes `(appliedDamage, killed, victimIsPlayer, victimMaxHP)`;
  non-lethal `appliedDamage < 1` and player-victim `victimMaxHP < 1` are
  deterministic domain errors.

#### 9.1.14 One swing per second (simulation time)

Source `IsOkayAttackTime` (default 1000 ms) both CHECKS and ARMS an
attack timer, and `TryAttack` calls it FIRST — so a later failure
(range, legality, vigor gate, costs) still consumes the swing, while a
too-early attempt returns before arming and mutates nothing. Frozen
tick-domain equivalent (`TickHz = H`, `cooldownTicks = H`):

```text
20 Hz  -> 20 ticks     60 Hz -> 60 ticks     120 Hz -> 120 ticks
```

- No wall clock, no timers, no goroutines, no per-entity ticker: pure
  function over `(hasSwung, lastSwingTick, nowTick, tickHz)` in the u32
  serial domain. First swing (`hasSwung = false`) is always allowed.
  Allowed iff unsigned mod-2³² `elapsed = nowTick - lastSwingTick` satisfies
  `elapsed >= cooldownTicks` (exact boundary allowed, one tick early
  rejected). Correct across `MaxUint32 -> 0` wrap by unsigned arithmetic;
  the exact half-range distance needs no special case (the `>=` on the
  unsigned distance is total).
- Recording rule (caller's storage, owned by later tasks): record
  `lastSwingTick = nowTick` when the readiness check passes (attempt-time
  arming, mirroring source); rejected too-early attempts MUST NOT update
  it. `tickHz` outside the configured `1..120` range is a domain error.

#### 9.1.15 Vigor/exertion cost contract (source-audited)

10000 exertion = 1 vigor (source `AddExertion` converts at `/10000`).
T1 owns ONLY the cost contract in integer exertion (M5-T4 owns the mutable
vigor model; T1 adds no vigor state to the entity):

- Standard weapon swing cost is 2000 exertion (source: slash/fire
  `viSkillExertion = 2`, charged as `1000*viSkillExertion` in `PayCosts`).
- CORRECTED from the research summary: slash and fire set
  `vbCheck_exertion = FALSE`, so standard weapon swings have NO pre-gate —
  the full 2000 is always charged on execution (stroke `SuccessChance` is
  unconditionally TRUE, so the half-cost `SkillFailed` path never fires
  for strokes). The "failed gate costs half" (`(1000*exertion)/2`) belongs
  to non-stroke skills and is out of T1 scope.
- The generic gate helper (for future gated strokes) mirrors source
  `HasVigor`: strict `vigor > required` (equality DENIES). A denied gate
  charges 0 at this stage for the stroke path; it still consumes the swing
  cooldown via the §9.1.14 attempt-time arming.
- No float vigor amounts, no regen, no mutation of player vigor. A future
  T4 composition consumes the integer result without changing T1
  arithmetic.

#### 9.1.16 Content, durability, range, and error boundaries

- Named weapons (Mace/Axe/Hammer/Longsword/…, bows/ammo) are M9-T13
  content: no proto IDs, no names, no durability counters in T1. Weapon HP
  degradation (75% −1 per hit) is OUT of T1; T1 result data lets a later
  item system observe that a successful weapon hit occurred.
- Family range values (§9.1.7) are frozen data. T1 implements NO
  range-check helper, NO lag-compensation rewind (the 2-second history
  ring stays reserved), NO line-of-sight/world-raycast (ranged content is
  later); all checks are server-authoritative by construction when they
  arrive.
- Public combat-domain functions fail deterministically on impossible
  domain inputs via stable `errors.Is` sentinels (never string parsing,
  never panic): unknown weapon family/quality, invalid damage range
  (`min > max`, negative), invalid `tickHz`, non-positive
  offense/defense rating inputs to `HitChance`, negative impossible
  stat/ability values, invalid HP snapshots. Plain already-resolved
  numeric modifiers (enchant bonuses, future buff scalars) are trusted
  inputs owned by future content systems and are not over-validated.

#### 9.1.17 Frozen property invariants

`HitChance` is always 10..95 for valid ratings; player Offense/Defense
always 1..1000; monster rating always 1..1500; successful table lookups are
immutable/value-only; rolled weapon damage stays inside the frozen
inclusive range before documented modifiers; ordinary player final damage
is `>= 1` and `<= 30`; when the one-third cap applies, final is `<=
ceil(BaseMaxHP/3)` subject to the minimum-one rule; outlaw/murderer
exemption affects only the one-third cap; cooldown never allows more than
one accepted swing per simulated second and stays correct across u32 wrap;
identical scripted RNG + identical inputs yield identical results.

### 9.2 M5-T2 defense mitigation math/domain core (frozen, v0.3.26)

M5-T2 builds the deterministic defensive-math layer as pure sim-domain
functions over immutable input structs/value objects. Normative source:
this section. Research reference: `docs/meridian59.md` §7.1, §7.4, §7.6
(as corrected by the v0.3.26 source audit; corrections are noted inline
below). Vendored sources audited: `kod/object/item/passitem/defmod.kod`
(`ModifyDefensePower`, `ModifyDefenseDamage`, `DefendingHit`),
`kod/object/item/passitem/defmod/shield.kod` (`GetBlockAbility`,
`ModifyDefensePower` identity, gated `ModifyDefenseDamage`),
`kod/object/item/passitem/defmod/armor/*.kod`,
`defmod/shield/*.kod`, `defmod/helmet/*.kod` (content numbers),
`kod/object/passive/skill.kod` (`SuccessChance`, `CanPayCosts`),
`kod/object/passive/skill/parry.kod`, `dodge.kod`, `block.kod` (flag
gates), `kod/object/passive/skill/stroke.kod` (`CanParry/CanBlock/
CanDodge` defaults), `kod/object/active/holder/nomoveon/battler.kod`
(`TryAttack` hit/miss split, `AssessMiss` + `PFLAG_DODGED`,
`ResistanceCheck`, `GetDamageFromResistance`, `GetDefenseDesc`
weighting), `.../battler/player.kod` (`GetDefense`,
`GetParry/Block/DodgeAbility`, `GetDamageType/GetSpellType`,
`AssessDamage` order, `AdvancementCheck` defensive-improvement block),
`.../battler/monster.kod` (`AssessDamage` without armor loop),
`kod/include/blakston.khd` (`ATCK_*` bits, `NO/MAX/MIN_RESISTANCE`),
`blakserv/ccode.c` (`C_Random` inclusive endpoints, `C_Bound`
clamp order). No GPL KOD/C text is copied: mechanics only are
reimplemented. All combat integer division truncates toward zero (C
semantics); every T2 combat operand in the reduction/resistance path is
non-negative at its truncation points except the documented unified
resistance numerator, whose truncation coincides with floor because the
result is non-negative (see §9.2.17). Intermediate products use 64-bit
widths. T2 MUST NOT duplicate the T1 `PlayerDefense` formula (§9.2.3).

#### 9.2.1 Ownership: T2 owns vs defers

T2 OWNS: defensive contribution primitives consumed by `PlayerDefense`;
Parry/Block/Dodge eligibility (capability) inputs and resolvers;
Parry/Dodge/Block success-roll math (one shared formula, §9.2.6);
shield block-value calculation; defensive power modifiers;
armor/shield damage reduction; the pure-spell vs weapon+spell armor
rule; the damage resistance tag/domain model; resistance aggregation;
resistance clipping; the resistance damage transform; deterministic RNG
use; golden/property tests.

T2 does NOT own (and MUST NOT implement): weapon hit/damage core
(T1, §9.1); spell damage/casting (M5-T3); real HP/mana/vigor entity
storage (M5-T4); actual vigor mutation (M5-T4); `LoseHealth`/HP mutation
(M5-T4/T5 composition); death/corpse/respawn (M5-T5); actual
weapon/item inventory or equipped-item lookup (M7/M9); item durability
mutation (M7/M9 composition — see §9.2.19 hook); named armor/shield
catalog content (M9-T13); gateway attack wiring (later M5 integration);
protocol changes (NONE); PG/store/persist (NONE).

The live sim entity gains NO new fields in T2 (`entity.go` untouched):
no HP/MaxHP/vigor/flags/inventory/equipment pointers. Every formula
input that needs such numbers receives already-resolved immutable
values, never live state.

#### 9.2.2 Pipeline position and stage boundary

Normative conceptual order (source `player.kod AssessDamage`):

```text
T1 RawWeaponDamage (pre-mitigation physical weapon damage, §9.1.10)
  -> T2 armor/shield ModifyDefenseDamage (§9.2.10–§9.2.13)
  -> T2 ResistanceCheck/GetDamageFromResistance (§9.2.14–§9.2.17)
  -> future already-resolved post-resistance bonuses (attmods AFTER resist)
  -> T1 ApplyPlayerDamageCaps (§9.1.12)
  -> later vitals LoseHealth (M5-T4/T5 composition)
```

(Monster victims skip the armor stage — source `monster.kod
AssessDamage` applies resistance only — but share the T2 resistance
helpers. The monster-side floor-1 and bonus composition belong to
future integration, not T2.)

T2 MUST NOT apply the minimum-one rule, the one-third player cap, or
the 30/hit cap; T2 MUST NOT mutate HP or classify death. Armor and
resistance may legitimately produce 0 intermediate damage (notably
+100 resistance yields exactly 0); T2 production MUST NOT floor that
to 1. The later T1 final player-cap stage owns minimum 1 (§9.1.12).

#### 9.2.3 PlayerDefense composition boundary (no duplication)

T1 owns the `PlayerDefense` arithmetic (§9.1.4) and T2 MUST NOT
reimplement it. T2 resolves the defensive components that a future
runtime feeds into `PlayerDefenseInput`:

```text
Parry     <- ResolveParryComponent (§9.2.5)
Block     <- ResolveBlockComponent (§9.2.5)
Dodge     <- ResolveDodgeComponent (§9.2.5)
ExtraMods <- ResolveDefensePowerModifier (§9.2.8)
```

`Agility` (effective attribute) and `BaseMaxHP` pass through unchanged
from their owning systems. A mandatory composition test feeds
T2-resolved components into the REAL T1 `PlayerDefense` (§9.1.4) and
asserts the golden result; production never duplicates the formula.

#### 9.2.4 Capability inputs (exact source gates)

Source conditions (`player.kod GetParry/Block/DodgeAbility`;
`parry.kod`/`dodge.kod` `CanPayCosts`; base `skill.kod CanPayCosts`;
stroke/monster `CanParry/CanBlock/CanDodge` defaults TRUE with rare
FALSE overrides, e.g. `avchief.kod` unparryable):

- Parry component is 0 iff ANY holds: no weapon equipped; parry
  cost-gate fails (source gate is the `NO_FIGHT` player flag; vigor is
  NOT gated — `vbCheck_exertion = FALSE` — and forget-enchantment /
  range are future systems folded into the same boolean); the incoming
  attack advertises CanParry = false. Otherwise the component is the
  resolved Parry ability (source: weapon `GetParryAbility` returns
  `GetSkillAbility(PARRY)`; no weapon-type bonus exists — the "TODO:
  Define standard bonuses" comment was never implemented).
- Block component is 0 iff ANY holds: no shield equipped; block
  cost-gate fails (source has NO `NO_FIGHT`/`NO_MOVE` flag check for
  block — only the base skill gate: forget-enchantment/range, folded
  into the boolean by future runtime); the incoming attack advertises
  CanBlock = false. Otherwise the component is the shield block rating
  (§9.2.5). Content note (M9 wiring, not T2 math): a torch forces
  rating 0 and a back-slung soldier shield forces rating 0; T2 models
  these as "no effective shield".
- Dodge component is 0 iff ANY holds: dodge cost-gate fails (source
  gate is the `NO_MOVE` player flag; vigor NOT gated); the incoming
  attack advertises CanDodge = false. Dodge has NO equipment
  requirement in source. Otherwise the component is the resolved Dodge
  ability.

T2 receives these as already-resolved immutable booleans/numbers —
conceptually:

```go
type DefenseCapability struct {
    HasWeapon      bool // weapon equipped (parry eligibility)
    ParryCostOK    bool // parry CanPayCosts resolved (NO_FIGHT + future)
    AttackCanParry bool // incoming stroke/monster CanParry
    HasShield      bool // effective shield equipped (block eligibility)
    BlockCostOK    bool // block CanPayCosts resolved (future gates)
    AttackCanBlock bool // incoming stroke/monster CanBlock
    DodgeCostOK    bool // dodge CanPayCosts resolved (NO_MOVE + future)
    AttackCanDodge bool // incoming stroke/monster CanDodge
}
```

Exact Go shape is flexible but the boundary is normative: NO live
equipped-item lookup, NO live room/flag reads, NO live vigor storage
inside T2. If capability depends on systems not yet implemented, the
caller passes the already-resolved boolean.

#### 9.2.5 Component resolvers

```text
ResolveParryComponent(parryAbility, cap) =
    0 unless cap.HasWeapon AND cap.ParryCostOK AND cap.AttackCanParry,
    else parryAbility
ResolveDodgeComponent(dodgeAbility, cap) =
    0 unless cap.DodgeCostOK AND cap.AttackCanDodge,
    else dodgeAbility
ResolveBlockComponent(blockSkill, shieldBonus, cap) =
    0 unless cap.HasShield AND cap.BlockCostOK AND cap.AttackCanBlock,
    else bound(blockSkill + shieldBonus, 1, 120)
```

- Skill abilities are `GetSkillAbility` values: 0 when the skill is
  unknown, else 1..99. Negative ability inputs are deterministic
  domain errors (`ErrInvalidDefenseSkill`); zero is legal (yields
  component 0 for parry/dodge; for block the rating formula still
  applies when enabled — see below).
- Block rating (source `shield.kod GetBlockAbility`): `bound(BlockSkill
  + piDefense_bonus, 1, 120)`. The bonus is the shield's
  `piDefense_bonus` (default `viDefense_base`); the `piBlockBonus`
  property exists in source but is NEVER read (CORRECTED from the
  research summary which said only "shieldBonus"). The SAME bonus
  value feeds both the rating here and the block success roll
  (§9.2.7) — that is source behavior, not double-counting: rating
  feeds `PlayerDefense`, the roll gates damage reduction. T2 MUST keep
  these two uses on separate, explicitly-tested paths (the T1
  quality-mod overlap bug is the cautionary example).
- Block rating edge (source-faithful): an enabled shield with skill 0
  and bonus 0 yields `bound(0,1,120) = 1` (a rating contribution with
  no reduction — reduction additionally requires ability > 0,
  §9.2.7). Disabled (no shield / gates fail) yields component 0, NOT
  the 1..120 clamp. Negative skill/bonus are domain errors only when
  they make the SUM input negative-impossible: negative `blockSkill`
  is always `ErrInvalidDefenseSkill`; negative `shieldBonus` is a
  trusted resolved numeric (no content in T2 is negative, but future
  cursed shields compose here) and flows through the clamp normally.
- `Agility` requisite values for §9.2.6 are effective attributes;
  negative is `ErrInvalidDefenseSkill`.

#### 9.2.6 Defensive skill success formula (shared)

All three defensive skills use the GENERIC skill `SuccessChance`
formula (source `skill.kod`; parry/dodge/block define NO override —
frozen explicitly). Requisite stat is Agility for all three (no
`GetRequisiteStat` override exists in any of the three skill
classes — the base returns `GetAgility`):

```text
chance = ((100 - requisiteStat) * ability) / 100 + requisiteStat + modifier
success iff d100 <= chance,   d100 in 1..100 inclusive
```

- ONE truncation: `((100-req)*ability)/100` evaluates the product
  first (64-bit intermediate), then adds `requisiteStat`, then adds
  `modifier`. (CORRECTED from the research shorthand which omitted
  modifier placement and truncation points.)
- Source has NO clamp on `chance`: `chance > 100` always succeeds
  (d100 max 100), `chance < 1` always fails (d100 min 1). T2 preserves
  this exactly — no invented 5..95 spell-style clamp (that clamp lives
  in spell code, not `skill.kod SuccessChance`).
- Normative API (exact names frozen for auditability):
  `DefenseSkillChance(ability, requisiteStat, modifier) (int, error)`
  (pure arithmetic, unclamped) and `RollDefenseSkill(rng, ability,
  requisiteStat, modifier)` returning the deterministic trace
  `{Chance, Roll, Success}`. Parry, Dodge, and Block contexts all call
  this ONE function; table tests prove all three contexts against
  hand-computed vectors.
- d100 uses the existing sim `RNG` interface and T1's deterministic
  inclusive helper. No new randomness interface, no global
  `math/rand`, no `crypto/rand`, no wall clock.
- Modifier placement for Block: `modifier = shieldBonus`
  (`piDefense_bonus`), per `shield.kod ModifyDefenseDamage`
  (`SuccessChance(#modifier=piDefense_bonus)`).

#### 9.2.7 Defensive roll meaning (source-audited, mandatory)

- The T1 Offense-vs-Defense hit roll (`chance >= d100`, `battler.kod
  TryAttack`) is the ONLY roll that decides whether an attack lands.
  Parry and Dodge abilities contribute to landing ONLY through the
  Defense rating (§9.1.4); their `SuccessChance` is NEVER rolled during
  combat resolution (verified: no `SuccessChance` call on
  SKID_PARRY/SKID_DODGE exists anywhere in the combat path).
- On a miss, `AssessMiss` sets `PFLAG_DODGED` on a player victim and
  selects miss prose via `GetDefenseDesc` weighting
  (`random(0, parry+block+dodge+50)`); at kill time,
  `AdvancementCheck` may `ImproveAbility` on dodge/parry/block
  (weighted 30% parry-if-armed, then 30% block-if-shielded).
  Advancement is M6-owned: T2 MUST NOT implement `ImproveAbility`.
- On a hit, ONLY Block rolls: `shield.kod ModifyDefenseDamage`
  requires `GetSkillAbility(BLOCK) > 0 AND
  Block.SuccessChance(+piDefense_bonus)` before applying the shield's
  reduction through the standard defmod algorithm.
- Consequently T2 implements exactly one combat roll: the Block
  success roll. Normative API: `RollBlock(rng, blockAbility,
  requisiteStat, shieldBonus)` returning `{Chance, Roll, Attempted,
  Succeeded}` where `Attempted = (blockAbility > 0)` and `Succeeded =
  Attempted AND (Roll <= Chance)`. `RollParry`/`RollDodge` as combat
  evasion rolls MUST NOT exist — a test MUST prove that a successful
  generic defensive-skill trace does not negate a landed hit (i.e.
  there is no second-evasion composition in production).
- T2 exposes `Attempted/Succeeded` flags (conceptually
  `UsedBlock`/`SuccessfulDefensiveSkill`) as data for the future M6
  advancement hook; no mutation, no text.
- The `GetDefenseDesc` miss-text weighting (`BATTLER_AVOID_CHANCE =
  50`) is presentation-only with no gameplay consequence and is
  frozen OUT of T2: no prose tables, no English strings, no outcome
  enum. A future presentation task owns it.

#### 9.2.8 Defense-power modifiers

Source (`defmod.kod ModifyDefensePower`; `player.kod GetDefense`
loop; `shield.kod` identity override):

```text
runningDefense = T1 base formula result (§9.1.4 without ExtraMods)
for each worn defense modifier, oldest-to-newest equivalent*:
    runningDefense = runningDefense + modifier.DefensePower
(*source iterates plDefense_modifiers head-first where Cons-prepend
makes the head most-recently-worn; addition commutes so iteration
direction is unobservable — frozen as a plain sum)
then faction / piFlags3 percent terms (phase-2/future, 0 in T2)
then bound 1..1000 (T1 owns the clamp)
```

- Sign semantics are plain addition of a SIGNED bonus (source
  `defense_power + piDefense_bonus`): Leather +50 raises, Chain −50 /
  Scale −100 / Plate −200 / Nerudite −150 lower, Robe +20 / Helm +25
  raise. No per-item clamp exists in source; the ONLY clamp is T1's
  final 1..1000 bound.
- Multiple equipped modifiers ALL apply (every element of
  `plDefense_modifiers`).
- Shields contribute ZERO DefensePower (source `shield.kod
  ModifyDefensePower` returns `defense_power` unchanged — the shield
  bonus flows to Block rating/chance instead). CORRECTED from the
  research summary, which did not state the shield exclusion.
- Normative API: `ResolveDefensePowerModifier(mods
  []DefenseModifier) (int, error)` returns the plain sum, suitable as
  `PlayerDefenseInput.ExtraMods`. To prevent a T1-style overlap bug,
  an entry with `RequiresBlock == true` (shield) MUST carry
  `DefensePower == 0`; otherwise `ErrInvalidDefenseModifier`. T2 never
  calls `PlayerDefense` internally.

#### 9.2.9 Generic defense-modifier representation

```go
type DefenseModifier struct {
    DefensePower  int  // signed ModifyDefensePower bonus; 0 for shields
    DamageReduce  int  // piDamage_reduce r (>= 0); 0 = no reduction stage
    RequiresBlock bool // true for standard shields (block-gated reduction)
}
```

Exact Go shape is flexible but the contract is normative: generic
immutable value, NO named armor/shield classes, NO hard-coded Leather/
Chain/Plate/Gold-Shield content (M9-T13 owns prototypes; golden tests
may use their documented numbers as vectors without creating catalog
types). Negative `DamageReduce` is `ErrInvalidDefenseModifier`
(source behavior for r < 0 is a degenerate NIL trace; unreachable from
real content which carries 0..6).

#### 9.2.10 Armor/shield damage-reduction algorithm

Source (`defmod.kod ModifyDefenseDamage`; `soldshld.kod` identical
copy) frozen exactly:

```text
reduce = 0
if r != 0:
    reduce = randomInclusive(floor(r/3), r)   (C_Random: low + rand28%(high-low+1))
    reduce = clamp(reduce, 0, damage-1)        (C_Bound order: raise to 0, then lower to damage-1)
if spellBits != 0:
    if weaponBits != 0:
        reduce = (reduce * 2) / 3              (single truncation, 64-bit intermediate)
    else:
        reduce = 0                             (pure spell bypass)
return damage - reduce
```

- `r/3` uses integer division BEFORE the random draw (floor for r ≥
  0); the 2/3 scaling applies to the ROLLED+CAPPED reduction AFTER
  the draw (source order: bound first, then `*2/3`). A test MUST
  distinguish scale-before-random from scale-after-roll (e.g. r = 5,
  forced max roll 5: correct `(5*2)/3 = 3`; scale-before would draw
  from `random(1, 3)` — different distribution AND different max).
- Edge table (normative): r = 0 → reduction 0, damage unchanged;
  damage = 1 → reduction 0 (`clamp(roll,0,0)`), returns 1; damage = 0
  → returns 0 unchanged with NO roll (documented freeze: the literal
  source trace yields `bound(roll,0,-1) = -1` and returns 1, a
  degenerate artifact of unreachable input — T1 guarantees pre-mit ≥
  1 — and T2 MUST NOT invent a minimum, §9.2.2); damage < 0 →
  `ErrInvalidDamageValue`; r < 0 → `ErrInvalidDefenseModifier`.
- No panic on any input; deterministic under scripted RNG (min/max
  rolls forcible).

#### 9.2.11 Damage-class rule

Source branch is on the RAW bitvectors (`atype` = weapon bits,
`aspell` = spell bits), NOT on a precomputed enum:

```text
aspell == 0                -> full reduction (pure weapon, incl. atype == 0)
aspell != 0 AND atype != 0 -> reduction scaled 2/3 (weapon+spell)
aspell != 0 AND atype == 0 -> reduction 0 (pure spell)
```

Normative domain representation (exact Go shape flexible):

```go
type DamageClass uint8
const (
    DamageClassWeapon ...       // pure weapon (incl. degenerate zero-vector)
    DamageClassSpell ...        // pure spell
    DamageClassWeaponSpell ...  // mixed
)
func ClassifyDamageClass(weaponBits, spellBits uint32) DamageClass
```

Unknown enum values on entry points are `ErrInvalidDamageClass`.
`ClassifyDamageClass(0, 0)` returns `DamageClassWeapon` (matches the
source branch: `aspell == 0` takes the full-reduction path).

#### 9.2.12 Multiple defensive modifiers

Source (`player.kod AssessDamage` loop) applies EVERY worn modifier
sequentially in list order; each sees the ALREADY-REDUCED damage and
draws its own independent RNG roll; each caps against its own current
`damage-1`:

```text
current = preMitigationDamage
for each mod in canonical order:
    if mod.RequiresBlock AND NOT blockSucceeded: continue
    current = current - RollDamageReduction(rng, mod.DamageReduce, current, class)
```

- Canonical Voxilian order is caller slice order, first element
  applies first. Live runtime feeds most-recently-worn-first to match
  source `Cons` list order. No Go map iteration anywhere on this path.
- Order-independence (verified algebraically, frozen): for fixed
  rolls and damage ≥ 1, sequential application equals `max(damage −
  Σreductions, 1)` regardless of order (each step is `max(d−r,1)` and
  `max` absorbs), including under the mixed 2/3 per-item scaling and
  the pure-spell all-zero case. A property test proves
  order-independence with an independent oracle (NOT the production
  function), while a scripted-RNG test pins the canonical draw order
  (RNG consumption follows slice order deterministically).
- Normative API: `ApplyDefenseModifiers(rng, damage, class, mods,
  blockSucceeded)` returning `{FinalDamage, TotalReduced,
  ModifiersApplied}` where `ModifiersApplied` counts entries that
  produced nonzero reduction (durability hook for M7/M9, §9.2.19; no
  mutation here).

#### 9.2.13 Shield block damage-reduction composition

- Shield reduction uses the SAME defmod algorithm (§9.2.10) via the
  same code path (source `propagate`), gated on the §9.2.7 Block
  outcome: ability > 0 AND success roll with `modifier =
  shieldBonus`. Armor reductions apply IN ADDITION in the same
  sequential loop (§9.2.12) — canonical order is the single worn-order
  slice containing both armor and shield entries.
- The shield bonus MUST NOT leak into `DefensePower`
  (§9.2.8 guard); the rating (§9.2.5) and the chance modifier
  (§9.2.6) are the only two placements. A composition test composes
  the REAL `ResolveBlockComponent` + `RollBlock` + shield reduction
  and proves the bonus lands exactly twice-in-two-places, never
  twice-in-one.
- Soldier-shield exception (source `soldshld.kod` override applies the
  algorithm with NO Block check): frozen as integration wiring, not T2
  math — the integrator models it as `RequiresBlock: false` with its
  documented numbers. T2 unit tests cover both gate settings.

#### 9.2.14 Resistance tag domain

Source bits (`blakston.khd`; values preserved exactly):

```text
weapon domain (ATCK_WEAP_*):
ALL 0x00001, NONMAGIC 0x00002, MAGIC 0x00004, HIT 0x00008,
BLUDGEON 0x00010, PIERCE 0x00020, THRUST 0x00040, SLASH 0x00080,
WHIP 0x00100, CLAW 0x00200, BITE 0x00400, STING 0x00800, ACID 0x01000,
UNARMED 0x02000, PUNCH 0x04000, KICK 0x08000, NERUDITE 0x10000,
SILVER 0x20000
spell domain (ATCK_SPELL_*):
ALL 0x0001, FIRE 0x0002, SHOCK 0x0004, COLD 0x0008, HOLY 0x0010,
UNHOLY 0x0020, ACID 0x0040, QUAKE 0x0080, HUNTERSWORD 0x0100
```

Normative representation (exact Go shape flexible): typed tag
constants carrying the source bit values plus a domain selector —
conceptually `ResistanceEntry{ IsSpell bool; Tag uint32; Value int }`
(source stores spell entries as NEGATED type; T2 uses an explicit
boolean, never sign tricks). These are COMBAT-domain tags: NOT item
proto IDs, NOT protocol IDs, NOT PG catalog IDs. Nothing persists in
T2.

Attack signature (conceptually `DamageSignature{ Weapon uint32; Spell
uint32 }`): raw bitvectors as passed to source `AssessDamage`
(`atype`/`aspell`). Callers pass them through; the resolver performs
ALL matching internally (see §9.2.15). Source note: weapon signatures
do NOT include the ALL bit (axe = NONMAGIC+SLASH) while spell
signatures DO (fireball = ALL+FIRE) — both styles resolve identically
under §9.2.15, so T2 requires no caller normalization.

#### 9.2.15 Resistance matching (exact source rule)

Source `battler.kod ResistanceCheck` frozen exactly. A weapon entry
with type T matches iff `(sigWeapon & T) != 0` OR (`sigWeapon != 0`
AND `T == WEAP_ALL`). A spell entry with type T matches iff
`(sigSpell & T) != 0` OR (`sigSpell != 0` AND `T == SPELL_ALL`).
Each entry is evaluated at most once; family and subtype entries are
independent entries (no double application within one entry; e.g.
signature NONMAGIC+SLASH vs entries WEAP_ALL +5 and SLASH +20
considers both once each).

#### 9.2.16 Resistance aggregation (exact source rule)

Source frozen exactly (`NO_RESISTANCE = 0`, `MAX_RESISTANCE = 100`,
`MIN_RESISTANCE = -100`):

```text
best = 0; worst = 0
for each matching entry with value v:
    if v > best:  best = v      (strictly positive only)
    if v < worst: worst = v     (strictly negative only)
best = min(best, 100)           (bound(best, $, 100): upper clip only)
worst = max(worst, -100)        (bound(worst, -100, $): lower clip only)
effective = best + worst        (structurally in -100..+100)
```

(CORRECTED from the research shorthand "clipped [−100,100]": source
clips each SIDE separately, never the sum — the sum is
structurally bounded because best ∈ [0,100] and worst ∈ [−100,0].)
Matching zeros change nothing. Normative API:
`ResolveResistance(entries []ResistanceEntry, sig DamageSignature)
int`. Duplicate same-(domain,tag) entries are merged by SUMMATION
before matching (mirrors source `AddResistance`, which sums into one
list element, so live lists carry unique tags; merging keeps the
helper total for all inputs). T2 MUST NOT mutate the input slice.

Frozen matrix (hand-computed): no matches → 0; [+20] → +20;
[+20,+50] → +50; [−10,−30] → −30; [+50,−30] → +20; [+150] → +100;
[−150] → −100; duplicates [+30,+30 same tag] → +60 (merge, mirrors
source); [+150,−150 different tags] → +100 + −100 = 0.

#### 9.2.17 Resistance damage transform (exact source rule)

Source `GetDamageFromResistance` has two branches that are
bit-identical in integer arithmetic; T2 freezes the UNIFIED form
(CORRECTED from the research shorthand which implied two different
formulas):

```text
effective = bound(effective, -100, +100)   (defensive; ResolveResistance output is already in range)
result = (damage * (100 - effective)) / 100   (single truncation, 64-bit intermediate)
```

- `damage * (100-effective)` evaluates first (saturating/64-bit; no
  sign flip on large legal inputs), then ONE truncating division by
  100. Truncation coincides with floor (result non-negative for
  damage ≥ 0 and effective ≤ 100).
- Frozen vectors (hand-computed): damage 100: +0 → 100; +25 → 75;
  +50 → 50; +100 → 0; −25 → 125 (`100*125/100`); −100 → 200.
  Truncation probes: damage 7 +50 → 3 (`7*50/100 = 350/100`); damage
  13 −25 → 16 (`13*125/100 = 1625/100`); damage 29 +25 → 21
  (`29*75/100 = 2175/100`).
- `+100` yields EXACTLY 0 (no minimum-one inside T2, §9.2.2);
  negative damage input is `ErrInvalidDamageValue`; damage 0 yields 0.
- Normative API: `ApplyResistance(damage, effective int) (int,
  error)`.

#### 9.2.18 Armor/resistance ordering and spell bypass

Order is normative: armor/shield `ModifyDefenseDamage` FIRST, then
`ResistanceCheck` + transform (§9.2.2). Never reversed; no T1 caps
inside either helper. Pure spell bypasses armor `DamageReduce`
(§9.2.10–§9.2.11) but STILL uses `ResistanceCheck` unless the damage
is absolute: source `AssessDamage` skips BOTH stages only when
`absolute = TRUE` (Illusionary Wounds path). `Absolute` spell
semantics belong to M5-T3; T2 exposes NO absolute flag — T3 composes
by skipping T2 helpers. No Illusionary Wounds implementation here.

#### 9.2.19 Durability, spell modifiers, content (all OUT)

- Durability (`DefendingHit`: 50% armor wear chance, 75% Nerudite
  wear, shield wear, shatter, repair, CAS): OUT of T2. Real equipped
  item instances and CAS ownership are later systems. T2 exposes only
  data: `ModifiersApplied` + `TotalReduced` (§9.2.12) and the Block
  outcome flags (§9.2.7) for future durability logic. No item
  mutation, no `DefendingHit` roll.
- `Spell_modifier` (casting penalties on armor/shields): OUT (M5-T3
  owns casting; M9-T13 owns content numbers). T2 defines no casting
  penalty value.
- Named equipment (Leather/Chain/Scale/Plate/Nerudite/Robe/Disciple/
  Helm/SimpleHelm/Circlet/Metal/Gold/Knight/Orc/Guild/Soldier
  shields/Torch, …): OUT (M9-T13). Their documented numbers appear in
  tests ONLY as vectors.

#### 9.2.20 Stable domain errors

```text
ErrInvalidDefenseModifier  (negative DamageReduce; shield entry with nonzero DefensePower)
ErrInvalidDefenseSkill     (negative ability / requisite-stat inputs)
ErrInvalidDamageClass      (unknown DamageClass enum value)
```

plus reuse of existing T1 errors: `ErrNilRNG`, `ErrInvalidDamageValue`
(negative damage), `ErrInvalidCombatStat` where a negative stat slips
a T1-owned check. Matching MUST use `errors.Is`, never string
parsing. No `ErrUnknownResistanceTag` (bitwise matching is total;
unknown bits are inert) and no `ErrInvalidResistanceValue` (source
`AddResistance` never clips; any integer entry value is legal and the
aggregation clip handles it) — errors are not invented beyond need.

#### 9.2.21 Determinism and RNG

Every randomized T2 mechanic uses the existing sim `RNG` interface
and T1's deterministic inclusive helper. No `math/rand` global, no
`crypto/rand`, no `time.Now`, no goroutines, no timers. Scripted RNG
drives: minimum reduction, maximum reduction, defense-success exact
threshold (`roll == chance` succeeds), `chance+1` fails, d100 1 and
100 boundaries. Same scripted RNG + same immutable input → same
output; no helper mutates input slices (resistance merge copies).

#### 9.2.22 Golden vectors (normative minimum)

DefensePower (plain sums): [] → 0; [Leather +50] → +50; [Plate −200]
→ −200; [Leather +50, Chain −50, Helm +25] → +25; shield entry
`{0, r, RequiresBlock:true}` → +0. Shield entry with DefensePower ≠
0 → `ErrInvalidDefenseModifier`.

Block rating: disabled (no shield) → component 0; (skill 0, bonus 0,
enabled) → 1; (skill 40, Metal bonus 5) → 45; (skill 90, Orc bonus
20) → 110; (skill 115, bonus 20) → 120 (upper clamp); parry/dodge
capability matrices per §9.2.4 (each gate's enabled/disabled path).

Skill chance (hand-computed, `((100−req)*abil)/100 + req + mod`):
(req 25, abil 50, mod 0) → `(75*50)/100+25 = 37+25` = 62; (req 10,
abil 20, mod 0) → `(90*20)/100+10 = 18+10` = 28; (req 30, abil 0,
mod 15) → `0+30+15` = 45; (req 40, abil 99, mod 0) → `(60*99)/100+40
= 5940/100+40 = 59+40` = 99; over-100 (req 40, abil 99, mod 10) →
109 (unclamped, always succeeds); negative (req 0, abil 0, mod −5) →
−5 (always fails). Roll boundaries: roll == chance succeeds,
chance+1 fails, 1 always succeeds unless chance < 1, 100 always fails
unless chance ≥ 100. Block outcome: ability 0 → `Attempted=false`;
ability > 0 → attempted with the shared formula.

DamageReduce: r = 0 → 0 for any damage/class; r = 6, damage 20:
forced-min roll → `random(2,6) = 2` → 18; forced-max → 6 → 14; r =
30, damage 3, forced-max roll 30 → capped `damage−1 = 2` → 1;
damage 1, any r → 1; damage 0 → 0 (no roll). Pure-spell class zeroes
any roll (r = 6 max roll, damage 20 → 20). Mixed class scales AFTER
cap: r = 5 max roll 5 → `(5*2)/3 = 3` → damage 20 → 17; r = 4 max
roll 4 → `(4*2)/3 = 2` → 20 → 18. Multi-modifier: damage 20, class
weapon, mods [{r:6},{r:4}] with scripted max rolls → 20−6−4 = 10;
same with pure-spell class → 20. Shield composition: REAL
`ResolveBlockComponent` + `RollBlock` + `ApplyDefenseModifiers`
with Gold-shield numbers (bonus 10, r 1).

Resistance aggregation matrix (§9.2.16) plus wildcard proofs:
signature (NONMAGIC+SLASH, 0) vs entries [WEAP_ALL +5 (leather),
SLASH +10 (gold), BLUDGEON +10] → best +10 (unrelated BLUDGEON
ignored, each entry counted once); signature (0, ALL+FIRE) vs
[SPELL_ALL +15 (helm), SPELL_FIRE +20 (nerudite), SPELL_SHOCK +20]
→ +20; signature (0, ALL+FIRE) vs [SPELL_ALL −20 (knight),
FIRE −10 (plate)] → −20; knight-disciple style [+10 PIERCE,
−20 SPELL_ALL] vs (NONMAGIC+PIERCE, 0) → +10 (spell weakness
ignored); vs (0, ALL+HOLY) → −20 (weapon resist ignored).

Resistance transform vectors (§9.2.17) plus composition:
T2 zero (`damage 100, +100 → 0`) into T1 `ApplyPlayerDamageCaps` in
a TEST ONLY yields 1 (stage-ownership proof; production never
composes them).

At least one real armor number (Plate: DefensePower −200,
DamageReduce 6, resists FIRE −10 / SHOCK −15) and one real shield
number (Gold: block bonus 10, DamageReduce 1, resists SLASH/BLUDGEON/
THRUST +10) appear as test vectors without becoming content types.

#### 9.2.23 Frozen property invariants

Effective resistance always in −100..+100; +100 resistance never
yields negative damage (exactly 0 for damage ≥ 0); −100 never
amplifies above exactly 2× damage; positive resistance never
increases damage; negative resistance never decreases damage; armor
reduction never exceeds `currentDamage−1` per applied modifier (for
damage ≥ 1) and never amplifies (damage 0 → 0); pure-spell armor
reduction is exactly 0; Block rating always within 1..120 when
enabled, exactly 0 when disabled; same scripted RNG + same immutable
input → same output; no helper mutates input slices; T2 production
never calls `ApplyPlayerDamageCaps` (a test asserts the zero case
stays zero through T2 alone).

### 9.3 M5 spell combat: T3a generic core, T3b special archetypes, T7 runtime (frozen, v0.3.27)

M5 spell work is split into three tasks with a hard pure/runtime
boundary. Normative source for T3a: §9.3a. Research reference:
`docs/meridian59.md` §3 (success formula), §4.3 (vigor gates), §5
(spell bases), §7.5 (spell damage) — as corrected by the v0.3.27
source audit; corrections are noted inline below. Vendored sources
audited: `kod/object/passive/spell.kod` (`SuccessChance`,
`SpellFailed`, `CanPayCosts`, `CanPayManaVigor`, `CanPayReagents`,
`KarmaCheck`/`GetRequiredKarma`, `PayCosts`, `GetManaCost`,
`GetTranceTime`, `BeginCastingTrance`, `DoubleCheckAfterTrance`,
`TranceBroken`), `kod/object/passive/trance.kod`
(`BeginCastingTrance`, `EndEnchantment`, `BreakTrance`),
`kod/object/passive/spell/atakspel.kod` (generic `CastSpell`
damage/scaling/ManaFocus),
`kod/object/active/holder/nomoveon/battler/player/user.kod`
(`UserCast` payment-before-trance order),
`kod/object/active/holder/nomoveon/battler/player.kod`
(`GetLevel` = `GetBaseMaxHealth`, `IsOkayAttackTime`,
`HasVigor`, `AssessDamage` absolute path),
`kod/object/item/passitem/spelitem.kod` and
`kod/object/item/passitem/specwand/spelwand.kod` (item-cast paths),
`kod/object/passive/spell/persench/touchatk.kod`,
`kod/object/passive/spell/atakspel/illwound.kod`,
`kod/object/passive/spell/atakspel/vampdrn.kod`,
`kod/object/passive/spell/earthqua.kod`,
`kod/object/passive/spell/walspell.kod`,
`kod/object/active/wallelem.kod` (+ `wallfire.kod`,
`wallltng.kod`) — the last paragraph is T3b-boundary reference
only (§9.3b). `kod/include/blakston.khd`
(`SPELLPOWER_MINIMUM = 1`, `SPELLPOWER_MAXIMUM = 99`). No GPL
KOD/C text is copied: mechanics only are reimplemented. All spell
integer division truncates toward zero (C semantics); every T3a
operand at a truncation point is non-negative except the
already-resolved signed Hinder delta, whose addition precedes the
5..95 clamp so the clamped result is non-negative. Intermediate
products use 64-bit widths.

#### 9.3.0 Why T7 exists (architectural rationale, frozen)

The live `sim.entity` deliberately does NOT yet contain HP,
MaxHP, Mana, Vigor, spell/cast state, death state, or
inventory/reagent state. M5-T4 owns the authoritative vitals
model. Therefore M5-T3a/T3b MUST NOT create a temporary second
vitals model, mutate HP/Mana/Vigor, implement gateway 103/104,
or invent a parallel combat entity. T3a/T3b are pure/value
mechanics; M5-T7 performs the runtime composition only after
the required state exists (see §9.3c).

#### 9.3a M5-T3a generic spell core (normative)

T3a OWNS: spell-power domain validation; generic SuccessChance
math; the d100 spell-success roll; the ReagentRing
success-override contract (as an already-resolved boolean);
generic AttackSpell damage scaling; the generic Mana Focus
damage bonus; mana-cost arithmetic; spell exertion/vigor gate
arithmetic; the resource-payment result plan (full vs failed);
the reagent gate/substitute plan; karma required-value
arithmetic and eligibility; the BaseMaxHP minimum gate; the
post-cast cooldown primitive; cast/trance duration arithmetic;
the cost-before-trance ordering contract; the
trance-break/no-refund contract; item-cast generic behavior;
the absolute/resistance policy value; deterministic RNG use;
golden/property tests.

T3a does NOT own (and MUST NOT implement): live HP/Mana/Vigor
(M5-T4); HP damage application (M5-T7); death (M5-T5); actual
reagent inventory (M7/M9, with a T7 seam); the actual
ReagentRing item (later content/inventory); room rules
(T7/world); target acquisition (T7); line-of-sight queries
(T7/world); PK/safety legality (M5-T6/T7); Spellbane/Silence
enchantments (later spell/content runtime); Jala Hinder
implementation (phase 2 — T3a accepts only an
already-resolved delta); Deflect runtime (later/T7
composition); advancement (M6 — T3a exposes only a pure
successful-cast hook flag); named spell catalog (M9);
gateway opcode 104 (M5-T7); protocol changes (NONE);
Store/PG (NONE).

The live sim entity gains NO new fields in T3a (`entity.go`,
`engine.go`, `ingress.go` untouched): no HP/BaseMaxHP/MaxHP/
Mana/MaxMana/Vigor/Exertion/Stomach/TranceState/CastState.
Every formula input that needs such numbers receives
already-resolved immutable values, never live state.

##### 9.3a.1 Spell-power domain

Source `blakston.khd`: `SPELLPOWER_MINIMUM = 1`,
`SPELLPOWER_MAXIMUM = 99`. The normal production path
(`UserCast` → `GetSpellPower`) always returns clamped
`bound(power, 1, 99)`; source handlers merely warn on 0 and
proceed arithmetically, but that leniency is NOT reachable
through normal casting and T3a MUST NOT reproduce it: T3a
functions take spell power as an already-resolved immutable
input valid in `1..99` only. Power `0`, negative power, and
power `> 99` are deterministic domain errors
(`ErrInvalidSpellPower`), never silent clamps. `SPELLPOWER_
MAXIMUM` is frozen as the named divisor 99 in the damage
pipeline (§9.3a.11 of this spec — the AttackSpell scaling
section) — never 100.

##### 9.3a.2 Generic spell SuccessChance

Source `spell.kod SuccessChance` frozen exactly. Inputs are
already-resolved values; T3a performs NO world queries:

```text
base = ((100 - requisiteStat) * spellPower) / 100 + requisiteStat
chance = base + ResolvedHinderDelta
chance = bound(chance, 5, 95)
```

- ONE truncation: `(100-stat)*power` first (64-bit
  intermediate), `/100`, then `+ stat`, then `+
  ResolvedHinderDelta`. `requisiteStat` is the already-resolved
  division requisite (Mysticism/Stamina/Intellect by school);
  negative requisite stats are domain errors
  (`ErrInvalidCombatStat` reuse); spell power is §9.3a.1.
- `ResolvedHinderDelta` is the already-resolved signed Jala/
  Hinder alteration (source: same-school hinder subtracts
  `random(sp/2, sp*2/3)` of the *hinder song's* power; the
  Hinder implementation itself is phase 2). It applies BEFORE
  the 5..95 bound (source order: Hinder, then clamp). Zero in
  the MVP. No clamping of the delta itself.
- Bound placement: exactly once, after Hinder, before the
  no-LOS adjustment. Source `bound(num, 5, 95)`.
- No-LOS single-Battler adjustment (§9.3a.3) applies AFTER the
  bound. There is NO second clamp after it.
- Success is source-exact: `d100 <= chance` over the existing
  sim `RNG`/`RollD100` (no second RNG implementation). The
  `ReagentRing` in-use override arrives as the already-resolved
  boolean `ForceSuccess`: an ordinary failed roll with
  `ForceSuccess == true` succeeds; with false it fails. No
  ring lookup, no charges, no inventory in T3a. A
  ForceSuccess-rescued cast resolves the FULL-success payment
  path (§9.3a.7 of this spec — the payment section), not the
  failed path.

##### 9.3a.3 Exact no-LOS adjustment

Source frozen exactly — NOT conventional distance falloff.
Applies only when the already-resolved inputs state
`SingleBattlerTarget == true AND HasLineOfSight == false`
(source additionally requires `GetNumSpellTargets = 1`, first
target is `Battler`, caster not immortal DM — all folded into
these two booleans by the future runtime). `SquaredDistance`
is the already-resolved integer squared distance (`>= 0`;
negative is `ErrInvalidSquaredDistance`):

```text
distance = SquaredDistance / 4          (integer division)
if distance > chance/2:    chance = chance/2
else if distance < chance/2: chance = chance - distance
(else equal: chance unchanged)
```

- Both comparisons are STRICT `>` / `<` against `chance/2`
  (itself integer division). Equality leaves chance unchanged.
- Effects only decrease-or-keep: halve (`chance/2`, integer
  division) when far, subtract `distance` when near. The
  result may fall below 5 (even to 0/negative) — source has
  NO post-distance clamp and T3a MUST NOT add one.
- `SquaredDistance` needs no validation beyond
  non-negativity; division uses 64-bit intermediates.

##### 9.3a.4 Mana cost

Source `spell.kod GetManaCost` frozen exactly. `baseMana` is
the already-resolved `viMana` (`>= 0`; negative is
`ErrInvalidManaCost`):

```text
if baseMana == 0: return 0
cost = baseMana
if spellPower > 40: cost -= 1        (strict >, 40 gets nothing)
if spellPower > 80: cost -= 1        (strict >, 80 gets nothing)
cost -= ceil(cost * ManaReductionPercent / 100)
return bound(cost, 1, $)
```

- `ManaReductionPercent` is the already-resolved scalar
  equipment reduction (source: Princess Shield faction-rank
  5–25%; T3a MUST NOT hard-code Princess Shield). Valid
  `0..100`; outside is `ErrInvalidManaReduction`.
- Exact rounding: `(cost*percent + 99)/100` integer division
  = `ceil(cost*percent/100)` — any fractional reduction
  rounds UP (costs the player). 64-bit intermediate.
- Final nonzero cost is bounded to at least 1
  (`bound(cost,1,$)`); the `baseMana == 0` path bypasses it
  and returns 0 (source `DMSpell`-style zero-mana spells).
- Mana availability (source `CanPayManaVigor` mana leg):
  `currentMana >= cost` passes (equality passes; strict `<`
  fails). Pure helper, immutable inputs.

##### 9.3a.5 Karma requirement

```text
Qor:       required = -10 * spellLevel
Shalille:  required = +10 * spellLevel
other ordinary schools: required = 0
```

- `spellLevel` valid `1..6` (M59 spell levels); outside is
  `ErrInvalidSpellLevel`. `school` is the typed T3a domain
  `{SchoolQor, SchoolShalille, SchoolOther}` (callers map
  Kraanan/Faren/Riija/Jala/DM to `SchoolOther`; DM immortal
  bypass is runtime, not T3a); unknown values are
  `ErrInvalidSpellSchool`. No spell IDs, no faction/quest
  system.
- Eligibility (source `KarmaCheck`, equality passes both
  ways):

```text
required > 0:  casterKarma >= required  (fails only on strict <)
required < 0:  casterKarma <= required  (fails only on strict >)
required == 0: allowed
```

##### 9.3a.6 BaseMaxHP / piMinHitPoints gate

CORRECTED from the research summary (verified in source):
`CanPayCosts` gates `caster.GetLevel() < piMinHitPoints`, and
Player `GetLevel()` returns `GetBaseMaxHealth` — i.e. the
authoritative UNBUFFED BaseMaxHP, NOT current HP and NOT
buffed MaxHP. T3a freezes the generic gate over immutable
inputs:

```text
allow iff BaseMaxHP >= minimum     (equality passes; strict < denies)
```

`BaseMaxHP < 1` or `minimum < 0` is `ErrInvalidHitPointGate`.
`minimum == 0` (source default `piMinHitPoints = 0`) always
allows. Current HP MUST NOT appear in this function.

##### 9.3a.7 Spell vigor/exertion gate and charges

Source generic spell defaults: `vbCheck_Exertion = TRUE`,
`viSpellExertion` in `0..100` (default 2); outside `0..100`
is `ErrInvalidExertion`. All amounts are integer exertion;
10000 exertion = 1 vigor (shared T1 domain, no float vigor,
no live mutation):

- Availability mirrors source `HasVigor` with the STRICT
  threshold: `currentVigor > required` passes (equality
  DENIES). When the already-resolved `checkEnabled == false`
  (source `vbCheck_exertion = FALSE`), the gate is skipped
  (proceeds, full charge on success). Negative vigor input
  is `ErrInvalidExertion`.
- Full successful-cast charge: `10000 * viSpellExertion`
  (64-bit; default 20000 = 2 vigor).
- Failed-cast charge (source `SpellFailed` "half exertion"):
  `(10000 * viSpellExertion) / 2` with integer truncation.

##### 9.3a.8 Preflight reagent contract

T3a MUST NOT invent reagent item IDs. It represents an
already-resolved preflight state:

```text
ReagentAvailable    (inventory has required reagents)
ReagentSubstituted  (substitute/ReagentRing satisfies requirement)
ReagentMissing      (neither)
```

Source timing frozen: a missing-inventory requirement
satisfied by ReagentRing charges during the reagent preflight
check consumes/reserves that substitute charge BEFORE the
later SuccessChance roll. A later spell-roll failure does
NOT retroactively restore it. T3a exposes this as
DATA/transaction-plan output only (`SubstituteConsumed bool`
on the preflight plan); no inventory mutation, no charge
counters.

##### 9.3a.9 CanPayCosts ordering relevant to T3a

Source `CanPayCosts` order relevant to this milestone
(runtime/legal gates interleaved by T7 later):

```text
enabled/accessibility
BaseMaxHP minimum
rest/forget
post-cast cooldown check+arm
other runtime/legal gates (token, mana/vigor availability,
  reagent availability/substitute, karma, range/safety/target,
  target spell-resist check)
```

Critical timing invariant (source `IsOkayAttackTime` both
checks AND arms, called before the later gates, with no
disarm path): post-cast cooldown does NOT arm when an
earlier BaseMaxHP/rest/forget gate fails. Once the post-cast
cooldown check succeeds, it IS armed even if a later mana,
vigor, reagent, karma, range/safety/target gate fails. T3a
owns the timing primitive (§9.3a.10); T7 owns the complete
runtime ordering.

##### 9.3a.10 Post-cast cooldown primitive

Simulation ticks, not wall clock. Inputs
`(hasPriorAttempt, lastAttemptTick, nowTick, tickHz,
postCastSeconds)`:

```text
allowed iff NOT hasPriorAttempt
  OR unsigned mod-2^32 (nowTick - lastAttemptTick) >= postCastSeconds * tickHz
```

- On an allowed attempt the caller arms
  `lastAttemptTick = nowTick` immediately at the
  source-equivalent stage (§9.3a.9). Rejected too-early
  attempts do NOT re-arm. First attempt always allowed.
- `postCastSeconds == 0` (valid) is always allowed.
  Negative seconds, `tickHz` outside `1..120`, or
  `postCastSeconds * tickHz` overflowing int64 are
  `ErrInvalidCastTiming` / `ErrInvalidTickHz`. No
  goroutine/timer. Wrap-safe by unsigned arithmetic.

##### 9.3a.11 Payment occurs BEFORE trance; full vs failed payment

Source `UserCast` sequence frozen as binding:
`CanPayCosts`/preflight → `PayCosts` → if the payment roll
succeeds → `BeginCastingTrance`. Payment resolves BEFORE the
cast/trance timer begins. T3a returns a value/result plan; it
does NOT mutate resources.

- Successful spell roll (or ForceSuccess rescue):

```text
mana charge = full resolved mana cost (§9.3a.4)
exertion charge = 10000 * viSpellExertion
normal reagents consumed = YES (plan flag ConsumeReagents)
substitute charge = as already resolved in preflight (§9.3a.8)
trance may begin
```

- Failed spell roll without override (source `SpellFailed`
  "half mana, half exertion, no reagents"):

```text
mana charge = resolved mana cost / 2      (integer truncation)
exertion charge = (10000 * viSpellExertion) / 2
normal reagents consumed = NO
substitute charge already consumed in preflight REMAINS consumed
no trance begins
```

##### 9.3a.12 Target spell-resist cost behavior

Generic `SpellResist` of an enchantment target happens during
the source preflight. When it resists, source deliberately
invokes the ordinary `PayCosts` path to simulate a cast with
no effect, then aborts before trance. Frozen composable T3a
contract: `TargetResisted == true` → still resolve the
ordinary spell-payment roll → full or half cost per that
roll → no effect → no trance. Do NOT confuse this
enchantment-resist gate with T2 `ResistanceCheck` for numeric
damage. No SpellResist algorithm in T3a; T7/later spell
runtime supplies the resolved `TargetResisted` decision.

##### 9.3a.13 Trance duration and sim ticks

Source `GetTranceTime` frozen exactly:

```text
if baseCastMs == 0: tranceMs = 0
else: tranceMs = (baseCastMs * (150 - spellPower)) / 100
```

- ONE truncation (`*` first, 64-bit intermediate, then
  `/100`). Normal power 1..99 yields 149%..51%. Negative
  base is `ErrInvalidCastTiming`; spell power is §9.3a.1.
  (Source immortal-DM zero-trance is runtime, not T3a.) No
  Warp Time implementation; a future resolved modifier may
  wrap this stage.
- Milliseconds → sim ticks (no wall-clock timer goroutines
  in sim): `ticks = ceil(durationMs * tickHz / 1000)` so a
  cast never completes earlier than the source duration.
  64-bit intermediate; negative ms, `tickHz` outside
  `1..120`, or product overflow are domain errors. No hidden
  floating point. Boundary vectors (§9.3a.20) pin 0/1/600/
  1000/1500/5000/30000 ms at 20/60/120 Hz.

##### 9.3a.14 Trance interruption and completion

Resources were already spent before trance (§9.3a.11), so:
trance interrupted → no spell effect, NO refund (source
`TranceBroken` is message+sound only). The runtime event
that breaks trance is NOT T3a; T3a exposes the value
`TranceRequired bool` (derived from `tranceMs > 0`) and owns
no enchantment/timer state. At natural trance completion
source revalidates targets/legality (may Deflect, then
`CastSpell`) and does NOT pay costs again — frozen
no-second-payment contract; revalidation, Deflect, and live
effects belong to T7/later systems.

##### 9.3a.15 Generic AttackSpell base damage and Mana Focus

Source `atakspel.kod CastSpell` frozen exactly:

```text
damage = inclusive Random(min, max)     (both endpoints reachable)
```

For a normal PLAYER cast, not an item cast:

```text
damage = (damage * (50 + spellPower/2)) / SPELLPOWER_MAXIMUM
```

- Truncation order: `spellPower/2` truncates FIRST, then
  multiply, then `/99` truncates (64-bit intermediate).
  Divisor is `SPELLPOWER_MAXIMUM = 99`, never 100. Power
  is §9.3a.1 (`min > max` is `ErrInvalidRange`; negative
  bounds are `ErrInvalidDamageValue`).
- Mana Focus (ordinary player non-item AttackSpell with the
  focus flag set; inputs are resolved scalars
  `ManaFocusActive bool`, `ManaFocusPower`,
  `ManaFocusBonus` — no enchantment implementation):

```text
if ManaFocusActive:
    damage += ((ManaFocusPower * ManaFocusBonus) / SPELLPOWER_MAXIMUM) + 1
```

  Multiply-then-divide (single truncation), then
  unconditional `+1` (source: the `+1` applies even when
  the bonus scalar is 0). Negative focus inputs are
  `ErrInvalidCombatStat`.

##### 9.3a.16 Player / monster / item-cast damage and resource differences

Source-audited, frozen via the explicit origin domain
`CastOrigin {OriginPlayer, OriginItem, OriginMonster}`:

```text
OriginPlayer  (normal player cast):
    spell-power scaling (§9.3a.15) + Mana Focus allowed;
    full mana/vigor/reagent payment (§9.3a.11);
    karma gate applies.
OriginItem    (player item/scroll/wand cast, bItemCast=TRUE):
    NO spell-power scaling, NO Mana Focus (raw roll to AssessDamage);
    NO mana/vigor/inventory-reagent payment;
    karma gate STILL applies unless the resolved ItemSkipsKarma
    policy input is true (source bCheckKarma, default enforced).
    Item charge consumption itself is later inventory, not T3a.
OriginMonster (non-player cast):
    NO scaling, NO Mana Focus; NO costs of any kind
    (source CanPayCosts/PayCosts no-op for non-players).
```

Unknown origin is `ErrInvalidCastOrigin`. T3a implements no
item consumption and no monster AI.

##### 9.3a.17 Generic attack-spell result and absolute policy

T3a produces only PRE-APPLICATION spell damage and
metadata: `{Damage, Origin, Policy}`. It does NOT call
`ApplyDefenseModifiers`, `ApplyResistance`,
`ApplyPlayerDamageCaps`, HP mutation, or death. Runtime
composition is later (§9.3a.18, §9.3c).

T3a defines the narrow policy value
`DamagePolicy {PolicyOrdinary, PolicyAbsolute}`
representing source `absolute`: ordinary spells take the
normal resistance pipeline (pure-spell armor bypass per
§9.2.18 still applies inside T2); absolute spells bypass
numeric resistance and bypass ordinary player damage caps
where source requires (source player `AssessDamage` skips
defense+resistance+bonus+floor-1+both-caps when absolute;
monster `AssessDamage` skips resistance only). T3a
implements NO absolute damage formula itself (Illusionary
Wounds is T3b); the representation exists so T3b/T7 compose
without ad-hoc booleans. Unknown policy is
`ErrInvalidDamagePolicy`.

##### 9.3a.18 T1/T2/T3 pipeline boundary

Full eventual damage order frozen (no premature universal
`ResolveDamage()` that erases these differences):

```text
Weapon attack:
  T1 raw weapon damage -> T2 defense modifiers -> T2 resistance
  -> resolved bonuses -> T1 player caps -> T4 HP mutation -> T5 death
Ordinary AttackSpell (armor DamageReduce bypassed by T2's pure-spell rule):
  T3 raw spell damage -> T2 damage-class defense stage -> T2 resistance
  -> resolved bonuses -> applicable T1 player caps
  -> T4 HP mutation -> T5 death
Absolute special spell:
  T3b special raw/absolute calculation
  -> bypass source-defined resistance/caps -> T4/T7 exact special HP rule
```

A T3a test composes generic AttackSpell raw damage through
the REAL T2 pure-spell defense stage + resistance to prove
no armor DamageReduce but full resistance; T3a production
never calls T1 caps.

##### 9.3a.19 Stable domain errors

`errors.Is`-compatible sentinels for public-domain errors
(no string parsing; no dozens of hyper-specific errors):

```text
ErrInvalidSpellPower       (power outside 1..99)
ErrInvalidSpellSchool      (unknown CastSchool value)
ErrInvalidSpellLevel       (spell level outside 1..6)
ErrInvalidManaCost         (negative base mana)
ErrInvalidManaReduction    (percent outside 0..100)
ErrInvalidExertion         (exertion outside 0..100, or negative vigor)
ErrInvalidHitPointGate     (BaseMaxHP < 1, or minimum < 0)
ErrInvalidCastTiming       (negative durations, overflow)
ErrInvalidSquaredDistance  (negative squared distance)
ErrInvalidCastOrigin       (unknown CastOrigin value)
ErrInvalidDamagePolicy     (unknown DamagePolicy value)
```

plus reuse of existing T1/T2 errors: `ErrNilRNG`,
`ErrInvalidRange`, `ErrInvalidHitRoll`,
`ErrInvalidCombatStat`, `ErrInvalidDamageValue`,
`ErrInvalidTickHz`, `ErrInvalidVictimSnapshot` where the
same domain condition applies. Deterministic under
scripted RNG; identical RNG + identical inputs yield
identical results.

##### 9.3a.20 Golden vectors and property invariants (normative minimum)

Success base `((100-req)*power/100)+req`: (req 10, power 1)
→ `(90*1)/100+10 = 0+10` = 10; (req 25, power 50) →
`(75*50)/100+25 = 37+25` = 62; (req 50, power 99) →
`(50*99)/100+50 = 49+50` = 99 → bound 95; (req 10, power
99) → `(90*99)/100+10 = 89+10` = 99 → 95; low clamp: (req
1, power 1) → 1 → 5. No-LOS: chance 60, squaredDistance 0
→ distance 0 `< 30` → 60; squaredDistance 120 → distance
30 `== 30` → unchanged 60; squaredDistance 200 → distance
50 `> 30` → 30; odd chance 61, squaredDistance 120 →
distance 30, `61/2 = 30`, equal → unchanged 61. d100:
`1`, `chance`, `chance+1`, `100` boundaries with
`d100 <= chance` success. ForceSuccess flips a scripted
failed roll to success with full-payment plan.

Mana: base 0 → 0 at any power; base 8 power 40 → 8, power
41 → 7, power 80 → 7, power 81 → 6; base 8 power 81 pct
0 → 6; pct 25 → `6-(150+99)/100 = 6-2` = 4; pct 100 →
`6-6` = 0 → bound 1; base 1 power 99 pct 100 → 1 (floor).
Karma: Qor L1 → −10, Qor L6 → −60, Shalille L1 → +10,
Shalille L6 → +60, other → 0; caster exactly threshold
allows, one point wrong-side denies. BaseMaxHP gate:
`Base < min` denies, `==`/`>` allows. Exertion: disabled
check proceeds; `vigor == required` denies (strict),
`required+1` allows; full `10000*e`, failed half with odd
truncation (`e = 3` → 30000/15000). Cooldown: first
attempt allows; one tick early rejects; exact boundary
allows; u32 wrap correct; 0 postcast seconds allows.
Trance: base 600 ms power 1 → `(600*149)/100` = 894 ms;
base 1000 power 50 → 1000 ms; base 5000 power 99 →
`(5000*51)/100` = 2550 ms; base 30000 power 99 → 15300 ms;
base 0 → 0 at any power. Ticks `ceil(ms*Hz/1000)`: 1 ms @
20 Hz → 1; 600 ms @ 20 Hz → 12; 1000 ms @ 60 Hz → 60;
1500 ms @ 20 Hz → 30; 5000 ms @ 120 Hz → 600.
AttackSpell (Fireball-like 8..12 scripted min/max): power
1 → `(8*(50+0))/99 = 400/99` = 4 .. `(12*50)/99 =
600/99` = 6; power 50 → `(8*75)/99 = 600/99` = 6 ..
`(12*75)/99 = 900/99` = 9; power 99 → `(8*99)/99` = 8 ..
12 (a `/100` implementation fails the power-50 vectors).
ManaFocus (bonus 5): inactive → unchanged; active focus
power 1 → `+((1*5)/99)+1 = +1`; focus 99 → `+(495/99)+1
= +6`; bonus 0 active → `+1`; monster/item origins ignore
focus. Origin matrix: identical raw roll + power proves
player-scaled vs item-raw vs monster-raw.

Property invariants: success base before the no-LOS stage
respects the frozen 5..95 bound only at that stage (the
post-distance value is intentionally unclamped);
nonzero-base mana cost stays `>= 1`; full payment `>=`
failed payment componentwise; failed normal casts consume
no normal reagents; trance duration deterministic in
(power, base); postcast timing wrap-safe; identical
RNG/input → identical result. Monotonicity is asserted
only where source guarantees it (no invented monotonicity
across the strict `>`/`>=` tier edges).

#### 9.3b M5-T3b special spell-damage archetypes (normative, v0.3.28)

T3b remains a PURE/VALUE sim-domain layer: deterministic mechanics
over immutable scalar/value inputs plus the injected RNG. Normative
source: this section. Research reference: `docs/meridian59.md` §7.5
(as corrected by the v0.3.28 audit). Vendored sources audited:
`kod/object/passive/spell/persench/touchatk.kod` (+ `touchatk/
zap.kod`, `icyfing.kod`, `acidtch.kod`, `holytch.kod`,
`flametch.kod`), `kod/object/passive/spell/persench.kod`
(identity `ModifyHitRoll`), `.../battler/player.kod`
(`GetOffense`, `TryAttack` cost gates, touch wield rule),
`kod/object/passive/spell/atakspel/illwound.kod`
(`CastSpell` override, `GetHPLoss`, `GetDuration`,
`EndEnchantmentEffects`), `kod/object/passive/spell/atakspel/
vampdrn.kod` (`DoSideEffect`), `kod/object/passive/spell/
earthqua.kod` (`CastSpell` severity, `ComputeDamage`,
item-self, environmental), `kod/object/passive/spell/
walspell.kod` (+ `walspell/firewall.kod`, `ltngwall.kod`,
`ifirewal.kod`), `kod/object/active/wallelem.kod`
(`CheckForEffect`, `PeriodicEffect`, affected dedup, gates),
`kod/object/active/wallelem/wallfire.kod`,
`wallltng.kod`, `kod/object/passive/pfirewll.kod`,
`pltngwll.kod`. No GPL KOD/C text is copied: mechanics only
are reimplemented. All integer division truncates toward zero
(C semantics); intermediate products use 64-bit widths, with
overflow-safe SIGNED arithmetic wherever a signed operand
(negative karma, `HP-1`) can occur — never a non-negative-only
helper on a signed operand.

T3b OWNS: TouchAttackSpell combat composition values, touch
base-damage scaling, touch duration, the source-audited Holy
Touch damage modifier; Illusionary Wounds HP-loss formula,
duration/refund contract; Vampiric Drain heal-from-damage
side-effect formula; Earthquake severity, squared-distance
falloff, normal/environmental/item-self damage formulas; wall
max-damage arithmetic, element lifetime arithmetic, periodic
jitter, ordinary fire/lightning rolls, illusionary-wall
damage/refund calculation; special-spell damage
policy/signature values; golden/property/fuzz tests.

T3b does NOT own (and MUST NOT implement): authoritative HP
mutation or healing (M5-T4/T7); death (M5-T5/T7); real cast
routing (M5-T7); 103/104 gateway handling (M5-T7); world
target enumeration, room object creation, wall placement
geometry (later runtime/world); wall goroutines/timers
(NEVER — later deterministic sim/runtime owns scheduling);
per-target wall affected-set runtime state (later
deterministic sim/runtime; T3b documents the contract only);
LOS queries, room flags, safety/PK checks (world/runtime,
M5-T6/T7); kill attribution (M5-T5/T7); advancement (M6);
named spell/item catalog (M9); inventory/reagents (M7/M9/T7
seam); Store/PG (NONE); protocol changes (NONE).

The live sim entity gains NO new fields in T3b
(`entity.go`, `engine.go`, `ingress.go` untouched): no
HP/MaxHP/Mana/Vigor/Stomach/regen/healing state.

Damage pipeline boundaries (no universal `ResolveDamage`):

```text
Ordinary special spell (quake, ordinary walls, vamp raw):
  T3b raw/special damage -> T2 defense-class handling -> T2 resistance
  -> future resolved bonuses -> applicable T1 player caps
  -> T4 HP mutation -> T5 death
Touch attack (COMBAT-STROKE spell, weapon AND spell domains):
  T3b touch raw damage -> T2 mixed weapon+spell defense handling
  -> T2 resistance -> future resolved bonuses -> T1 player caps
  -> T4 HP mutation
Illusionary Wounds / illusionary wall (absolute):
  T3b absolute loss calculation -> bypass T2 numeric resistance
  -> bypass ordinary T1 player hit caps -> T4/T7 exact HP application
```

##### 9.3b.1 Touch offense components

Touch is a combat-stroke spell: the touch spell ability is the
Stroke, and proficiency is (source `touchatk.kod GetProf`):

```text
MysticismContribution = (Mysticism * 3) / 2
Proficiency = max(PunchAbility, MysticismContribution)
```

Integer order is binding: `Mysticism * 3` first (64-bit),
then `/ 2` — NOT `Mysticism * (3/2)`. `Mysticism` is the
already-resolved effective attribute; `PunchAbility` the
already-resolved Punch skill (0 when unknown). Negative
inputs are `ErrInvalidCombatStat`. Normative API conceptually
`TouchProficiency(punchAbility, mysticism int) (int, error)`.

Future composition uses the REAL T1 functions (no second
offense/hit formula): Touch Stroke + Touch Proficiency +
resolved Aim + resolved BaseMaxHP + resolved attack modifiers
→ T1 `PlayerOffense` → T1 `HitChance`/`RollHit`. A test MUST
compose T3b with the real T1 functions and MUST fail if a
hit-factor bonus is added, if Mysticism 1.5 rounds wrong, or
if the Punch/Mysticism max is reversed.

`viHit_Factor` audit conclusion (mandatory anti-footgun,
verified by exhaustive consumer grep): touch subclasses
carry `viHit_Factor` classvars (generic 80, Zap 50, Icy 60,
Acid 10, Holy 15, Flame 25) but the current live Player
`GetOffense` path (GetStroke/GetProf/Aim/BaseMaxHP/weapon
`ModifyHitRoll`/attack modifiers; touch `ModifyHitRoll` is
identity) NEVER reads them. Voxilian MUST NOT add a
hit-factor bonus merely because the classvar exists.

Touch attack-modifier behavior (source `touchatk.kod`): the
generic TouchAttackSpell reports `GetParryAbility() = 0`,
`CanBlock() = TRUE`, `CanParry() = TRUE`, `CanDodge() =
TRUE`. Meaning: an incoming touch attack CAN be defended
against with the victim's ordinary defensive capabilities
(T2 owns defense semantics; no additional evasion rolls),
while a touch spell as the caster's effective weapon
contributes zero parry ability.

##### 9.3b.2 Touch damage base

Source `touchatk.kod FindDamage` frozen exactly:

```text
r = inclusive Random(minDamage, maxDamage)
half = r / 2                  (integer truncation BEFORE power multiply)
damage = half + (half * spellPower) / 99 + 1
damage = DamageFactors(damage)   (generic identity; §9.3b.3 overrides)
damage = bound(damage, 1, $)
```

- Divisor is `SPELLPOWER_MAXIMUM = 99`. Generic
  `DamageFactors` is identity. Final floor is 1
  (`bound(damage,1,$)`); no upper cap here (later
  `AssessDamage` caps belong to T1/T7 composition).
- No Might bonus, no weapon quality, no weapon proficiency
  flat bonus, no generic T1 `RawWeaponDamage`: touch has its
  OWN source formula. Spell power is the T3a valid `1..99`
  domain; `min > max` is `ErrInvalidRange`; negative bounds
  are `ErrInvalidDamageValue`.

##### 9.3b.3 Touch damage signature

Touch attacks carry BOTH domains (source defaults
`viAttackType = ATCK_WEAP_UNARMED+ATCK_WEAP_PUNCH =
0x2000+0x4000`, subclass `viSpellType` overrides; source
bits preserved): T3b takes already-resolved
`WeaponAttackBits`/`SpellAttackBits` (or reuses the T2
`DamageSignature`) — NO named spell IDs. The default/source
family pairs unarmed/punch weapon bits with one spell
subtype (SHOCK/COLD/ACID/HOLY/FIRE `+ SPELL_ALL 0x0001`)
supplied by the touch spell. A touch with both domains
nonzero MUST classify through the existing T2 mixed
weapon+spell rule (`ClassifyDamageClass`); neither domain
is counted twice. T3b implements no resistance itself.

##### 9.3b.4 Touch duration

Source `touchatk.kod GetDuration` frozen exactly:

```text
secondsUnit = inclusive Random(spellPower/3, spellPower/2)
secondsUnit = bound(secondsUnit, 10, 75)
durationMs = secondsUnit * 6 * 1000
```

Truncation points: integer `spellPower/3` and
`spellPower/2` BEFORE the random draw; `*6*1000` after the
10..75 bound (64-bit; product overflow is a domain error).
Spell power is T3a `1..99`. Value-only: no timers. Future
runtime composes with the existing T3a `CastTicks`
conversion; no duplicate tick conversion.

##### 9.3b.5 Touch per-hit resource cost

Source `player.kod TryAttack` frozen: the CanPayCosts gate
and the PayCosts charge apply ONLY to `Skill` strokes
("spells have already been cast" / "Only check skills,
because spells are already cast"). A touch spell, already
cast, pays NO mana, NO spell exertion, NO skill exertion,
NO reagents per swing. T3b charges nothing per hit. Normal
combat swing timing remains T1/T7 runtime ownership.

##### 9.3b.6 Touch subclass audit and Holy Touch modifier

Source-audited: Zap, Icy Fingers, Acid Touch, Touch of
Flame override ONLY damage signature (spell subtype),
range, mana/cost, the dead `viHit_Factor` classvar,
presentation, and reagents — NOT the damage formula. ONLY
Holy Touch overrides `DamageFactors`. Frozen explicitly
(source `holytch.kod DamageFactors`; victim-undead and
victim-karma are already-resolved inputs — no victim
object, no `IsUndead`/`GetKarma` callbacks, no HolyTouch
catalog/proto ID):

```text
undead victim:  damage = damage * 2
non-undead:     damage = damage + ((-victimKarma) * damage) / 200
```

- C integer truncation toward zero on the `/200`
  (64-bit SIGNED intermediate; negative karma is an
  ordinary resolved input, NOT an error).
- Consequences: negative karma → more damage, zero karma
  → unchanged, positive karma → less damage
  (≈ ±50% at ±100 karma); undead → exactly double with
  the karma path skipped. No karma lookup in T3b.

##### 9.3b.7 Illusionary Wounds victim and base formulas

Immutable victim snapshot (conceptually
`IllusionaryVictim{Kind PlayerOrMonster, Intellect,
Difficulty, MaxHP, HP}`; only the selected kind's fields
are consumed; no live entity pointer):

- Player: `MaxHP` = source `GetMaxHealth` (BUFFED max
  health, NOT BaseMaxHP); `Intellect` = already-resolved
  effective Intellect within the source-valid domain
  (hostile huge integers MUST NOT overflow silently —
  validate or bound before multiplying).
- Monster: `MaxHP` = source `ReturnMaxHitPoints`;
  `Difficulty` = already-resolved difficulty.
- Living target only: `HP >= 1`, `MaxHP >= 1`;
  otherwise `ErrInvalidSpecialVictim`.

Source `illwound.kod GetHPLoss` frozen exactly:

```text
Player:   baseDamage = 17 + (50 - Intellect) / 10
Monster:  baseDamage = 30 - bound(Difficulty * 2, 1, 20)
loss = (baseDamage * spellPower) / 100
```

- Divisor is literally `100`, NOT 99. `Difficulty*2`
  uses 64-bit signed math before the `bound(...,1,20)`.
  Spell power is T3a `1..99`.

##### 9.3b.8 Illusionary Wounds caps, policy, duration, refund

Binding cap order (source: MaxHP/3 first, then HP−1):

```text
loss = bound(loss, 0, MaxHP/3)     (floor division)
loss = bound(loss, 0, HP-1)
```

Thus: never lethal (HP=1 → loss 0); CAN return 0 (low
power, high Intellect/Difficulty, low HP, floor
division). Do NOT call `ApplyPlayerDamageCaps`,
`ApplyResistance`, or `ApplyDefenseModifiers`: this is
the absolute path (`pbAbsolute = TRUE`; source player
`AssessDamage` skips defense+resistance+bonus+floor-1+
both-caps; monster `AssessDamage` skips resistance).
T3b produces the loss value with `PolicyAbsolute` only;
T4/T7 own HP application.

Source `GetHPLoss` exposes an optional final `iFactor`
division (`if iFactor > 1 { loss = loss / iFactor }`
after both caps). Exhaustive caller audit: NO live
MVP-relevant caller passes `iFactor > 1` (both call
sites — direct cast and illusionary wall — omit it).
Frozen: Voxilian T3b implements the reachable factor-1
behavior ONLY (division skipped); no generic public
factor parameter mirrors unreachable source API.

Duration (source `GetDuration`):

```text
durationMs = 20000 + spellPower * 750
durationMs = bound(durationMs, 20000, 80000)   (20 s..80 s)
```

64-bit intermediate; spell power T3a `1..99`
(power 99 → 94250 → capped 80000). Value-only, no timer.

Refund (source `StartEnchantment #state=iDamage` +
`EndEnchantmentEffects`): the INITIALLY APPLIED loss
becomes enchantment state; on expiration, if the victim
is alive restore that STORED applied amount, else
restore 0. Never a re-roll, never a re-computation.
T3b exposes conceptually
`IllusionaryRefund(appliedLoss int, victimAlive bool)
int` (or encodes it in a result value): no healing
mutation, no timer state, no enchantment map.

Source randomly chooses an apparent elemental spell for
presentation/messages; this does NOT affect the absolute
arithmetic. T3b MUST NOT implement random fake-spell
selection, localized text, sound, or animation.

##### 9.3b.9 Vampiric Drain side effect

Vampiric Drain uses the GENERIC T3a AttackSpell pipeline
(source `vampdrn.kod`: `piDamageMin/piDamageMax =
12/18`, `piManaFocusBonus = 0` — the focus `+1` still
applies when focused per §9.3a.15; no `CastSpell`
override). T3b owns ONLY the heal number, computed from
the POST-application damage result (source `DoSideEffect`
receives `iDamage` AFTER `AssessDamage`; `$` = killed).
Future runtime order: T3a raw → T2 mitigation/resistance
→ T1 applicable caps → T4 apply damage → T3b heal
formula from the applied result → T4 apply healing.

Source `DoSideEffect` frozen (divisor constant
`DAMAGE_FACTOR_TO_HEAL = 2`):

```text
nonlethal appliedDamage:  heal = bound(appliedDamage / 2, 1, $)
lethal (killed):          heal = bound(resolvedBaseDamageMax / 2, 1, $)
```

- Voxilian uses an explicit `killed bool` (never nil
  magic). Integer truncation: applied 0→1, 1→1, 2→1,
  3→1, 4→2, 17→8. The lethal path IGNORES the applied
  scalar and uses the caller-supplied resolved prototype
  max (source Vampiric Drain `piDamageMax = 18` → heal 9;
  `18` is a TEST vector only, never production catalog
  content). No HP state, no `GainHealth`, no T3a raw
  damage duplication in T3b.

##### 9.3b.10 Earthquake severity and squared-distance falloff

Source normal cast (`earthqua.kod`): `viMin_damage = 5`,
`viMax_damage = 9` (mechanics constants for this
archetype, not a catalog spell ID).

```text
severity = 1 + spellPower / 25        (integer division)
```

Spell power T3a `1..99`; no invented clamp. Golden
boundaries: 24→1, 25→2, 49→2, 50→3, 74→3, 75→4, 99→4.
Invalid severity inputs to downstream helpers are
`ErrInvalidEarthquakeSeverity` (severity < 1).

Source constants: full-damage distance 8, zero-damage
distance 20; input is already-resolved SQUARED distance
(no sqrt, no float; negative is
`ErrInvalidSquaredDistance`):

```text
if squaredDistance <= 64:   percent = 100
else if squaredDistance > 400: percent = 0
else: percent = 100 * (400 - squaredDistance) / (400 - 64)
```

- Integer truncation AFTER the `100*(...)` multiplication
  (64-bit). Exactly 400 yields 0 via the interpolation
  branch; above 400 is also 0. Full table: 0→100,
  64→100, 65→interpolated, mid literal required, 399→,
  400→0, 401→0. Tests MUST include vectors that fail if
  linear distance is used instead of squared distance.

##### 9.3b.11 Earthquake damage modes

Caster mode (source `ComputeDamage`), operation order
binding:

```text
roll = inclusive Random(5, 9)
damage = (roll * severity) * percent / 100
```

Left-to-right: `(roll*severity)` first (64-bit), then
`*percent`, then ONE truncation at `/100`. Zero percent
may produce zero intermediate damage — do NOT floor to
1 in T3b (T2/T1/runtime own later stages).

Environmental mode (source, no caster position):

```text
damage = inclusive Random(5, 9) * severity
```

Item-cast self damage (source scroll-punishment rule):
for a PLAYER using an Earthquake item, caster
self-damage is `MaxDamage * severity` with `MaxDamage =
9` (mechanics constant, not a catalog ID) — NO RNG for
this self-hit. Normal non-item player self-damage uses
caster-mode `ComputeDamage` at squared distance zero
instead. Freeze the distinction; represent
caster/environmental modes explicitly. No room object or
blame target in T3b.

Signature/policy: `viAttack_spell = SPELL_ALL +
QUAKE` (0x0001+0x0080); weapon domain none; policy
ORDINARY (all source `AssessDamage` calls omit
`#absolute=TRUE`). Therefore T2 armor DamageReduce is
bypassed as pure spell, T2 resistance still applies,
and normal player caps may later apply. No absolute
behavior.

World behavior OUT (lives in source, NOT ported): room
target enumeration, guildhall foyer filtering, boss
exceptions, NPC/shopkeeper filtering, rumble/cast
disruption, messages, kill attribution, player flags.
T3b returns damage math only.

##### 9.3b.12 Wall mechanics domain

Pure mechanics for three source wall archetypes only —
ordinary fire, ordinary lightning, illusionary fire —
via a small mechanics enum (conceptually
`WallDamageKind{WallFire, WallLightning,
WallIllusionaryFire}`). This is NOT a spell/proto/item/
database/protocol ID; no named catalog table in sim; no
names/descriptions/resources. Additional formations
(e.g. Ring of Flames) may later reuse these generic
primitives with content-supplied values; do NOT expand
this task into every WallSpell subclass.

Placement arithmetic (source spell side):

```text
Fire:       maxDamage = spellPower / 6,  bound(maxDamage, 1, 16)
Lightning:  maxDamage = spellPower / 4,  bound(maxDamage, 1, 25)
```

Pin fire vectors power 1/5/6/95/96/99 (truncation +
clamps) and lightning low/ordinary/upper-bound vectors.

Illusionary Firewall does NOT convert power to
ordinary wall `maxDamage`: it passes `spellPower`
DIRECTLY to the illusionary WallOfFire element, later
consumed as Illusionary-Wounds spell power. Never run
`power/6` on the illusionary wall (§9.3b.14 reuses IW).

Base lifetimes (placement; NOT yet final element
lifetime):

```text
Fire/Illusionary:  baseDurationSeconds = spellPower * 2 + 30,
                   bound(..., 30, 180)
Lightning:         baseDurationSeconds = spellPower * 2 + 20,
                   bound(..., 20, 120)
```

Element final lifetime (active fire/lightning AND both
passive fillers apply the constructor jitter; source
`GetDuration`/inline constructor):

```text
seconds = inclusive Random(baseDurationSeconds - 20,
                           baseDurationSeconds + 20)
durationMs = seconds * 1000
durationMs = bound(durationMs, 30000, 200000)
```

Pure function over injected RNG; no timer, no
`time.Now`. Note the 30 s floor matters at the low
end (spell-side durations reach 20 s).

Periodic interval (active elements; source
`EFFECT_INTERVAL = 1500 ms`): every period draws a fresh
`percent = inclusive Random(90, 110)` and returns
`delayMs = (1500 * percent) / 100` — exact range
1350..1650 ms with integer source arithmetic. No
scheduler, no ticker, no goroutine: return the next
delay value only. Each source random operation gets
exactly one RNG draw (lifetime/period/damage draws are
never combined or pre-rolled).

Ordinary wall damage (fire/lightning active element
effect):

```text
rawDamage = inclusive Random(0, maxDamage)
```

Future runtime applies ordinary T2 spell resistance
(Fire → FIRE signature, Lightning → SHOCK signature);
policy ORDINARY, no absolute flag. Zero is a legitimate
pre-application value — T3b MUST NOT floor it to 1. Do
not run T2 inside the wall primitive; cross-stage tests
compose it separately.

Active vs passive elements: source 9-element placement
alternates 5 active damaging elements + 4 passive
filler elements (`PassiveWallofFire`,
`PassiveWallofLightning`); passives provide
appearance/lifetime only and NEVER run the active
periodic damage path. Freeze the distinction; do NOT
implement eight-direction placement geometry, rows/
cols, angles, or world objects.

Once-per-period contract (for future runtime; T3b
documents it and exposes only pure policy values —
NO `map[EntityID]`, mutex, timer queue, scheduler, or
wall registry in T3b): an active wall element affects a
target at most once per wall-element period;
movement-triggered effect and periodic scan share that
suppression domain; the affected set clears at the
beginning of each periodic cycle.

Wall targeting policy is DEFERRED runtime (documented,
not implemented, no live room/player objects in any
wall math function): active walls do not affect their
caster by default; target must be in range + LOS;
NO_COMBAT/NO_MOB_COMBAT can suppress; the player
moved-since-entry gate applies; player safety/
AllowPlayerAttack can suppress harmful effects; a
suppressed player is still considered affected for
that cycle.

##### 9.3b.13 Illusionary wall damage

Source element threshold (exact):

```text
if spellPower < 35:  no illusionary damage effect
```

Eligible at exactly 35. Test 34 → no effect, 35 →
effect path.

When eligible:

```text
maxIllusionaryLoss = IllusionaryWoundsLoss(victim, spellPower)
if maxIllusionaryLoss <= 0:  no damage/enchantment effect
else: rawLoss = inclusive Random(0, maxIllusionaryLoss)
```

- Policy ABSOLUTE (source `#absolute=TRUE`):
  ordinary numeric resistance/caps bypassed.
- MANDATORY stage ownership: the illusionary wall MUST
  call/reuse the production IW loss primitive — no
  copy/paste of the IW formula inside wall code (a test
  proves the shared path).
- The refund-state amount derives from the ACTUALLY
  APPLIED absolute loss (the rolled `rawLoss` as passed
  to future application), never from the maximum
  possible loss. Use the regular IW duration for that
  spell power. No enchantment is started in T3b.
- Non-lethal by construction (`maxIllusionaryLoss <=
  HP-1`, `rawLoss <= maxIllusionaryLoss`): prove the
  property; do NOT add a second arbitrary non-lethal
  cap.

Wall presentation OUT: burn/shock text, sounds, light
source, animation, outlaw warning prose, dissipate
prose. Domain results only.

##### 9.3b.14 Stable domain errors

Reuse existing errors where ownership is exact:
`ErrInvalidSpellPower`, `ErrInvalidCombatStat`,
`ErrInvalidSquaredDistance`, `ErrInvalidDamageValue`,
`ErrInvalidRange`, `ErrNilRNG`, `ErrInvalidCastTiming`.
Narrow T3b additions only where genuinely needed:

```text
ErrInvalidSpecialVictim      (impossible IW victim snapshot:
                              unknown kind, MaxHP < 1, HP < 1)
ErrInvalidWallKind           (unknown WallDamageKind value)
ErrInvalidEarthquakeSeverity (severity < 1)
ErrInvalidWallLifetime       (impossible wall lifetime input:
                              negative base seconds)
```

`errors.Is` matching; no string parsing. Exact names
flexible but frozen once implemented.

##### 9.3b.15 Golden vectors and property invariants (normative minimum)

Touch: proficiency Punch-wins / Mysticism-wins /
equality / odd-Mysticism truncation (`(m*3)/2`
before max); damage min/max rolls at power 1/50/99
plus an odd raw roll proving half-before-multiply;
one REAL `TouchProficiency → PlayerOffense →
RollHit` composition; one REAL touch raw →
`ClassifyDamageClass` (mixed) →
`ApplyDefenseModifiers` (existing T2 2/3 behavior) →
resistance composition. Holy Touch: undead 2x;
karma negative/zero/positive; one /200-truncation
vector; overflow-safe signed proof.

Illusionary Wounds: player Intellect 50 / lower
Intellect at power 1/50/99; monster low/ordinary/
high-clipped difficulty; caps MaxHP/3 boundary,
HP−1 tighter, HP=1 → 0; duration power 1 /
80 s-cap threshold / 99; refund alive→exact,
dead→0. Properties: loss ≥ 0, ≤ floor(MaxHP/3),
≤ HP−1, never lethal; duration 20 s..80 s.

Vampiric Drain: applied 0→1, 1→1, 2→1, 3→1, 4→2,
17→8; lethal base-max 18→9 with applied scalar
ignored. Heal ≥ 1 on a valid side-effect event.
Cross-stage test: T3a raw → T2 resistance →
test-only applied → T3b heal, with a raw-direct
control proving order matters.

Earthquake: severity 24→1, 25→2, 49→2, 50→3, 74→3,
75→4, 99→4; squared 0/64→100, 65→interpolated,
mid literal, 399, 400→0, 401→0 (with linear-distance
killers); damage forced rolls 5/9, zero-percent
result, environmental mode, normal self at sq=0,
item self `9*severity`. Properties: percent 0..100;
sq≤64→100; sq≥400→0; monotonic non-increasing in
distance. One REAL quake raw → T2 pure-spell class
→ quake resistance composition (armor bypassed,
resistance applies; no T3b cap).

Walls: fire maxDamage power 1/5/6/95/96/99;
lightning low/ordinary/upper; base durations
fire/lightning/illusionary; final lifetime forced
min/max jitter + 30 s clamp + upper behavior;
period 90→1350, 100→1500, 110→1650; ordinary
damage forced 0 and forced max; illusionary power
34/35, maxLoss 0, forced random 0/maxLoss.
Properties: delay 1350..1650; ordinary raw
0..maxDamage; illusionary raw ≤ IW max; illusionary
never lethal; same inputs + same scripted RNG →
same output.

T3a/T3b math MUST NOT introduce room goroutines, wall
timers, world-object goroutines per effect,
`time.Sleep`, or per-spell `time.Ticker`. Later
runtime/world integration uses deterministic sim
ownership.

#### 9.3c M5-T7 authoritative attack/cast runtime integration (deferred)

T7 owns: real C→S 103 attack routing; real C→S 104 cast
routing; typed sim-owner combat commands; composition of
T1/T2/T3a/T3b mechanics; authoritative T4 HP/mana/vigor
mutation; T5 death handoff; T6 safety/personal-state
interaction where required; runtime cooldown ownership;
spell/weapon/loadout resolver seams; actual mana/exertion
charging; reagent transaction/inventory seam; damage
application; hit/cast result events; gateway result
transport; scripted-duel integration proof. Depends on
M5-T1, M5-T2, M5-T3a, M5-T3b, M5-T4, M5-T5, M5-T6. Opcode
ownership: 103/104 attack/cast → M5-T7; T1/T2/T3a/T3b own
pure mechanics only and MUST NOT implement 103/104
handlers. The M5 exit gate sits after T7.

#### 9.4 M5-T4a authoritative player-vitals state/mutation core (normative, v0.3.29)

T4a OWNS: the canonical sim-domain player-vitals value; HP/BaseMaxHP/MaxHP
mutation primitives; ordinary and over-max/vamp healing semantics; mana/
max-mana mutation primitives; initial-mana and node-mana arithmetic;
Vigor/rest-threshold/exertion accumulator semantics; pure health/mana/rest
interval calculations; pure lazy stomach-decay calculation; validation and
stable errors; the durable-vitals JSON compatibility contract; golden/
property/fuzz tests. T4a is pure/value: no timers, no goroutines, no clock,
no wall-clock timestamps, no entity fields, no gateway, no Store.

T4a does NOT own (M5-T4b owns ALL of the following, deferred): attaching
vitals to live sim entities; owner-local entity mutation APIs; health/mana
timer scheduling; rest start/stop scheduling; moved-since-entry regen
gating; runtime timer/due-tick metadata; handoff preservation;
deterministic tick/manual-clock integration; dirty/snapshot notification;
inspection/event hooks required by later T7. T4a MUST leave
`internal/sim/entity.go`, `internal/sim/engine.go`, `internal/sim/ingress.go`
and `internal/sim/saver.go` (plus `internal/persist`, `internal/store`,
migrations, queries, generated sqlc) byte-identical. No death/corpse/
respawn (M5-T5); no rest/eat opcodes, no Second Wind execution (M5-T6);
no 103/104 runtime (M5-T7).

Normative source: `kod/object/active/holder/nomoveon/battler/player.kod`
(`NewVigor`, `SetVigorRestThreshold`, `AddExertion`, `RestAddExertion`,
`UpdateStomach`, `HasVigor`, `HealthTimer`, `ManaTimer`,
`CalculateHealthTime`, `CalculateManaTime`, `ReqEatSomething`,
`EatSomething`, `LoseHealth`, `GainHealth`, `GainHealthNormal`,
`NewHealth`, `GainBaseMaxHealth`, `GainMaxHealth`, `GetInitialMaxMana`,
`NewMaxMana`, `ComputeMaxMana`, `LoseMana`, `GainMana`, `NewMana`,
`StartResting`, `StopResting`, `GetRestTime`, `RestTimer`,
`GetStamina`/`GetMysticism`, creation reset),
`kod/object/passive/mananode.kod` (`GetManaAdjust`),
`kod/object/passive/mananode/feynode.kod` (double override),
`kod/object/passive/mananode/avarnode.kod` (no override — standard),
`kod/object/active/portal/corpnode.kod` (standard formula),
`kod/object/passive/spell/jala/restorat.kod` (`AdjustHealthTime`),
`kod/object/passive/spell/jala/rejuven.kod` (`AdjustManaTime`),
`kod/object/passive/spell/jala/invigor.kod` (`AdjustVigorTime`),
`kod/object/passive/spell/focus.kod` (`AdjustManaTime`),
`blakserv/ccode.c` (`C_GetTime`) + `blakserv/btime.h` (`time_t GetTime()`).
Research reference: `docs/meridian59.md` §3–§4 as corrected by the v0.3.29
audit. No GPL KOD/C text is copied: mechanics only are reimplemented. All
integer division truncates toward zero (C semantics); intermediates use
64-bit widths and MUST be overflow-safe; no floats anywhere.

##### 9.4.1 Canonical state and units (frozen)

Conceptually (exact Go types are an implementation choice, but the
constraints are normative):

```text
HP            signed int     current health, >= 0, NO upper invariant
BaseMaxHP     signed int     unbuffed base, 20..150
MaxHP         signed int     buffed max, >= 20, NO upper invariant
Mana          signed int     current mana, >= 0, NO upper invariant
MaxMana       signed int     mana max, >= 1 (T4a validation; source has no
                              explicit bound — see §9.4.3)
Vigor         signed int     visible whole points, 1..200
RestThreshold signed int     10..100
Exertion      signed int64   1/10000-vigor residual, -20000..20000
Stomach       signed int     0..100 at rest (1..100 after any update)
```

- `10000` exertion = 1 vigor. Vigor is whole points only, never
  float/fixed-point. Copying the value MUST produce an independent
  snapshot: no pointers/maps/slices required.
- FORBIDDEN in the vitals value: CharacterID, PG revision, EntityID,
  NetEntityID, session ID, timer handles, `time.Time`, wall-clock
  timestamps, persisted timer deadlines, `last_stomach_update`.
- Effective Stamina/Mysticism arrive as already-resolved immutable inputs
  valid in `1..70` (source `bound(base+mod,1,70)`, `MAXIMUM_STAT=70`);
  outside is a domain error. T4a performs NO attribute lookup and owns NO
  attribute buffs.

##### 9.4.2 Durable JSON compatibility (frozen)

Creation (`internal/character.NewVitals`) persists exactly:

```text
hp, base_max, max, mana, max_mana, vigor, threshold, stomach
```

T4a MUST preserve those exact semantic field names. The authoritative
residual `piExertion` freezes the one canonical extension field:

```text
exertion
```

with `missing exertion -> semantic zero`, so every character already
created decodes without migration (`characters.vitals` is already JSON;
NO Store/schema/query changes in T4a). T4a MUST NOT change the creation
package to force an explicit zero field; omission stays
backward-compatible. JSON tags live on the sim-domain value or a narrow
`internal/sim` codec. No new persisted fields.

##### 9.4.3 Validation domain and REJECTED false invariants (frozen)

Source behavior is intentionally asymmetric. The following are NOT global
invariants and T4a MUST NOT assert them: `HP <= MaxHP`; `HP <= 2*MaxHP`
at all times; `MaxHP >= BaseMaxHP` at all times; `Mana <= MaxMana`.
Vamp/over-max health may exceed MaxHP; changing MaxHP never auto-clamps
HP; uncapped mana gains may exceed MaxMana; MaxHP modifiers move
independently of BaseMaxHP.

`Validate` (or equivalent) rejects impossible/corrupt values:

```text
HP >= 0                        (no upper bound)
BaseMaxHP 20..150
MaxHP >= 20                    (no upper bound; HP untouched by checks)
Mana >= 0                      (no upper bound)
MaxMana >= 1                   (T4a corrupt-reject: source states no bound,
                               but GetInitialMaxMana yields >= 15 and no
                               source path legitimately reaches <= 0;
                               no upper bound imposed)
Vigor 1..200
RestThreshold 10..100
Exertion -20000..20000         (canonical post-mutation residual range)
Stomach 0..100                 (0 only pre-first-update; §9.4.14)
```

##### 9.4.4 Base max HP (frozen)

Source `GainBaseMaxHealth` exactly (`effectiveStamina` already resolved):

```text
newBase = bound(oldBase + amount, 20, 100 + effectiveStamina)
newBase = bound(newBase, <no lower bound>, 150)
```

Returns the new state plus the ACTUAL delta (`newBase - oldBase`); the
delta may be 0 at a floor/ceiling. No advancement decision lives here
(M6 decides when to request +1). The source follow-on
(`GainMaxHealth(amount=actualDelta)`) is runtime composition, NOT part of
this primitive: callers apply `AdjustMaxHP(actualDelta)` separately, and
T4a tests prove that composition. Goldens: `+1` ordinary; negative change;
`20` floor; `100+Stamina` ceiling (Stamina edge values, e.g. 1 → 101);
`150` hard ceiling (e.g. high Stamina); delta-0 cases.

##### 9.4.5 Max HP modifier (frozen)

Source `GainMaxHealth` exactly: `MaxHP = bound(MaxHP + amount, 20, $)`
(lower bound 20 only, no upper). Changing MaxHP does NOT clamp, shift, or
otherwise touch current HP — this preserves existing over-max health (the
commented-out alternatives in source were deliberately rejected upstream).
Returns the actual delta. No enchantment/content lookup. Goldens: increase;
decrease to floor 20 (delta clips); decrease that leaves HP above the new
MaxHP (e.g. HP 25, Max 30 → −15 → Max 20, HP still 25).

##### 9.4.6 Lose health (frozen)

Source `LoseHealth` exactly, minus the trance break (T7 owns "damage
breaks trance"; T4a owns NO live trance state). Requested loss MUST be
non-negative (`ErrInvalidHealthAmount` otherwise):

```text
after = max(before - amount, 0)
applied = before - after
ZeroHP = (after == 0)
```

Returns before/after/applied/ZeroHP (exact API flexible). No death
transition, no corpse, no inventory, no ledger (M5-T5). Decay-vs-damage
classification is a value only where needed (§9.4.7): the timer decay path
MUST be distinguishable from combat damage because it must never break
trance later.

##### 9.4.7 Normal healing (frozen)

Source `GainHealthNormal` exactly. Negative amount is a domain error
(`ErrInvalidHealthAmount`; source merely returns, but T4a validates
explicitly):

```text
if HP > MaxHP: gain = 0, HP unchanged
else: HP += amount, capped at MaxHP
```

Returns the actual gained amount. Goldens: `10/20 +5 → 15 (5)`;
`18/20 +5 → 20 (2)`; `20/20 +5 → 20 (0)`; `25/20 +5 → unchanged (0)`.

##### 9.4.8 Over-max / vamp healing (frozen)

Source `GainHealth` exactly, NEVER merged with §9.4.7. Ceiling `2*MaxHP`
(overflow-safe doubling):

```text
if 2*MaxHP < HP + amount: HP = 2*MaxHP
else: HP += amount
```

Negative amount is `ErrInvalidHealthAmount`. The already-above-`2*Max`
corner (possible after a MaxHP reduction) resolves per source even though
unintuitive: the condition is true, so HP is SET DOWN to `2*MaxHP`; the
reported actual delta (`after - before`) is then negative. Pin it in a
test. No Vampiric Drain formula here (T3b already returns the requested
heal number; T4a only applies it). Goldens: `20/20 +10 → 30`;
`30/20 +20 → 40 (cap)`; the above-corner reduction.

##### 9.4.9 Health timer one-step value (frozen)

Pure one-event decision, NO scheduling (moved-since-entry gating is T4b;
source `HealthTimer` also gates on `PFLAG_MOVED_SINCE_ENTRY`, and
`NewHealth` deletes the timer at exact equality):

```text
HP < MaxHP  -> ordinary +1 (via §9.4.7 GainHealthNormal path)
HP == MaxHP -> no change (timer deleted in source)
HP > MaxHP  -> decay -1 (via §9.4.6 decay path, NOT combat damage)
```

Over-max decay MUST be distinguishable from damage (decay flag/value).

##### 9.4.10 Health regen interval (frozen)

Source `CalculateHealthTime` exactly, operation order and truncation
preserved. Inputs already resolved (`effectiveStamina`,
`factionRegenBonus`, optional Restorate power — see below):

```text
time = ((200 - Vigor)^2 / 6) + 1000
time = ((125 - effectiveStamina) * time) / 100
maxForRate = bound(MaxHP, 40, 100)
time = (time * 100) / maxForRate
time = time - factionRegenBonus
```

 faction bonus is phase 2: T4a accepts it as an already-resolved scalar
(default 0 in tests) and MUST NOT implement factions. Then source room/Jala
handling: iff a Restorate song is active, return
`AdjustHealthTime(time, power)` (source `restorat.kod`:
`time = bound(time,1000,60000)`,
`out = (time * (400 - (40 + power))) / 400`, return
`bound(out,670,60000)`; power is the already-resolved `iSpellPower`).
With no Restorate, return `bound(time,1000,60000)`. RESTORATE constants
`SPELL_POWER_INTERCEPT = 40`, `SPELL_POWER_SLOPE = 400` are frozen as pure
helper inputs, NOT named-spell lookups: T4a exposes e.g.
`ApplyRestorateAdjust(timeMs, spellPower)` with power validated `1..99`
(T3a domain). AUDIT CONCLUSION (frozen): there is NO over-max HP branch in
`CalculateHealthTime` — over-max decay reuses this same interval;
`BOOST_DECAY_TIME = 30000` ms is mana-only (§9.4.12 of this spec — the mana
interval section). No timer creation. Goldens (faction 0, no Jala):
`V100/S25/Max40 → 6665`; low vigor `V1 → 19000`; high vigor `V200 → 2500`;
`Max20 → 6665` (uses 40); `Max150 → 2666` (uses 100); final min
(`V200/S70/Max100 → 550 → 1000`); over-max (`HP30/Max20/... → 6665`,
same interval).

##### 9.4.11 Initial mana (frozen)

Source `GetInitialMaxMana` exactly, no lookup:

```text
InitialMaxMana = 15 + effectiveMysticism / 5     (integer truncation)
```

`effectiveMysticism` already resolved, `1..70`. Goldens: Myst `1 → 15`,
`5 → 16`, `25 → 20`, `50 → 25`.

##### 9.4.12 Node mana (frozen)

Source `mananode.kod GetManaAdjust` exactly:

```text
StandardNodeMana(mysticism) = ((5 + mysticism) / 10) + 3
```

Source `feynode.kod` override exactly (the ONLY double):

```text
DoubleNodeMana = 2 * StandardNodeMana
```

AUDIT CONCLUSION (frozen): `avarnode.kod` does NOT override
`GetManaAdjust` (karma gating only), and `corpnode.kod` repeats the
standard formula — there is NO second independent multiplier. Research
wording that said "Fey/Vale double" is corrected: the special variant is
Fey-only. T4a encodes at most a small mechanics enum
(`Standard`/`Double`); NO node IDs, room IDs, karma rules, ownership, or
meld state (M9/content owns the catalog). Mysticism is the already-resolved
effective value at meld time. Goldens: Myst 25 → standard `+6`, double
`+12`.

##### 9.4.13 Compute max mana (frozen)

Source `ComputeMaxMana` value semantics exactly, over already-resolved
inputs (NO inventory/enchantment/node-list lookups in T4a):

```text
MaxMana = InitialMaxMana(mysticism) + sum(nodeBonuses) + otherResolvedBonus
```

AUDIT CONCLUSION (frozen): source states NO bound on the result
(`NewMaxMana` adds blindly; `ComputeMaxMana` recomputes blindly) — T4a
imposes none on the pure calculation (overflow-safe 64-bit sum). A
separate `AdjustMaxMana(amount)` primitive (signed amount, no bound,
returns actual delta) covers the `NewMaxMana` add path. The node-list
bitmask is EXCLUDED from T4a (M9/content owns node identity; T4b owns
runtime attachment).

##### 9.4.14 Lose mana (frozen)

Source `LoseMana` value semantics exactly: for non-negative requested loss
(negative is `ErrInvalidManaAmount`), `Mana -= amount`, clamped at 0
(source clamps in `NewMana`; T4a clamps in the primitive so the returned
value is always valid). Returns the ACTUAL mana lost
(`amount - max(-(before-amount),0)`; source's uninitialized-nil path when
nothing clamps is NOT reproduced — T4a always returns a number). No spell
gate here: T7 first decides legality, then routes the T3a payment plan
through this primitive.

##### 9.4.15 Gain mana (frozen)

Source `GainMana(amount, bCapped)` exactly, both modes. Negative amount is
`ErrInvalidManaAmount`:

```text
capped=false:   Mana += amount              (may exceed MaxMana; no upper
                                            bound unless source proves one —
                                            audit finds none)
capped=true:    Mana += amount, clamp to MaxMana
```

Returns the actual gained amount (capped path: `amount - (Mana - MaxMana)`
overflow-safe). Goldens: loss `10 -3 → 7 (3)`; `2 -5 → 0 (2)`; capped gain
near max (`18/20 +5 → 20 (2)`); uncapped above max (`18/20 +5 → 23 (5)`).

##### 9.4.16 Mana timer one-step value (frozen)

Pure one-event behavior (source `ManaTimer`; `NewMana` deletes the timer
at exact equality), NO scheduling:

```text
Mana < MaxMana  -> +1 (uncapped §9.4.15 path)
Mana == MaxMana -> no change
Mana > MaxMana  -> -1 (NOT spending)
```

##### 9.4.17 Mana regen interval (frozen)

Source `CalculateManaTime` exactly, order preserved. Over-max first:

```text
if Mana > MaxMana: return BOOST_DECAY_TIME (30000 ms; no modifiers, no bounds)
```

else, over already-resolved inputs (`effectiveMysticism`,
`factionRegenBonus`, optional Rejuvenate/ManaFocus powers):

```text
time = BASE_REGEN_TIME + (25 - effectiveMysticism) * 1000     (BASE = 150000)
time = time * 200 / bound(Vigor, 1, $)        (lower bound ONLY — no upper)
time = time / bound(MaxMana, 1, $)            (lower bound ONLY — no upper)
time = time - factionRegenBonus               (phase 2; resolved scalar, 0 in tests)
time = bound(time, 1000, 60000)
iff Rejuvenate active: time = (time * (200 - power)) / 200    (no clamp in helper)
time = bound(time, 500, 60000)                (only when a room owner exists;
                                              T4a always applies this bound —
                                              frozen simplification: the
                                              poOwner<>$ path is the only one
                                              with content, and the bound is
                                              idempotent on the no-Jala path)
iff Mana Focus active: time = bound((time * (200 - power)) / 200, 500, 60000)
return time
```

Pure helpers with validated `1..99` powers, no spell lookup:
`ApplyRejuvenateAdjust`, `ApplyManaFocusAdjust` (with its `500..60000`
bound), matching `rejuven.kod`/`focus.kod`. Goldens (faction 0, no
enchantments): `Myst25/V100/MM20 → 15000`; low vigor `V1 → 60000`
(clamped from 1500000); high vigor `V200 → 7500`; divisor edge `MM1 →
60000`, `MM200 → 1500`; min edge (`Myst70/V200/MM200 → 525 → 1000`);
over-max → `30000` exactly. Every expected number independently
hand-computed (see §9.4.10/§9.4.17 vectors above).

##### 9.4.18 Has vigor (frozen)

Source exactly: `Vigor > required` — STRICT `>`, never `>=`
(`Vigor10 required9 → true`; `Vigor10 required10 → false`). This is the
authoritative-state counterpart for later T7/T6; T1/T3a cost-plan
arithmetic is NOT duplicated.

##### 9.4.19 General exertion (frozen)

Source `AddExertion` accumulator semantics exactly, AFTER already-resolved
external reductions/blocks. T4a consumes an already-resolved signed amount
plus a generic policy flag; faction percentage reduction, Second Wind
blocking/auto-invocation, and skill lookup are EXCLUDED (M5-T6 owns Second
Wind; factions are phase 2). `MIN_VIGOR_CHANGE = 20000` frozen:

```text
Exertion += amount
if abs(Exertion) > 20000 OR SetToThreshold:
    if SetToThreshold AND Vigor < RestThreshold:
        Vigor = RestThreshold; Exertion = 0
    else:
        vigorLost = Exertion / 10000        (signed, trunc toward zero)
        Vigor -= vigorLost
        Exertion -= vigorLost * 10000       (sub-10000 residual PRESERVED)
    Vigor = bound(Vigor, 1, 200)
```

Critical: `abs(exertion) == 20000` does NOT trigger conversion (strict
`>`). If a `RecoveryAllowed=false`-style generic block is needed it stays
a value-only boolean; no skill lookup. Test `±19999/±20000/±20001`,
residual preservation both signs, large multi-point debits, recovery,
`1`/`200` clamps, and both `SetToThreshold` cases (below threshold snaps
to threshold with residual cleared; already `>=` threshold takes the
ordinary conversion path — e.g. vigor 100, exertion 5000,
SetToThreshold → `vigorLost = 0`, unchanged). Go/C signed truncation
toward zero is explicit in tests.

##### 9.4.20 Rest threshold (frozen)

Source default `80`; source setter `Bound(value,10,100)` with a Second
Wind force-to-10 path (T6 owns the skill; NOT implemented here). T4a
validates explicitly: raw input outside `10..100` is
`ErrInvalidRestThreshold` (no silent clamp — source fidelity does not
require clamping at this value layer; T6 may clamp-then-set at its own
layer if source parity demands it). Goldens pin `10`, `80`, `100`.

##### 9.4.21 Rest-specific exertion (frozen)

Source `RestAddExertion` exactly — a SEPARATE path from §9.4.19 with three
frozen distinctions: (a) no-op while `Vigor > RestThreshold`; (b) a
RESOLVED room multiplier on negative (recovery) amounts — sanctuary `2x`,
triple-heal `3x`, with source assignment order (sanctuary first, then
triple-heal overwrites) so BOTH flags set means `3x`; T4a receives the
multiplier as an already-resolved value (`1`/`2`/`3`; room lookup is OUT);
(c) conversion CLEARS the residual (`Exertion = 0`) instead of preserving
it, and overshoot clamps UP to the threshold:

```text
if Vigor > RestThreshold: no-op (state untouched)
Exertion += amount * resolvedMultiplier   (multiplier applies to the amount;
                                          positive amounts are unmultiplied
                                          per source — flags require amount<0)
if abs(Exertion) > MIN_VIGOR_CHANGE:       (strict >, same constant)
    Vigor -= Exertion / 10000              (signed, trunc toward zero)
    if Vigor > RestThreshold: Vigor = RestThreshold
    Exertion = 0
    Vigor = bound(Vigor, 1, 200)           (via NewVigor)
```

Goldens: above-threshold no-op; ordinary `1x`; sanctuary `2x`; triple `3x`;
strict `20000` boundary (two `-10000` ticks convert nothing; the third
converts); overshoot clamps to threshold; conversion clears residual.

##### 9.4.22 Rest interval (frozen)

Source `GetRestTime` base exactly, from resolved inputs:

```text
timeMs = 1000 + 30 * (51 - effectiveStamina)
```

Source Jala Invigorate handling becomes a pure resolved seam:
`ApplyInvigorateAdjust(timeMs, spellPower) = (time * (200 - power)) / 200`
(`invigor.kod`; no clamp in helper; power validated `1..99`). AUDIT
CONCLUSION (frozen): source states NO final bound on the rest interval —
T4a adds none. No timer, no resting boolean, no Start/Stop runtime state
(T4b owns scheduling; T6 owns opcode 115). Goldens over effective Stamina:
`1 → 2500`, `25 → 1780`, `50 → 1030`, `70 → 430`.

##### 9.4.23 Lazy stomach decay + food preflight (frozen)

Source `UpdateStomach` as a pure function over `(currentStomach,
elapsedWholeSeconds)` — NO clock, NO stored timestamp in T4a (T4b/T6
supply elapsed time deterministically). `FOOD_USE_RATE = 12` frozen.
`GetTime()` is seconds-based server time (`time_t`; the KOD-visible
constant offset cancels in differences), hence whole-second inputs:

```text
decayed = stomach - (elapsedSeconds * 12) / 100    (multiply BEFORE divide)
result = bound(decayed, 1, 100)
```

Negative elapsed is `ErrInvalidElapsedTime`; multiplication is
overflow-safe (64-bit). `stomach=0, elapsed=0` on update yields `1` —
pinned as a regression test (initial-zero vs post-update distinction).
Goldens: `(0,0) → 1`; `(100,0) → 100`; `(100,8) → 100`; `(100,9) → 99`;
`(100,100) → 88`; `(100,833) → 1`; `(1,long) → 1`. Monotonicity
properties: decay never increases; greater elapsed never increases result;
result always `1..100`.

T6 owns eat intent; T4a exposes ONLY the pure capacity check over the
post-update stomach (source `ReqEatSomething` value rule):

```text
allow iff stomach + filling <= 100     (100 passes, 101 fails)
```

No inventory/item/message logic; nutrition→exertion composition is T6.

##### 9.4.24 Creation compatibility (frozen)

`internal/character.NewVitals` emits `HP/BaseMax/Max 20`,
`Mana/MaxMana 15+Myst/5`, `Vigor 100`, `Threshold 80`, `Stomach 0`, no
exertion field. T4a MUST prove (fixture or external-package test, NO
import cycle) that this shape decodes into a valid canonical vitals with
`Exertion = 0`, and that a new encode/decode round-trip preserves a
non-zero residual. The creation package itself is UNTOUCHED.

##### 9.4.25 Damage/heal composition boundary (frozen)

T1/T2/T3b produce values; T4a applies final requested numbers. Future T7
order: attack/spell raw → T2 mitigation/resistance → T1 caps where
applicable → T4 health loss → T5 death iff HP reached zero. Illusionary
Wounds: T3b computes the absolute non-lethal loss → T4 applies it
(non-lethality preserved and proven in a composition test). Vampiric
Drain: T4 applies target loss → actual applied returned → T3b
`VampiricDrainHeal(applied, ...)` → T4 applies caster over-max heal (real
T3b helper in tests). Spell payment: T3a `ResolveSpellPayment` → T4a
`LoseMana` + exertion application, as a TEST composition only — no cast
runtime is implemented.

##### 9.4.26 Stable errors (frozen)

Narrow `errors.Is` sentinels (exact Go names flexible; reuse existing sim
sentinels where semantically exact — `ErrInvalidExertion` is reused for
exertion-amount domain errors): `ErrInvalidVitals`, `ErrInvalidHealthAmount`,
`ErrInvalidManaAmount`, `ErrInvalidExertion` (reuse), `ErrInvalidRestThreshold`,
`ErrInvalidElapsedTime`, `ErrInvalidNodeKind` (only if the enum needs it).
No string parsing.

##### 9.4.27 Test minimums (frozen)

Pure value tests only: no sleep, no goroutine, no timer, no fake clock
(the old M5-T4 "timer tests" wording moves to T4b). Required goldens:
§9.4.3 creation/validation vectors (Myst `1 → 15`, `5 → 16`, `25 → 20`,
`50 → 25`; `HP/BaseMax/Max 20`, `Vigor 100`, `Threshold 80`, `Exertion 0`,
`Stomach 0`; old-JSON-missing-exertion and round-trip); §9.4.4–§9.4.5
base/max vectors; §9.4.6–§9.4.8 loss/heal vectors incl. the
already-above-`2*Max` corner; §9.4.11–§9.4.16 mana vectors; §9.4.18–§9.4.19
`HasVigor` strictness and `±19999/±20000/±20001` boundaries with residual
proofs; §9.4.20–§9.4.22 threshold/rest vectors incl. `1x/2x/3x` and
Stamina `1/25/50/70`; §9.4.10/§9.4.17 regen-time vectors (every number
hand-computed above); §9.4.23 stomach vectors + capacity `100/101`.
Property invariants: loss never yields `HP < 0`; normal heal from
`<= Max` never exceeds Max and never lowers over-max HP; base stays
`20..min(100+Stamina,150)`; vigor mutations end `1..200`; threshold
`10..100`; residual in range; mana never negative; stomach update
`1..100` and monotone; intervals positive and bounded; determinism
(same input → same result). NO false-invariant assertions (`HP<=MaxHP`
etc.). Fuzz SEEDS (no long campaign) for the cheap primitives with
no-panic/no-overflow/stable-error/bounds properties.

### 9.4b M5-T4b runtime vitals integration + scheduling (normative v0.3.30, T4b2 composition freeze v0.3.31)

This section freezes the T4b runtime: attaching the §9.4 vitals value to
live sim entities, owner-local mutation composition over the T4a
primitives, and the deterministic health/mana/rest/stomach runtime. It
splits the old single broad T4b row into TWO tasks (v1.14):

```text
M5-T4b1 — live player-vitals entity integration (§9.4b.2–§9.4b.9,
          §9.4b.18–§9.4b.20; §9.4b.8/§9.4b.9 are the T4b1 proof base
          that T4b2 extends)
M5-T4b2 — deterministic vitals scheduling/runtime (§9.4b.1, §9.4b.10–
          §9.4b.17, §9.4b.19, §9.4b.21)
```

Normative source audited for this freeze (mechanics only, no GPL text
copied): `player.kod` (`HealthTimer`, `NewHealth`, `CalculateHealthTime`,
`ManaTimer`, `NewMana`, `CalculateManaTime`, `StartResting`, `StopResting`,
`IsResting`, `RestTimer`, `GetRestTime`, `RestAddExertion`, `UpdateStomach`,
`NewOwner`, logon flag reset), `player/user.kod` (`UserMove`, `UserTurn`,
`NotifyMonstersOfPresence`, `VIGOR_RUN_THRESHOLD`), `blakserv/timer.c`
(`CreateTimer`, `AddTimerNode`, `TimerActivate`),
`blakserv/ccode.c` (`C_GetTime`).

#### 9.4b.1 Runtime architecture (frozen)

T4b stays inside the existing single-writer simulation architecture
(§5.2/§5.2.10): one Run goroutine owns ALL mutation; health/mana/rest
progress is sim-owned deterministic runtime metadata processed by
`Engine.Step`. FORBIDDEN in T4b: goroutine per player, ticker per player,
`time.Timer` per player, wall-clock gameplay deadlines, background regen
worker, lock per entity. The existing engine policy is authoritative: one
delivered engine tick executes exactly one `Step`; no wall-clock catch-up
burst (§5.2.1). Gameplay timer progress follows simulation ticks, never
elapsed production wall time. `Run`/`Step` remain the ONLY step cores; no
second timer-driven simulation path may exist.

#### 9.4b.2 Player-entity classification [T4b1]

- The internal entity gains player-owned state ONLY: a player
  classification flag plus (for players) one authoritative `PlayerVitals`
  VALUE (§9.4.1). A generic entity created through the existing M4 path
  (registry `AddEntity`/`EnqueueAddEntity`) remains byte-for-byte generic:
  no vitals, no classification, unchanged behavior. No timer, rest, or
  acted-since-entry state exists yet in T4b1 (T4b2 adds the §9.4b.10
  deadline slots, §9.4b.15 stomach anchor, and §9.4b.16 acted flag as
  further player-owned fields on the SAME entity object).
- `EntityID` remains the opaque §5.2.5 domain: T4b1 MUST NOT reinterpret it
  as a CharacterID, and MUST NOT add CharacterID, PG revision, session ID,
  NetEntityID, or persistence ownership to `PlayerVitals` or the entity.
- `EntitySnapshot` gains an `IsPlayer` classification boolean (additive;
  default false preserves every existing construction site). Immutable
  vitals inspection is a separate value-returning API (§9.4b.4).

#### 9.4b.3 Player attach/add/load path [T4b1]

- T4b1 provides an explicit owner-local player-entity creation path,
  conceptually `AddPlayerEntity(pos, vitals) (EntitySnapshot, error)`:
  validates the vitals (`Validate`), computes the cell, allocates a fresh
  EntityID exactly like the generic add (same all-or-nothing semantics,
  same invalid-position/ID-exhaustion errors), and installs a VALUE COPY of
  the vitals plus the player classification. The caller's copy is never
  aliased: later caller mutation cannot affect live state, and live
  mutation cannot leak through returned values. T4b1 creates NO timers,
  NO rest state, NO acted-since-entry state.
- T4b1 also provides owner-local attachment to an existing generic entity,
  conceptually `AttachPlayerVitals(id, vitals) error`: same validation and
  value-copy rules; unknown IDs return the existing `ErrEntityNotFound`;
  attaching to an entity that is already a player returns a stable
  `ErrEntityAlreadyPlayer` with zero mutation (re-load/re-attach is NOT a
  T4b1 concept; respawn composes remove+add later). Attaching during
  MIGRATING ownership fails with zero mutation (the migration record owns
  the quiesced entity; §5.4.2 forbids source-side gameplay mutation).
- Ingress boundary: T4b1 adds NO new ingress command kind. The mailbox
  keeps exactly add/remove/move (§5.2.10). A typed concurrent
  player-add/attach command is deferred to the later gateway world-entry
  composition task that actually holds durable vitals (M5-T6/T7 era);
  when added it MUST be a typed command carrying a `PlayerVitals` value,
  never a `func(*Engine)` escape hatch.

#### 9.4b.3a T4b2-complete atomic player initialization [T4b2, v0.3.31]

At the T4b2-complete boundary a newly attached live player MUST NOT
exist in an accidentally-unscheduled state: the §9.4b.9 initial state
is installed ATOMICALLY with attach. The production-facing semantics
are equivalent to the add/attach paths taking the §9.4b.14a runtime
inputs alongside the vitals:

```text
AddPlayerEntity(pos, vitals, runtimeInputs)
AttachPlayerVitals(id, vitals, runtimeInputs)
```

(or one equally atomic replacement API — the requirement is that
production world-entry composition can never yield a player between
steps with vitals attached but runtime metadata uninitialized).

Rules:

- BOTH the vitals AND the runtime inputs validate BEFORE a new
  EntityID is consumed (same all-or-nothing contract as §9.4b.3; an
  invalid runtime-input value allocates no ID and mutates nothing).
- NO default Stamina/Mysticism is invented (e.g. no implicit 25): the
  caller supplies resolved values (§9.4b.14a domain 1..70).
- Initial runtime state at the current simulation tick:
  `vitals` = supplied validated value; `runtimeInputs` = supplied
  validated value; `actedSinceEntry = false`; rest deadline absent;
  `stomachAnchorTick = current sim tick`; health deadline armed iff
  `HP != MaxHP && HP > 0` (NewHealth create semantics applied
  immediately); mana deadline armed iff `Mana != MaxMana` (NewMana
  create semantics applied immediately). Deadlines arm from the
  current tick using the supplied runtime inputs.
- No runtime metadata is persisted (§9.4b.9). Remove/re-add/re-attach
  re-anchors at the then-current tick; there is no offline digestion
  or offline regen.

#### 9.4b.4 Immutable inspection [T4b1]

- Conceptually `PlayerVitalsOf(id) (PlayerVitals, bool, error)`: unknown
  ID -> (`ErrEntityNotFound`); known generic entity -> (zero value, false,
  nil); known player -> (value COPY, true, nil). The bool distinguishes
  "not a player" from a VALID player whose vitals legitimately contain
  zero-valued fields (e.g. creation `Stomach 0`, `Exertion 0`): zero
  fields MUST NOT be treated as absent vitals. No mutable pointer into
  the registry escapes.

#### 9.4b.5 Owner-local mutation surface [T4b1]

T4b1 exposes the minimum coherent owner-local mutation families later
T5/T6/T7/T4b2 compose over; exact Go grouping may differ but the families
are frozen. EVERY mutation: (a) executes only on the sim owner goroutine
(owner-local, same contract as `SubmitMove`); (b) resolves the entity,
requiring a player — a generic entity returns stable `ErrEntityNotPlayer`,
an unknown ID `ErrEntityNotFound`, zero mutation in both cases; (c) calls
the EXISTING T4a production helper — formulas are never reimplemented or
shadowed; (d) writes the resulting value back ONLY on success — a failed
validation/domain error leaves the entity unchanged; (e) returns the real
T4a result values (applied/delta/result structs) plus the post-mutation
immutable vitals copy; (f) emits at most one §9.4b.6 event.

```text
HP:            PlayerLoseHealth(id, amount, decay)  -> LoseHealth result
               PlayerGainHealthNormal(id, amount)   -> actual gained
               PlayerGainHealthOvercap(id, amount)  -> actual delta
               PlayerAdjustBaseMaxHP(id, amount, effectiveStamina) -> delta
               PlayerAdjustMaxHP(id, amount)        -> actual delta
Mana:          PlayerLoseMana(id, amount)           -> actual lost
               PlayerGainMana(id, amount, capped)   -> actual gained
               PlayerAdjustMaxMana(id, amount)      -> actual delta
Exertion:      PlayerApplyExertion(id, amount, setToThreshold)
               PlayerApplyRestExertion(id, amount, roomMultiplier)
Threshold:     PlayerSetRestThreshold(id, threshold)
```

T4a error sentinels propagate predictably (wrapped `%w`, matched with
`errors.Is`). T4b1 owns NO death transition, NO timer scheduling, NO rest
runtime, NO spell/weapon resolver, NO gateway.

#### 9.4b.6 Dirty/event seam [T4b1]

- One narrow observer seam, conceptually
  `PlayerVitalsObserver{ OnPlayerVitalsChange(PlayerVitalsEvent) }`,
  an OPTIONAL `EngineDeps` member (nil = no-op, mirroring
  Movement/Anomaly). The event carries ONLY immutable values:
  `EntityID`, `Before`, `After` (two `PlayerVitals` copies).
- Fired iff the mutation produced a REAL state change (`Before !=
  After` — the struct is a comparable plain value). A T4a no-op result
  (e.g. `GainHealthNormal` gain 0, over-cap heal delta 0,
  `AdjustMaxHP` delta 0) MUST NOT fire. A failed mutation MUST NOT fire.
- The observer owns NO persistence and MUST be non-blocking/bounded on
  the sim owner (same contract class as `MovementSink`). Durable
  CharacterID/revision mapping happens OUTSIDE the vitals value in later
  composition (T7/saver); T4b1 imports NO `store`/`persist`/pgx/sqlc and
  adds NO Prometheus metrics.

#### 9.4b.7 Player run gate [T4b1]

- Source `VIGOR_RUN_THRESHOLD = 10`: running is denied iff
  `GetVigor() < 10`, i.e. `CanRun := Vigor >= 10`. This is NOT
  `HasVigor(10)` (§9.4.18 is strict `>` and would wrongly deny vigor
  exactly 10). The constant is frozen once in the sim package (e.g.
  `VigorRunThreshold = 10`) and reused; no second copy.
- Movement selection consults, per entity: player entity -> its
  authoritative current Vigor via the frozen rule; generic entity -> the
  injected M4 `RunGate` UNCHANGED. Run denial still only falls back to
  walk (§5.3.3); no rejection, no vigor mutation in the gate.

#### 9.4b.8 Handoff preservation [T4b1 proof base, T4b2 extension]

- M4 handoff transfers the SAME entity object (§5.4.5), so all
  player-owned state rides along by construction. T4b1 proves for a
  player crossing a cell boundary: identical `PlayerVitals` (deep value
  equality), retained player classification, no copy/reset/alias of
  vitals, single ownership generation bump. T4b2 additionally proves the
  §9.4b.10/§9.4b.16 runtime metadata survives with no timer duplication
  and no same-tick double firing (a migrated-in entity is outside the
  tick-start worklist, §5.4.3, and due processing is per-tick-per-slot).

#### 9.4b.9 Attach/logon initial state [T4b1 for fields, T4b2 for timers]

At attach/logon of a loaded player the runtime state initializes to:

```text
PlayerVitals       loaded value, Validate()d (attach rejects invalid)
resting            false           [T4b2]
actedSinceEntry    false           [T4b2] (source: logon flag reset)
health deadline    per NewHealth from loaded HP/MaxHP   [T4b2]
mana deadline      per NewMana from loaded Mana/MaxMana [T4b2]
stomach anchor     current simulation time             [T4b2]
```

Rest/timer/acted/stomach-anchor metadata is NEVER persisted merely to
simplify runtime code; only the §9.4.2 durable JSON fields exist.

#### 9.4b.10 Runtime timer metadata model [T4b2]

- Health/mana/rest progress is represented as sim-owned per-player
  deadline slots on the entity, conceptually three optional due-tick
  fields (healthDue, manaDue, restDue) in the `uint32` tick domain. No
  goroutine, no heap, no global timer list, no wall-clock timestamp.
- v0.3.31: slot ABSENCE MUST NOT be encoded as `due == 0` — a due tick
  of `0` is a valid future deadline after uint32 wrap (§5.2.2 has no
  "tick 0 is invalid" rule). Each slot carries an explicit
  armed/present bit (or an equivalent small optional-deadline value).
  `restArmed` IS the resting state; no redundant independent resting
  boolean may exist that can disagree with the deadline's presence.
  Runtime metadata fields are plain values (no pointers/maps/slices,
  no `time.Time`, no timer handle, no durable ID/revision/session ID)
  riding on the SAME entity object through cell handoff.
- ONE canonical ms->tick conversion exists, the §9.3a.13 `CastTicks`
  semantics: `delayTicks = ceil(intervalMs * tickHz / 1000)` with
  integer-only arithmetic, `tickHz` domain `1..120`, overflow and
  `> MaxInt32` rejected, never firing earlier than the source interval.
  T4b2 reuses this ONE conversion (calling the existing helper or
  promoting it to a shared name) and MUST NOT introduce a second
  subtly-different ms->tick conversion anywhere.
- When a slot is newly created or re-armed from an absent/cancelled
  state: `due = currentSimTick + delayTicks` (`uint32` addition; wrap is
  normal §5.2.2 behavior). The new deadline is based on the tick at
  which creation/re-arm occurs — NOT the previous deadline and NOT wall
  clock. Due comparison uses the existing canonical `serial32`
  arithmetic (`After(due, currentTick)`), unambiguous because
  `CastTicks` bounds every delay to `< 2^31` ticks; no second comparator
  may be invented.
- At most ONE event per slot per Step: processing clears the slot FIRST
  (the source timer.c removes the timer node before dispatching, and the
  KOD handlers null their handles on entry), applies at most one §9.4b
  step, then applies create/cancel/keep semantics. NO catch-up loop may
  execute multiple missed regen events in one Step; if Steps were not
  delivered, the deadline simply compares as due on the next executed
  Step (single event).

#### 9.4b.11 Health timer lifecycle [T4b2]

`NewHealth` semantics frozen exactly (source):

```text
no deadline AND HP != MaxHP AND HP > 0 -> create (CalculateHealthTime inputs)
existing deadline AND HP == MaxHP      -> cancel
existing deadline AND HP != MaxHP      -> KEEP the existing deadline
```

Changing Vigor, effective stats, MaxHP/BaseMaxHP, faction/Jala/Focus
inputs, etc. MUST NOT silently restart a running deadline: changed
inputs affect the NEXT create. At a health deadline (after clearing the
slot):

```text
if actedSinceEntry:
    HP < MaxHP  -> +1 health via the source-faithful heal path
                   (§9.4.7 normal heal; +1 below max is identical to the
                   source GainHealth(1) call site)
    HP > MaxHP  -> decay -1 via §9.4.6, Decay = true (never breaks
                   trance later; distinct from combat damage)
    HP == MaxHP -> no mutation
else:
    no health mutation
```

Then apply NewHealth create/cancel/keep. Source-faithful consequence: an
idle (not-acted) player's health deadline KEEPS re-arming without healing
while HP != MaxHP and HP > 0. Health at zero MUST NOT spontaneously
schedule healing (`HP > 0` create guard); the zero-HP/death boundary is
M5-T5 — T4b2 only guarantees no self-scheduling from zero.

#### 9.4b.12 Mana timer lifecycle [T4b2]

`NewMana` semantics frozen exactly (source; note NO acted-since-entry
gate — mana regen is not action-gated in source):

```text
no deadline AND Mana != MaxMana -> create (CalculateManaTime inputs)
existing deadline AND Mana == MaxMana -> cancel
existing deadline AND Mana != MaxMana -> KEEP existing deadline
```

At a mana deadline (after clearing the slot): `Mana < MaxMana -> +1`
(uncapped gain), `Mana > MaxMana -> -1 decay`, `Mana == MaxMana -> no
mutation` (§9.4.16 one-step). Then apply NewMana create/cancel/keep. The
over-max branch of `CalculateManaTime` (BOOST_DECAY_TIME) naturally
governs the decay re-arm.

#### 9.4b.13 Rest lifecycle [T4b2]

Frozen exactly (source):

```text
Start:  already resting -> no-op (plus source trance break owned by T6/T7)
        not resting    -> create first rest deadline (GetRestTime inputs)
Stop:   cancel deadline if present; resting becomes false
Firing: clear deadline
        if Vigor < RestThreshold: apply ONE RestAddExertion recovery
                                  event (resolved room multiplier)
        ALWAYS create the next deadline from then-current resolved inputs
```

Reaching the threshold does NOT stop resting; the deadline continues and
produces no recovery while `Vigor >= RestThreshold` until explicitly
stopped. `IsResting` == rest deadline present. Opcode 115 start/stop
semantics, messages/animation, trance breaking, Second Wind
execution/recovery blocking remain M5-T6; T4b2 only exposes the
authoritative owner-local lifecycle hooks.

#### 9.4b.14 Resolved runtime regen inputs [T4b2]

Health/mana/rest interval calculations consume ONLY already-resolved
numeric runtime values (§9.4): effective Stamina, effective Mysticism,
faction regen scalar (phase 2, default 0), Restorate power / absent,
Rejuvenate power / absent, Mana Focus power / absent, Invigorate power /
absent, rest recovery multiplier 1/2/3. T4b2 implements NO live faction,
item, Jala song, enchantment, node, or spell-catalog lookup. The resolved
values are supplied as explicit owner-local inputs at each create/re-arm
(computed by the caller of the timer hook — initially T4b2's own
scheduling code with static/config-level defaults, later enriched by
T6/T7/world tasks); no caching layer may serve stale values across a
create. Room policy remains resolved OUTSIDE the sim vitals layer:
sanctuary -> 2x, triple-heal -> 3x, both -> 3x (not 6x) — the caller
resolves the multiplier; T4b2 invents NO `ROOM_*` flag IDs or
`world.VolumeFlags` bit assignments (content/world mapping belongs to its
own later task).

#### 9.4b.14a Runtime-input ownership value [T4b2, v0.3.31]

v0.3.30 says interval calculations consume current already-resolved
values; v0.3.31 freezes WHERE the authoritative current snapshot lives:
one small plain-value runtime input type in sim, conceptually:

```go
type PlayerVitalsRuntimeInputs struct {
    EffectiveStamina   int
    EffectiveMysticism int

    RestoratePower  int // 0 absent, else 1..99
    RejuvenatePower int // 0 absent, else 1..99
    ManaFocusPower  int // 0 absent, else 1..99
    InvigoratePower int // 0 absent, else 1..99

    RestRecoveryMultiplier int // exactly 1, 2, or 3
}
```

Rules:

- Effective Stamina/Mysticism domain `1..70` (source
  `bound(base+mod,1,70)`); power `0` means absent and nonzero means
  `1..99`; the rest recovery multiplier is exactly `1..3`. Validation
  rejects anything else with the existing stable error sentinels
  (§9.4.26 reuse; no string matching).
- Faction regen remains phase 2 and contributes `0` in T4b2; T4b2
  invents NO faction state or API. No named-spell lookup, no room flag
  lookup, no `world.VolumeFlags` bit assignment, no pointers/maps/
  slices, no durable IDs/revisions/session IDs. Runtime inputs are
  ephemeral sim state and are NOT persisted (§9.4b.9).
- The stored value is the authoritative CURRENT resolved snapshot used
  by T4b2: every health/mana/rest create/re-arm and every rest
  recovery event reads the entity's current value; NO stale secondary
  cache may exist. Later tasks update it when stat modifiers, Jala
  songs, or room policy change, through one owner-local update API,
  conceptually `PlayerSetVitalsRuntimeInputs(id, inputs)`, which
  validates BEFORE storage (hostile/invalid values are rejected before
  any state change; the combination of Validate-valid vitals,
  Validate-valid runtime inputs, and TickHz 1..120 makes every
  interval calculation + CastTicks total, so `Step` can never receive
  an error it cannot return).
- SOURCE TIMING RULE: changing runtime inputs MUST NOT reset or
  restart an already-running health, mana, or rest deadline. New
  values affect ONLY the NEXT health create/re-arm, the NEXT mana
  create/re-arm, the NEXT rest create/re-arm, and the CURRENT rest
  recovery event's room multiplier — the multiplier is read at FIRE
  time because source `RestAddExertion` checks then-current room
  state.

#### 9.4b.15 Stomach sim-time anchor [T4b2]

- T4a owns only `DecayStomach(current, elapsedWholeSeconds)`. T4b2 adds
  ONE ephemeral deterministic per-player anchor: the sim tick of the last
  stomach update (conceptually `uint32 stomachAnchorTick`), initialized
  at attach/logon to the current simulation tick. No wall-clock
  timestamp in `PlayerVitals`, no persisted timer deadline, no DB
  migration, no background stomach timer.
- Lazy update (owner-local, explicit): derive elapsed whole seconds from
  the fixed-step tick domain — `elapsedTicks = currentTick - anchorTick`
  (`uint32` modular subtraction; unambiguous while `< 2^31`, guarded),
  `wholeSeconds = elapsedTicks / tickHz` (floor, exact for fixed steps) —
  then call T4a `DecayStomach`, write the result, and advance the anchor
  by EXACTLY the consumed whole seconds of ticks
  (`anchorTick += wholeSeconds * tickHz`), so repeated updates within
  one simulated second accumulate fractional-tick progress instead of
  discarding it. Wrap behavior is deterministic `uint32` arithmetic in
  the §5.2.2 modulo domain.
- MVP offline behavior (explicit, matches source `Load()`): the anchor
  is NOT durably persisted; unload/restart/offline periods simply
  re-anchor at re-attach, so NO offline digestion is invented or
  applied. Actual opcode 116 eat flow remains M5-T6.

#### 9.4b.16 actedSinceEntry [T4b2]

- The source `PFLAG_MOVED_SINCE_ENTRY` + `NotifyMonstersOfPresence`
  mechanism means FIRST ACTION since room entry — including turning
  (`UserTurn` fires it with zero translation) and many non-movement
  actions (attacks, casts, etc.) — NOT "physical displacement occurred".
  Voxilian names the runtime concept truthfully: `actedSinceEntry`
  (player-owned bool on the entity). T4b2 invents NO wire/room flag bit
  numbers.
- Frozen architecture: fresh player attach/logon starts false; a
  qualifying owner-local player action sets it true through one narrow
  owner-local hook (conceptually `MarkActedSinceEntry(id)`); movement
  integration sets/routes it at the source-faithful point (an ACCEPTED
  processed player movement input — including a zero-translation/
  turn-like accepted input — qualifies, per `UserMove`/`UserTurn`);
  future T6/T7 actions call the same narrow hook; ordinary technical
  32 m CELL handoff PRESERVES it (CELL handoff is NOT Meridian room
  entry); future actual world/room/portal entry may reset it ONLY
  through an explicit hook consuming an ALREADY-RESOLVED reset policy.
- Safe-room boundary (source `NewOwner`): the source resets the flag on
  room entry only when the destination room is NOT no-combat / sanctuary
  / safe-death (and not chaos night). Voxilian `world.VolumeFlags` is
  intentionally opaque with no assigned sanctuary/safe bits, so T4b2
  MUST consume an already-resolved boolean reset decision (or expose the
  narrow hook for the later content-owning task) and MUST NOT assign
  ROOM_SANCTUARY / ROOM_TRIPLE_HEAL / ROOM_NO_COMBAT / ROOM_SAFE_DEATH
  to arbitrary Voxilian bits. `NotifyMonstersOfPresence`'s monster-aggro
  side effect is M7 scope; only the vitals gate is consumed here.

#### 9.4b.17 Deterministic ordering [T4b2]

Source audit (`timer.c`): timers are deadline-ascending; equal
millisecond deadlines fire in creation order (insertion after existing
`<=` nodes, monotonic timer IDs); one timer per activation. Voxilian
freezes the deterministic equivalent: due processing runs as its own
Step phase in canonical engine order (tick-start worklist order: CellCoord
X/Z then EntityID ascending — the same canonical order as movement), and
WITHIN one player the slot order is fixed: health, then mana, then rest.
DOCUMENTED DIVERGENCE: tick quantization collapses distinct millisecond
deadlines onto one sim tick and does not reproduce global cross-entity
creation order; the frozen replacement (canonical entity order + fixed
intra-entity slot order) is deterministic, depends on NO map iteration /
memory address / scheduler order, and is the smallest representation
preserving per-entity source ordering (health-before-mana creation
precedence at attach matches the per-entity slot order). Each slot fires
at most once per Step (§9.4b.10), so equal-tick processing cannot
cascade.

v0.3.31 pins the timer phase's position relative to movement. The
final per-entity Step order is FROZEN as:

```text
1. verify tick-start resident ownership epoch (existing worklist)
2. consume/process movement input
3. integrate movement/collision/handoff
4. finalize + emit MovementUpdate if applicable
5. process the T4b2 player runtime for THIS SAME entity:
       health due, then mana due, then rest due
6. append the position-history sample
```

This remains ONE canonical tick-start worklist in CellCoord/EntityID
order — NO second global map iteration and no timer map walk; the
runtime phase is one small private per-player step called from
`Engine.Step` after movement output and before history sampling, only
when the entity is a player. Consequences (documented AND tested):

1. An ACCEPTED pending player movement input marks
   `actedSinceEntry = true` when that input is CONSUMED in the
   movement phase, before the same Step's due processing — including
   zero-translation/turn-like accepted input (heldDirs may resolve to
   no displacement; it still counts as the first action). Therefore,
   if the first action and a health deadline occur on the same tick,
   the health deadline sees `actedSinceEntry = true`.
2. Movement/run selection happens BEFORE rest recovery: a player at
   Vigor 9 requesting run on the same tick that a rest event raises
   Vigor to >= 10 still WALKS that tick and may run from a later
   tick. This frozen movement-before-vitals ordering is Voxilian's
   explicit replacement for source per-millisecond deadline
   interleaving.
3. Technical cell handoff occurs before timer processing, but the
   SAME entity object is still processed exactly once from its
   tick-start work item: runtime metadata survives, each due event
   fires exactly once, and the re-arm is one deadline (no duplicate
   destination processing — §5.4.3/§9.4b.8).
4. Movement output happens before any vitals event produced by the
   timer phase for that entity on the same Step.
5. A slot newly re-armed during processing is not revisited until a
   later Step (§9.4b.10); a due slot fires once, never N catch-up
   times.

#### 9.4b.18 Run-gate wiring in Step [T4b1]

The per-entity run decision in movement integration consults the player
authoritative gate for players and the injected `RunGate` for generic
entities (§9.4b.7). The gate is consulted ONLY for genuinely moving
controls (existing behavior preserved); no movement code path may call
`HasVigor`.

#### 9.4b.19 Snapshot/inspection boundary (frozen)

`internal/sim` continues to import NO `internal/store`, `internal/persist`,
pgx, or sqlc output. The §9.4b.6 event is the ONLY vitals notification
seam; it carries immutable values and performs no blocking work on the
sim owner. Durable identity/revision mapping and saver composition
remain outside `PlayerVitals` (T7/M4-T4 composition).

#### 9.4b.20 T4b1 test minimums (frozen)

No sleeps, no wall clock, manual clock only where tick delivery is
needed. At minimum: generic entity remains generic (behavior + snapshot);
player attach accepts valid vitals; attach rejects invalid vitals with
zero mutation (no ID consumed for the add form); caller mutation after
attach cannot alias live state; snapshot/inspection copies cannot alias
live state; zero-valued Stomach/Exertion are player vitals, not
"no vitals"; every §9.4b.5 family returns the REAL T4a results
(composition against the production helpers, no formula re-derivation in
tests) including the over-cap negative-delta corners and strict `>20000`
exertion boundary; failed mutation leaves the entity unchanged; generic
entity passed to a player mutation -> `ErrEntityNotPlayer`; unknown
entity -> `ErrEntityNotFound`; vigor 9 walks on a run request / vigor 10
and 11 run / generic entity still delegates to the injected M4 gate;
cross-cell handoff preserves exact vitals and classification; ordinary
generic handoff regression stays green; dirty seam: real change -> exactly
one immutable event, T4a no-op -> no event, failure -> no event.

#### 9.4b.21 T4b2 test minimums (frozen, for the later task)

Manual-tick/manual-clock tests: deadline creation/cancel/keep per
NewHealth/NewMana (including input changes NOT restarting a running
deadline); acted/idle health gating (idle re-arm without healing);
health-at-zero never schedules; mana ungated; rest lifecycle (start
no-op, stop cancel, always re-arm, below-threshold-only recovery,
threshold does not stop rest); due-tick arithmetic incl. u32 wrap and
`< 2^31` delay bounds via the ONE conversion; deterministic ordering
(canonical entity order, fixed slot order, at-most-once per Step);
stomach anchor (fractional-second preservation across repeated lazy
updates, wrap, re-attach re-anchor); handoff preserves all runtime
metadata with no double firing; logon initialization snapshot; the
§9.4b.14a runtime-input validation/ownership matrix; the §9.4b.22
post-commit guard corners; and the §9.4b.17 movement-before-vitals
phase-ordering pin (Vigor-9 run-request-same-tick walk).

#### 9.4b.22 Live post-commit validity guard [T4b2, v0.3.31]

T4a pure/value mechanics stay UNCHANGED: in particular NO bound is
added to the source-faithful pure `AdjustMaxMana`/`ComputeMaxMana`
(the source adds blindly). But a live player entity is canonical
runtime state, so T4b2 freezes: a `PlayerVitals` value may be
committed to a live entity ONLY IF `after.Validate()` succeeds. The
guard is ONE narrow commit check at the §9.4b.6 seam, not scattered
special cases. Consequences:

- every successful T4a output is post-validated before storage;
- if post-validation fails, live state stays bit-identical, NO dirty
  event fires, and the caller receives the validation error;
- this primarily hardens the source-unreachable hostile corner where a
  signed `AdjustMaxMana` request would make `MaxMana < 1` (pinned:
  `MaxMana 20 + (-19) -> MaxMana 1` allowed; `MaxMana 20 + (-20) ->
  rejected`, live MaxMana remains 20);
- this guarantees T4b2 scheduling never receives a live player with an
  invalid MaxMana, so no user-controlled input can create a panic path
  through invalid stored vitals.

### 9.5 M5-T5 death pipeline (normative freeze, v0.3.33)

Source basis: `player.kod` `Killed`/`ApplyDeathPenalties`/`GetDeathCost`/
`SetDeathCost`/`CreateCorpse`/`ChangeSpellAbility`/`ChangeSkillAbility`,
`user.kod` `UserGotoDeadRoom`, `uworld.kod` `LeaveHold`/`ZaptoCorpse`,
`body.kod`, `portlife.kod`, `item.kod` `DropOnDeath`, `settings.kod`
`GetDefaultDeathCost` — audited 2026-09-08 against upstream commit
`095c07b69e957fb5c49593e6ad488b4c64ba088d`; reconciled in
`meridian59.md` §9.5. Source wins over older paraphrases.

#### 9.5.1 Ownership split and the two-phase lifecycle

M5-T5 is FIVE tasks (this section is their shared boundary):

- **T5a — pure/source-faithful death mechanics and immutable plans**
  (§9.5.4–§9.5.14 pure surface; §9.5.17 non-scope).
- **T5b1a — durable death schema + SQL primitives** (§9.5.8a): ONE
  narrow goose migration (`pending_deaths` + `item_pk_protections`)
  plus low-level sqlc primitives. No Store transaction, no sim.
- **T5b1b — durable immediate death-entry transaction** (§9.5.8a,
  §8.1/§8.3 rules): one atomic critical Store operation conceptually
  `CommitDeathEntry(ctx, plan)` covering pending-death recovery state,
  character immediate death state, corpse row, droppable item relocation,
  PK-drop metadata, advancement immediate reset/halve, and the existing
  kills audit row when the resolved killer fits the existing
  character/mob identity domain. NO death/drop ledger row is written:
  T5b1b invents no ledger kind (§9.5.8a is binding) —
  plus stale/crash/commit-ambiguity proof. Depends on T5b1a.
- **T5b2 — durable delayed Underworld-exit penalties** (§9.5.11–§9.5.13):
  one SEPARATE atomic critical Store operation conceptually
  `CommitDeathPenalties(ctx, plan)` covering pending-DeathCost consumption,
  HP/ability penalties, clearing pending-death state, character/ability CAS
  + ledger — plus the same proof obligations.
- **T5c — live runtime + transport integration**: zero-HP → death
  orchestration, dead/Underworld state, resolved world-target seams
  (§9.5.15), rest/regen cancellation/composition, C→S 120, S→C 214/215,
  reconnect/crash recovery, end-to-end lifecycle proof.

Death is TWO durable phases mirroring source. Phase 1 (immediate
real-death entry) and phase 2 (Underworld exit) are separate atomic
critical transactions; NO database transaction remains open between them.
The old phrase "single-txn state+ledger" means each phase's own mutation
is atomic with its audit rows (§8.1) — never one transaction spanning the
lifecycle.

#### 9.5.2 Death disposition: avoided vs cheap vs normal (frozen)

Three dispositions, semantically distinct:

```text
Avoided — not a death: HP set to 1, regen recreated, special-item
          cheap-death activation, RETURN. No corpse, no drop, no
          Underworld, no kill record.
Cheap    — a real death (corpse IS created, death pipeline runs) with
          zero drop/penalty cost (DeathCost = 0): chaos/frenzy night,
          death in the newbie room range, newbie-honor string, or Token
          death. Advancement is NOT reset. Special-item forced loss
          (artifact re-entering circulation) still happens.
Normal   — real death with DeathCost = the settings-sourced default.
```

Avoided conditions (source `Killed` early-return; all RESOLVED inputs):
arena-and-in-play-and-not-arena-real-death; prison-room class;
safe-player-attack room. World/game-mode facts (frenzy night, room
newbie-zone membership, honor-string newbie status, carried-token,
safe-player-attack, arena state) arrive as RESOLVED booleans. T5a MUST
NOT assign `world.VolumeFlags` bits (no ROOM_SAFE_DEATH, no
ROOM_NO_COMBAT, no arena/newbie bits) and no source room IDs (e.g.
1010–1018) appear in pure mechanics; §5.3.5 remains the only volume-flag
authority and later live integration resolves these facts.

Sequencing freeze: the special-item forced-loss notification is emitted
(1) in the avoided branch, and (2) after cheap-death determination but
BEFORE the token check — so a Token death (which sets cheap) still loses
the artifact. A Token-triggered cheap death additionally carries a
separate caller-resolved token unuse/ground-relocation side effect:
the token is unused, its stored rest-threshold adjustment is undone on
the player, and the token is moved to the death room at the player's
death position — while the generic cheap `DeathDropPlan` remains
empty. Ownership is frozen: T5a's `TokenDeath` bool remains the pure
classification signal (no item object lookup in T5a); T5c/runtime
resolves the actual token item, its runtime unuse state, and the
resulting restored `PlayerVitals.Threshold`, building the complete
character/item durable snapshots; T5b1b atomically persists those
already-resolved snapshots together with the rest of immediate death
entry. No Token proto/catalog ID is hard-coded anywhere, and no
token-class lookup belongs in Store. The kill
record/broadcast is written for every real death (cheap included), after
the special-item block.

#### 9.5.3 Double-death guard

Source rejects a second death while `now < lastDeathTime + 2` whole
seconds (strict `<`; exactly +2 s proceeds). T5a exposes this as a pure
decision helper over (lastDeathSeconds, nowSeconds) plus the frozen
2-second constant; T5c later owns runtime gating on the deterministic
tick clock. No wall-clock timer in T5a.

#### 9.5.4 Corpse policy (constants and metadata only in T5a)

Frozen source facts (player corpses):

```text
decompose lifetime            = 600000 ms
initial pickup/no-steal period= 25000 ms (only the corpse's own player)
death-time scalar             = whole seconds (Portal-of-Life age input)
resurrected flag              = at most ONE Portal of Life per corpse
corpse identity/owner         = owning player + death position
```

Cheap real deaths create the SAME corpse (creation is unconditional for
real deaths). Corpse expiration does NOT cancel the pending Underworld
death state (a corpse may expire while its player is still dead;
Portal-of-Life then can no longer target it — §9.5.10). T5a exposes
these as immutable constants/plan fields ONLY: no corpse entity, no DB
row, no timer (T5b1 owns the corpse row; T5c owns expiry).

#### 9.5.5 Drop plan (resolved per-item policy inputs)

Normal death iterates the two flat inventory families in source order
(active-then-passive) and drops an item iff the room can accept it, the
room movement check passes, AND the item's resolved policy says
drop-on-death. Voxilian freezes the input contract:

```go
type DeathItemInput struct {
    Key             // opaque deterministic identity for plan output
    DropOnDeath     bool // resolved item policy (base true; shields/
                         // rings/keys/crystals/item-attribute vetoes false)
    ForcedCheapLoss bool // special-item artifact loss notification
}
```

No catalog lookup, no PG, no live inventory walk, no item classes, no
invented item IDs in T5a. Containers: source has NO nested player
inventory (two flat lists); the plan is exactly the ordered inputs — no
recursive descent is invented. The output plan preserves input order and
records per dropped item: drop destination (to-death-position; source
places normal death drops on the GROUND at the death square unmerged —
the player corpse and the dropped items are separate world objects),
and PK-protection metadata
WHEN the killer was a player:

```text
NeedsPKProtection bool   // killer was a player (resolved input)
PKProtectionDurationMs = 600000 (source PKPOINTER_TIME = 10*60*1000)
```

The PK-protection POLICY (non-PK-enabled players cannot pick the item up;
the victim always can) is frozen here; its STORAGE is frozen in §9.5.8a:
the dedicated `item_pk_protections` child of the item aggregate (NOT
`enchants` JSON). The generic cheap-death drop plan is empty but still
produces the special-item forced-loss flags; a Token death additionally
has the separate caller-resolved token unuse/ground-relocation side
effect frozen in §9.5.2 (never a generic drop-plan entry).

In Voxilian storage every normal-death drop is a ground placement at
the death position:

```text
item_locations.kind = 1
pos_x/pos_y/pos_z = death position
character_id = NULL, corpse_id = NULL, container_item_id = NULL,
vault_region = NULL, slot = NULL
```

Drops are NOT `kind = 2` corpse-contained placements using the
generated corpse ID. The generic `item_locations kind = 2` schema stays
valid for other corpse-contained item use cases; it is simply not the
normal player-death drop location. The Token special relocation ends at
the same ground death position and never gains PK protection merely
because the killer was a player: Token death is cheap and the generic
normal drop/PK path is skipped.

#### 9.5.6 Immediate advancement plan (normal death only)

Frozen source result: advancement points → 0; gain chance → integer half
(truncation toward zero — the value is usually negative); gain flags
reset (did-damage/took-damage/dodged + kill target); atrophy marks reset
(spell entries marked unused; the atrophy feature itself stays disabled).
All four are durable character-advancement state in Voxilian (written by
T5b1); T5a returns them as an immutable plan value. Cheap/avoided deaths
change none of these.

#### 9.5.7 Immediate post-death vitals

Frozen branch order (real deaths only):

```text
frenzy death:   HP = MaxHP/2, Mana = MaxMana/2, Vigor = 100
ordinary death: HP = 1, Mana = 1, Vigor = bound(Vigor/4, 0, 50)
                then vigor normalization bound 1..200 (0 becomes 1)
angel-mail override (cost > 0 AND still-newbie AND not murderer):
                Mana = MaxMana/2 + 2
```

Still-newbie is a resolved input (source `PFLAG_TUTORIAL` is TRUE = NO
LONGER a newbie; "still newbie" = flag false). T5a exposes a pure helper
returning the planned post-death `PlayerVitals` (reusing the §9.4 value
type; result MUST pass `PlayerVitals.Validate()`; integer truncation and
0→1 vigor floor exact). No live `Engine`/entity mutation, no timer
scheduling (T5c recreates regen timers per §9.4b).

#### 9.5.8 Durable pending-death representation (schema audit result)

The CURRENT schema cannot safely represent the state between immediate
death and Underworld exit: `corpses` carries no death-cost/death-time/
portal state, `characters.vitals/advancement/flags` have no
pending-death phase semantics, and hiding lifecycle state inside
`advancement` JSON or `flags` bits has no established extensibility
contract (§9 creation freezes `advancement = {}`, `flags = 0`). Between
the phases the server must durably recover at least: the player is
pending death penalties, the effective current DeathCost (a reduced
Portal-of-Life cost MUST survive crash/reconnect), the death time (whole
seconds) needed by Portal of Life, the corpse association, and the
Underworld/dead lifecycle phase. A process crash/reconnect MUST NOT:
erase a reduced Portal-of-Life DeathCost, apply penalties twice, skip
penalties, create a second corpse/drop, or return the player alive for
free.

DECISION: T5b1 adds ONE narrow goose migration for a durable
pending-death representation (exact shape is T5b1's after schema audit —
e.g. a `pending_deaths` table keyed by character carrying phase,
effective death cost, death-time scalar, corpse id, and a
consumed-once/cleared marker with the character aggregate's CAS rules).
T5a implements NOTHING durable; a pure, persistence-agnostic
pending-death plan value (phase, effective cost, death-time scalar,
corpse-policy result) MAY be defined for T5b1 to persist.

#### 9.5.8a T5b1a frozen death-persistence schema (migration 0006)

This section freezes the T5b1 schema audit result. T5b1 is split into
T5b1a (this schema + low-level SQL primitives, no transaction) and
T5b1b (the atomic `CommitDeathEntry` Store transaction composing them).
T5b1a implements no Store transaction, no sim change, no Portal
mutation, no penalties, no expiry worker.

Migration `0006_death_persistence.sql` is the ONE new goose migration;
its Down reverses exactly what its Up adds. No second migration, no
`corpses` alteration (that table stays as-is: corpse remains NOT a CAS
root, and no DeathCost/Portal/death-time mechanics are duplicated into
it).

`pending_deaths` — the durable between-phase state. Columns:

```text
character_id         BIGINT PRIMARY KEY REFERENCES characters (id)
effective_cost       SMALLINT NOT NULL CHECK (effective_cost BETWEEN 0 AND 100)
death_time_seconds   BIGINT NOT NULL CHECK (death_time_seconds >= 0)
corpse_id            BIGINT NULL REFERENCES corpses (id) ON DELETE SET NULL
portal_used          BOOLEAN NOT NULL DEFAULT FALSE
created_at           TIMESTAMPTZ NOT NULL DEFAULT now()  -- operational only
```

Semantics, all binding:

- Character cardinality: at most ONE active pending-death row per
  character. The PRIMARY KEY is `character_id` itself — the table IS
  current state, never death history. Kills/ledger remain the
  audit/history. A second death-entry insert for a character with a
  live row fails on this PK; T5b1b maps that violation to the frozen
  machine-readable sentinel `ErrDeathAlreadyPending` (a caller with a
  freshly reloaded character revision is still rejected — character
  CAS alone is not sufficient).
- `effective_cost` is the Portal-reduced current cost, persisted
  explicitly with a `0..100` DB CHECK. No `NULL = 0` magic: cheap
  deaths store literal `0`.
- `death_time_seconds` is the source-semantic whole-second scalar
  (`>= 0`), persisted explicitly. `created_at` is operational only
  and MUST NOT be used to reconstruct the mechanics scalar.
- Phase encoding: the row's very existence means
  `DeathPhasePending`; there is deliberately NO phase column and no
  speculative future state-machine value. Deleting the row (T5b2,
  inside its character-CAS transaction) IS the transition to
  `DeathPhaseNone`.
- Corpse association: `corpse_id` is nullable with
  `ON DELETE SET NULL`. Pending death MUST survive natural corpse
  expiration/deletion: after `DeleteCorpse`, the pending row STILL
  EXISTS with `corpse_id` NULL and cost/time/portal state preserved
  (the corpse FK `SET NULL` is a referential side effect, not a
  gameplay mutation — it advances no revision). The association
  therefore MUST NOT cascade-delete the pending row and MUST NOT
  block `DeleteCorpse`. One live corpse backs at most one pending
  row: a partial `UNIQUE (corpse_id) WHERE corpse_id IS NOT NULL`
  pins this.
- Portal once-per-corpse state: `portal_used` is the durable source
  `pbResurrected` equivalent. It survives restart; T5b2 later
  implements the atomic "Portal once + lowers-only DeathCost"
  operation against it. Default `FALSE`; round-trips.
- CAS ownership: pending-death rows are a CHILD of the character
  aggregate, NOT an independent revisioned root (no `revision`
  column). Every gameplay write to this table (insert at death
  entry, cost lowering, row deletion at penalties) happens ONLY
  inside a transaction whose FIRST mutation is a successful
  character-root revision CAS (`UPDATE characters ... WHERE
  revision = $expected`), per the §8.1 aggregate rule — exactly
  like `character_spells`/`character_skills`. Future T5b1b/T5b2
  critical transactions therefore can never combine stale character
  state with pending-death writes: a stale character revision
  aborts the whole transaction, including the pending-death
  mutation. The sole exception is the FK-driven `SET NULL` above,
  which carries no gameplay semantics.

`item_pk_protections` — the durable PK-protection pointer (source
`IA_PKPOINTER`, 600000 ms, victim-directed). A dedicated relational
child table is chosen OVER `item_instances.enchants` JSON: `enchants`
is opaque per-item content with no established extensibility contract
for temporary gameplay attributes, while protection needs FK-backed
victim identity, exactly-one-row cardinality, and deterministic
replace semantics. Columns:

```text
item_id              BIGINT PRIMARY KEY REFERENCES item_instances (id)
victim_character_id  BIGINT NOT NULL REFERENCES characters (id)
expires_at           TIMESTAMPTZ NOT NULL
```

Semantics, all binding:

- One current protection row per item (PRIMARY KEY is `item_id`);
  the victim FK and the absolute expiry are always present. The
  killer is NOT stored (no later policy consumes it).
- Item-aggregate ownership: the protection is mutable child state of
  the ITEM aggregate. Every write/replacement happens ONLY in the
  SAME transaction as that item root's successful item-root CAS
  (`UPDATE item_instances ... WHERE revision = $expected`), per
  §8.1. NO public Store method may add protection to an existing
  item without advancing/checking the item root revision; T5b1b
  composes the low-level primitive inside the death transaction.
- Re-drop semantics: re-protection of an already-protected item is a
  deterministic UPSERT replacing victim + expiry with the new
  death's values (no duplicate rows possible by construction).
- Expiry: T5b1a implements no enforcement and no worker. Later
  pickup enforcement owns interpreting `expires_at` (absolute time;
  T5b1b computes it as death time + 600000 ms).

Generated corpse-ID composition contract (binding on T5b1b): the
corpse ID does not exist before the death-entry transaction. The
future Store API MUST NOT require the caller/sim to fabricate or
preallocate a corpse ID. The transaction conceptually begins,
validates/CASes durable roots, inserts the corpse, obtains the
generated corpse ID, and uses that ID for the pending-death
`pending_deaths.corpse_id` association (plus any genuinely
corpse-owned future behavior) — committing once. The generated corpse
ID is NOT used for ordinary player death-drop item locations: those
are ground placements at the death position per §9.5.5
(`item_locations.kind = 1`, all of `character_id`/`corpse_id`/
`container_item_id`/`vault_region`/`slot` NULL). The player corpse row
and the dropped-item ground rows are separate world objects sharing
only the death position; no player death item is placed in the
generated corpse merely to make the DB transaction easier.

T5b1b death-item mutation contract (binding): the death transaction
carries zero or more caller-resolved item aggregate mutations
representing normal-death ground drops and/or the Token-death special
ground relocation. Every such death-entry relocation MUST end at
exactly the transaction's death position (`Kind = ground`,
`Pos = DeathPos`, all non-ground references NULL); T5b1b rejects
anything else, in particular `Kind = 2` / `CorpseID != nil`.
Ordinary normal drops may additionally receive PK protection; the
Token special relocation never does via the player-killer path. The
frozen transaction model is therefore:

```text
character post-death durable snapshot
item death-relocations -> ground at death position
corpse row -> separate row at death position
pending_deaths.corpse_id -> generated corpse ID
optional kills row
```

Multi-item CAS / deadlock contract (binding on T5b1b): each dropped
existing item is its own CAS root. The death transaction locks/CASes
roots in deterministic order — character root FIRST, then item roots
in ASCENDING `item_instances.id`. The T5a drop-plan order stays the
GAMEPLAY/audit order and is not re-sorted for locking. Any stale root
(character or item) rolls back EVERYTHING: character update, corpse
creation, every earlier item update, pending-death insertion, PK
metadata, kills, ledger. Nothing partially survives.

Kill-audit decision (binding): `InsertKill` is mandatory ONLY when
the resolved killer fits the existing auditable character/mob
identity domain (`killer_kind` 0/1). Environmental/system/item killers
outside that domain MUST NOT be represented by a fabricated
mob/character ID — no `kills` row is written for them. `kills` is not
broadened for convenience.

Ledger decision (binding): T5b1b invents NO death/drop ledger kind.
No death/drop ledger-kind numeric meaning is frozen anywhere in §8,
so T5b1b inserts only the already-normative audit rows (the `kills`
row per the paragraph above) and no ledger rows. Recovery MUST come
from materialized state (`characters`, `pending_deaths`, `corpses`,
`item_instances`/`item_locations`, `item_pk_protections`) — never by
replaying ledger rows (§8.1/D7).

T5b1a SQL primitives (low-level `internal/store` building blocks for
T5b1b to compose transactionally — NOT independent public Store
mutations, which could create pending death without the
character/corpse transaction or PK protection without item CAS):

```text
InsertPendingDeath(character_id, effective_cost, death_time_seconds,
                   corpse_id, portal_used)   -- PK violation => replay
GetPendingDeathByCharacter(character_id)     -- recovery/read path
DeletePendingDeathByCharacter(character_id)  -- T5b2 frozen ClearPending
UpsertItemPKProtection(item_id, victim_character_id, expires_at)
GetItemPKProtection(item_id)
DeleteItemPKProtection(item_id)              -- expiry/pickup cleanup
```

No pending-cost update primitive is added here: the lowers-only
Portal mutation belongs to T5b2, which will freeze its own update
semantics. No `CommitDeathEntry`, no character/item death CAS
composition, no kill/ledger composition in T5b1a.

#### 9.5.9 DeathCost domain

The default cost comes from server settings (source default 100,
per-server overrides 90/60, documented domain 1..100). The pure planner
RECEIVES the default cost as an explicit input and validates it in
1..100; it must not silently assume 100. Zero means cheap (no penalties,
no drops). A Portal-reduced cost is 5..80 (§9.5.10); runtime pending
costs live in 0..100.

#### 9.5.10 Portal of Life (pure calculation in T5a)

Frozen source formula over (pendingCost, corpseAgeSeconds, spellPower):

```text
timeAdj = age < 60 ? -(60 - age)      // age 0 → -60 … age 59 → -1 (bonus)
                  : age/10 - 6        // age 60 → 0 … age 600 → +54 (penalty)
newCost = pendingCost - (spellPower - timeAdj)
newCost = bound(newCost, 5, 80)       // integer arithmetic, truncating /
```

Whole-second ages; the 60-second boundary is strict (`<`); `/10` integer
truncation; spell power domain 1..99 (§9.3a). Once per corpse: a corpse
that already received a Portal cannot receive another (source
resurrected flag; T5b1 persists it with the corpse/pending-death state).
`SetDeathCost` semantics: the pending cost only LOWERS (a later, worse
portal result cannot raise it; override is admin-only, out of scope).
Voxilian validation policy (frozen): negative age is a domain error;
ages beyond the corpse lifetime are accepted by the formula but the
runtime (T5c) may not offer expired corpses; invalid power (< 1, > 99)
is a domain error BEFORE any RNG or mutation. T5a implements ONLY the
pure calculation — no spell/corpse lookup, no gateway, no timer, no DB.

#### 9.5.11 Delayed DeathCost normalization (T5a pure, T5b2 durable)

Frozen branch order at Underworld exit:

```text
1. frenzy active at exit → clear haunted flag, NO penalties at all
2. cost >= default (no portal mitigation) → clear outlaw flag,
   re-evaluate PK status, clear haunted flag
3. cost > 0:
     still-newbie AND not murderer → cost = cost / 3 (truncation; no HP roll)
     ELSE (experienced OR murderer)  → HP-loss roll (§9.5.12)
4. guild-quit check (§9.5.14) — runs on every non-frenzy exit, even cost 0
5. ability losses (§9.5.13)
6. pending cost cleared to 0
```

Newbie status here is the resolved still-newbie boolean (§9.5.7) — NOT
inferred from HP. Murderer is a resolved boolean. The murderer loss
severity decision is read AFTER step 2's flag clears (source order; the
PK re-evaluation is async and does not feed this check).

#### 9.5.12 Delayed BaseMaxHP penalty (T5a composes T4a)

The HP-loss roll happens only in the experienced-or-murderer branch with
cost > 0, with boundary `roll <= cost` (roll EXACTLY cost loses — note
this differs from the ability cost roll §9.5.13). On loss:
`AdjustBaseMaxHP(vitals, -1, stamina)` — base floor 20 — and the SAME
actual delta flows into `AdjustMaxHP` (source `GainBaseMaxHealth`
followed by `GainMaxHealth` with the reported delta); current HP is NOT
changed. T5a MUST compose the existing §9.4.4/§9.4.5 production helpers
and MUST NOT rederive their bounds. Minimum behavior pinned: base 20
loss → delta 0, nothing changes.

#### 9.5.13 Ability penalties and RNG consumption order (frozen)

For every spell then every skill (caller-supplied canonical order):

```text
eligible iff current ability > 5        (5 never eligible; 99 eligible)
stamina save: roll > Stamina → save FAILED (roll == Stamina saves;
              effective Stamina 1..70, resolved input)
death-cost roll: roll < cost → ability lost (roll == cost saves)
loss: -2 if murderer else -1
result ability = bound(ability + loss, 1, 99)   (6 − 2 → 4; clamp is 1)
```

RNG consumption order (deterministic replay contract): HP roll first
(when §9.5.11 routes to it), then spells in order, then skills in order.
Within one ability: the stamina roll is consumed for every ELIGIBLE
ability; the cost roll is consumed ONLY when the stamina save fails
(short-circuit AND). An ineligible ability consumes NO rolls. A failed
stamina save consumes the cost roll even when cost is 0 (the comparison
still evaluates). Uses the existing injected `RNG`/`RollD100` seam; no
`math/rand`. Output states exactly which abilities lose how much
(ordered plan); no live ability-collection mutation. Validation of all
inputs precedes the first roll.

#### 9.5.14 Justice/guild/PK side-effect hook flags (classification)

T5a outputs pure hook decisions; each is classified for later ownership:

```text
clearOutlaw        — T5b2 durable core state (cost >= default branch)
clearHaunted       — T5b2 durable core state (both frenzy and full-cost)
reEvaluatePKStatus — future phase-2 justice integration hook (flag now)
quitGuild          — base BaseMaxHP < PKILL_ENABLE_HP (30) after penalties;
                     future phase-2 guild integration hook (flag now);
                     NOT live guild state on the player entity
guardianAngelMail  — presentation/message only (mail system is post-MVP);
                     the mana side-effect rides §9.5.7 vitals instead
soldierShieldDeathEffect — future item-hook integration (plan value now,
                     NOT a bare boolean; see frozen rule below)
karmaBoobyPrize    — deferred content hook (M9 protos); plan flag now
specialItemLoss    — resolved per-item forced-loss flag (§9.5.5)
```

No absent guild/faction/justice system is implemented in T5a; no fake
guild state exists on the player entity. No shield object is mutated, no
faction state is implemented, no lookup is performed: T5a exposes only
the immutable source-faithful plan below.

Frozen SoldierShield death-outcome rule (source `player.kod` `Killed`
normal-death branch invoking the currently-used `soldshld.kod`
`OwnerDied(what=killer)` with the source default `logoff = FALSE`):

```text
if no SoldierShield:                              no effect
if death is not Normal:                           no OwnerDied death effect
if Normal death but NOT IsEnemyAttack(killer):    no SoldierShield death effect
if Normal death AND shield present AND IsEnemyAttack(killer):
    rank 1..3:   shield is deleted
    rank 4..10:  shield survives with ModifyFactionRank(-4),
                 rank bounded to 1..10
```

Exact surviving rank vectors (frozen):

```text
4  -> 1
5  -> 1
6  -> 2
7  -> 3
8  -> 4
9  -> 5
10 -> 6
```

The T5a immediate-`Killed` planner's resolved enemy input MUST mean only
the immediate death's `SoldierShield.IsEnemyAttack(killer)`. The
`logoff = TRUE` branch is a separate logoff-ghost penalty path
(`logghost.kod` system) and MUST NOT be folded into the T5a immediate
`Killed` plan; the future unsafe-logoff owner may compose the same pure
calculation with `logoff = true`. T5a takes no logoff boolean.

Frozen Normal+Frenzy contradiction guard: `plan.Disposition ==
DeathNormal && FrenzyActive -> ErrInvalidDeathInput` unconditionally
(zero hook output), regardless of newbie/murderer/angel eligibility.
Frenzy routing is a cheap real death before immediate hooks are planned.
`DeathCheap + FrenzyActive` remains accepted; an avoided death's
early-return takes precedence so it is not over-constrained.

#### 9.5.15 Underworld target is resolved, not hard-coded

No RID_UNDERWORLD, no Meridian row/column coordinates, no invented
Voxilian coordinates in T5 tasks. Newbie-range deaths respawn at the
resolved newbie-home placement; other deaths at the resolved
Underworld-placement seam; both are T5c live-integration inputs (M10-T2b
authors the real classic Underworld source). T5a performs NO world
lookup.

#### 9.5.16 Wire ownership (unchanged)

The frozen codecs stand: C→S `120 respawn_ack {}`; S→C `214 death {victim
u32}`; S→C `215 respawn {pos}` (§6.3). No second death wire protocol.
T5a touches no proto/gateway code. T5c owns routing/state-machine
integration incl. opcode 120 handling (rate-gated then delegated per
§7.3.2 until then).

#### 9.5.17 T5a pure surface and non-scope (binding)

T5a delivers small pure functions + plain comparable structs in
`internal/sim` covering: disposition decision (§9.5.2), double-death
guard (§9.5.3), corpse constants (§9.5.4), ordered drop plan + PK
metadata (§9.5.5), advancement plan (§9.5.6), post-death vitals
(§9.5.7), persistence-agnostic pending-death plan value (§9.5.8),
DeathCost validation (§9.5.9), Portal-of-Life cost (§9.5.10), delayed
cost normalization (§9.5.11), HP penalty via T4a composition (§9.5.12),
ability penalty plan + RNG order (§9.5.13), hook flags (§9.5.14).
NON-SCOPE: live entity mutation, Store/PG imports, corpse DB rows, item
mutation, gateway/proto changes, Underworld teleport, respawn, opcode
120, timers, `internal/sim` importing `internal/store`/pgx/sqlc, any
T5b1/T5b2/T5c/T6/T7 work. Stale-revision/commit-ambiguity behavior for
T5b1/T5b2 follows §8.1/§8.3 exactly; no PG call occurs on the sim owner
goroutine.

#### 9.5.18 Test minimums (T5a)

Avoided/cheap/normal matrix with corpse/drop/penalty/Underworld-plan
differences and each cheap condition pinned separately (frenzy, newbie
room, honor, token + token-still-loses-artifact ordering); corpse
constants; post-death vitals boundary vectors (low/non-divisible/high
vigor, cap 50, 0→1 floor, frenzy branch, angel-mana override) all
`Validate()`-clean; advancement exact results; ordered drop plan
(droppable/undroppable/mixed/cheap/PK vs non-PK); Portal-of-Life golden
vectors (ages 0/59/60/61/later/lifetime-edge, power min/max, bounds 5
and 80, truncation, invalid inputs); cost scaling (ordinary/newbie/3/
murderer/zero); HP roll boundary (roll == cost) independent from ability
boundaries (stamina ==, cost ==); eligibility 5/6/99; murderer clamp
6−2→4; spell-vs-skill separation; scripted-RNG full-consumption-order
proof (HP → spells → skills; ineligible and saved rolls consume exactly
as frozen); hook flags; bounded property loops over DeathCost/vigor/
BaseMaxHP/Stamina 1..70/ability 1..99/power 1..99/corpse ages with
value invariants and no panics on hostile integers (overflow-safe
arithmetic).

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

- v0.3.36: freeze M5 death-entry item relocation (docs only; T5a stays
  `[x]`, no schema/query change): normal player-death drops are GROUND
  placements at the death position (`item_locations.kind = 1`, death
  pos, all non-ground references NULL) — NOT `kind = 2` corpse-contained
  placements; the generated corpse ID serves `pending_deaths.corpse_id`
  (plus genuinely corpse-owned future behavior), never ordinary drop
  locations; the generic cheap `DeathDropPlan` stays empty while a Token
  death additionally carries a separate caller-resolved token
  unuse/ground-relocation side effect (unuse, rest-threshold restore,
  move to death room at death position; no PK protection via the
  player-killer path); ownership split frozen as T5a pure `TokenDeath`
  signal / T5c resolution / T5b1b atomic persistence with zero or more
  caller-resolved ground relocations. No Token proto/catalog ID is
  hard-coded; no token-class lookup belongs in Store.

- v0.3.35: documentation consistency correction only (no schema or
  implemented-behavior change): §9.5.1 T5b1b ownership now states the
  §9.5.8a decision verbatim — the atomic death-entry transaction covers
  the existing kills audit row only when the resolved killer fits the
  existing character/mob identity domain, and writes NO death/drop
  ledger row (no invented ledger kind). The older "kill/ledger audit"
  phrasing contradicted §9.5.8a and is removed.

- v0.3.33: freeze M5 soldier-shield death outcome (normative §9.5.14):
  source `Player.Killed` invokes the currently-used `SoldierShield`
  `OwnerDied(what=killer)` in the NORMAL-death branch with the source
  default `logoff = FALSE`; `soldierShieldDied` alone is insufficient so
  T5a exposes the complete immutable `soldierShieldDeathEffect` (no
  effect / delete shield / survive with exact post-death faction rank);
  rank 1..3 deleted, rank 4..10 survive via `ModifyFactionRank(-4)`
  bounded 1..10 with exact vectors 4->1, 5->1, 6->2, 7->3, 8->4, 9->5,
  10->6, gated on Normal + shield present + `IsEnemyAttack(killer)`; the
  `logoff = TRUE` branch belongs to the separate logoff-ghost system and
  is NOT merged into the T5a immediate `Killed` planner; unconditional
  `DeathNormal + FrenzyActive -> ErrInvalidDeathInput` guard frozen
  (cheap + frenzy stays accepted, avoided keeps early-return precedence).
  Reconciled `meridian59.md` §9.5 accordingly. No implementation change.

- v0.3.32: freeze M5 death semantics and split M5-T5 into T5a/T5b1/T5b2/T5c
  (normative §9.5): source-audited two-phase lifecycle (immediate
  `Killed`-equivalent death entry as ONE atomic critical transaction;
  delayed Underworld-exit `ApplyDeathPenalties`-equivalent as a SEPARATE
  one; no open transaction between); avoided-vs-cheap-vs-normal
  disposition with resolved world/game-mode inputs and no volume-flag or
  source-room-ID leakage into pure mechanics; 2-second double-death guard;
  corpse constants (600000 ms decompose, 25000 ms no-steal, whole-second
  death time, once-per-corpse Portal flag); ordered drop plan from
  resolved per-item policy with PK-protection policy (10 min) separated
  from its future storage; immediate advancement plan (points 0, gain
  chance truncated-half, gain/atrophy resets); immediate post-death
  vitals (frenzy half/max vs 1/1/vigor÷4-cap-50 with 0→1 floor and
  still-newbie angel-mana override); durable pending-death DECISION —
  current schema insufficient, T5b1 will add ONE narrow migration;
  settings-sourced DeathCost 1..100; exact Portal-of-Life formula
  (60-second strict boundary, /10 truncation, bound 5..80,
  lowers-only); delayed penalty branch order with exact d100 boundaries
  (HP roll `<=` cost vs ability rolls stamina `>` / cost `<`), eligibility
  > 5, clamp 1..99, murderer −2, still-newbie /3 with the inverted
  source PFLAG_TUTORIAL reading; RNG consumption order; justice/guild
  hook-flag classification; resolved Underworld placement seam;
  unchanged 120/214/215 wire ownership; T5a pure surface + non-scope +
  test minimums; `internal/sim` must not import store/pgx/sqlc and no PG
  on the sim owner goroutine; T5b1 `CommitDeathEntry` / T5b2
  `CommitDeathPenalties` conceptual boundaries under §8.1/§8.3 rules.
  Reconciled `meridian59.md` §9.5 against source (avoided-vs-cheap
  distinction, PFLAG_TUTORIAL inversion, exact roll boundaries, angel
  mail/mana, newbie-range respawn target, PK pointer/cheap-death
  sequencing, corpse metadata) + verified corrections recorded.

- v0.3.31: narrow M5-T4b2 composition freeze closing the three
  implementation-architecture gaps exposed by the completed T4b1
  integration (mechanics unchanged; no new source research): (a)
  runtime-input OWNERSHIP — §9.4b.14a freezes one plain-value
  `PlayerVitalsRuntimeInputs` snapshot (effective Stamina/Mysticism
  1..70, Restorate/Rejuvenate/ManaFocus/Invigorate powers 0-or-1..99,
  rest multiplier exactly 1..3, faction still phase-2 zero) stored on
  the player entity as the authoritative current resolved value with
  one validating owner-local update API, the source timing rule that
  input changes never restart a running deadline and affect only the
  next create/re-arm plus the fire-time rest multiplier, and no stale
  secondary cache; (b) STEP PHASE ORDERING — §9.4b.17 pins the final
  per-entity order (ownership verify -> movement consume -> integrate/
  handoff -> movement output -> health/mana/rest in fixed slot order ->
  history sample) inside the ONE tick-start worklist with no second
  global pass, including acted-marks-on-consume-before-timers and the
  documented Vigor-9 same-tick walk; (c) POST-COMMIT LIVE-STATE
  VALIDITY — §9.4b.22 freezes the one narrow commit guard (live commit
  requires after.Validate(); failure leaves bit-identical state, no
  event, caller error) while §9.4 T4a pures stay unbounded/
  source-faithful, pinning MaxMana 20-19 -> 1 allowed and 20-20
  rejected. Also §9.4b.3a freezes atomic T4b2-complete player
  initialization (vitals + runtime inputs validate before an EntityID
  is consumed; §9.4b.9 initial state installed atomically; no invented
  default stats) and §9.4b.10 pins the explicit armed/present bit with
  due==0 as a valid wrapped deadline.

- v0.3.30: freeze M5-T4b runtime semantics and split the broad row into
  T4b1 entity integration + T4b2 deterministic scheduling (normative
  §9.4b: single-writer deadline-slot model with one canonical
  CastTicks-equivalent ceil ms->tick conversion and serial32 due
  comparison bounded < 2^31, currentSimTick-based create/re-arm,
  at-most-one event per slot per Step with clear-first ordering,
  NewHealth/NewMana create/cancel/KEEP-existing-deadline semantics with
  HP>0 create guard and idle-player re-arm-without-healing, ungated mana,
  rest lifecycle with always-re-arm and below-threshold-only recovery,
  resolved-inputs-only regen supply with no live lookups and resolved
  1/2/3 room multiplier, tick-based stomach anchor with fractional-second
  preservation and documented no-offline-digestion MVP, actedSinceEntry
  first-action concept with movement/turn routing, cell-handoff
  preservation, resolved room-entry reset policy boundary with no ROOM_*
  bit assignment, deterministic canonical-order + fixed-slot-order tie
  break with documented quantization divergence, player Vigor>=10 run
  gate distinct from strict HasVigor, immutable dirty/event seam firing
  only on real change, owner-local mutation surface composing the T4a
  helpers, player/generic classification without EntityID overloading,
  and T4b1/T4b2 test minimums) + audit corrections recorded in
  `meridian59.md` (timer runtime facts, run gate, regen action gating).

- v0.3.29: freeze M5-T4a authoritative vitals state/mutation core
  (normative §9.4: canonical PlayerVitals value with 1/10000 exertion
  units, hp/base_max/max/mana/max_mana/vigor/threshold/stomach JSON
  names plus exertion with missing-means-zero, explicit validation
  domain with false invariants rejected, two-step base-max formula
  with actual delta, unclamping MaxHP modifier, loss/normal/over-max
  heal trio with the already-above-2xMax corner, pure timer one-steps,
  health/mana/rest interval formulas with Restorate/Rejuvenate/
  ManaFocus/Invigorate resolved seams, initial/node/double-node/compute
  mana arithmetic, strict HasVigor, strict >20000 exertion conversion
  with residual preservation vs rest clearing, 10..100 threshold with
  explicit errors, seconds-based lazy stomach decay with 1..100 post
  bound and <=100 eat-capacity seam, creation compatibility, T1/T2/T3
  composition boundary, stable errors, golden/property/fuzz minimums;
  T4b deferred: entity attachment, scheduling, moved-gating, handoff,
  tick integration, saver) + verified `meridian59.md` corrections
  (Fey-only double — AvarNode inherits standard, no Vale multiplier;
  stomach 1..100 post-update bound with seconds-based decay and
  +nutrition vigor points; HP over-max decay reuses the normal health
  interval, BOOST_DECAY is mana-only).

- v0.3.28: freeze M5-T3b special spell semantics (normative §9.3b:
  touch proficiency max(Punch,(Myst*3)/2) with dead viHit_Factor
  excluded, real-T1 hit composition, touch damage half+(half*power)/99+1
  with truncation order, mixed weapon+spell signature, duration
  Random(power/3,power/2) bound 10..75 ×6×1000 ms, no per-hit costs,
  Holy Touch undead-2x and karma/200 modifier; Illusionary Wounds
  player/monster /100 bases over buffed MaxHP with MaxHP/3 then HP-1
  caps, PolicyAbsolute, 20 s..80 s duration, applied-amount refund,
  factor-1-only reachability; Vampiric Drain post-application
  bound(dmg/2,1) and lethal bound(max/2,1) with explicit killed flag;
  Earthquake severity 1+power/25, squared-distance 64/400 falloff with
  100*(400-sq)/336 interpolation, caster/environmental/item-self
  (9*severity) modes, ordinary ALL+QUAKE policy; walls fire power/6
  (1..16) / lightning power/4 (1..25), illusionary direct-power with
  <35 threshold, base lifetimes, ±20 s jittered final lifetime
  30 s..200 s, 1350..1650 ms periodic jitter, Random(0,max) ordinary
  rolls, 5-active/4-passive distinction, once-per-period contract,
  illusionary IW reuse with non-lethal proof; new narrow sentinels;
  golden/property minimums) + verified `meridian59.md` corrections
  (wall maxDamage/duration per family, IW difficulty bound and
  buffed-Max cap precision).

- v0.3.27: split M5-T3 into generic core T3a + special archetypes T3b
  (new §9.3/§9.3a normative: spell-power 1..99 domain, generic
  SuccessChance order Hinder → 5..95 bound → no-LOS adjustment →
  d100<=chance → ReagentRing ForceSuccess rescue with full payment,
  exact strict no-LOS distance rule with equality-unchanged and no
  second clamp, mana-cost arithmetic with strict >40/>80 tiers and
  ceil equipment reduction floored at 1, karma ±10×level with
  equality-passing eligibility, BaseMaxHP-not-current-HP minimum
  gate, strict HasVigor exertion gate with full/half charges,
  reagent preflight substitute-consumed-before-roll contract,
  postcast check+arm ordering invariant, payment-before-trance with
  full/half plans and target-resisted cost rule, trance duration
  (150−power)% with ceil ms→tick conversion and no-refund/no-repay
  contracts, AttackSpell /99 scaling with truncation order and Mana
  Focus +1, player/item/monster origin matrix, absolute policy
  value, T1/T2/T3 pipeline boundary; §9.3b T3b boundary families;
  §9.3c new M5-T7 authoritative 103/104 runtime after T1..T6 with
  M5 exit after T7; T3a/T3b own pure mechanics only, no live
  vitals/gateway) + verified `meridian59.md` correction
  (piMinHitPoints gates BaseMaxHP via Player GetLevel, not
  current HP).

- v0.3.26: freeze M5-T2 defense mitigation semantics (new §9.2:
  T1/T2/T3/T4/T5 ownership, T1→armor→resistance→caps pipeline with no
  T2 minimum, T2-resolved PlayerDefense components without duplicating
  the formula, exact Parry/Block/Dodge capability gates, shared
  Agility-requisite skill-chance formula with unclamped chance and
  d100<=chance success, Parry/Dodge never rolled in combat with only
  Block gating shield reduction, DefensePower sums with shield
  exclusion, generic DefenseModifier value, exact random(r/3,r) +
  damage−1 cap + post-roll 2/3 damage-class rule, sequential
  order-independent multi-modifier application, block rating
  1..120 with piDefense_bonus placement, source-bit resistance
  tags/matching/aggregation/transform, durability/spellmod/catalog
  exclusions, golden vectors, property invariants) + verified
  `meridian59.md` corrections (capability-gate mapping, Parry/Dodge
  never rolled, piBlockBonus dead, disciple robe +5/0, per-side
  resistance clip, unified resistance transform).

- v0.3.24: correct M4-T5b2 fanout closure — targeted outbound state
  cancellation before 206 (§7.1.13, exact 206 ordering in §7.4.5);
  exact post-admission fanout-control completion with a race-free
  Close/admission boundary and non-blocking OnMovement (§7.4.3);
  emergency fanout ready/throttle invalidation with one short
  metadata mutex and exact post-sim-remove fallback (§7.4.6); and
  TransportLiveness.Close ownership of all Ping loops (§7.4.7).

- v0.3.23: freeze M4-T5b2 AOI fanout and transport liveness —
  gateway Presence gains an entity->viewer reverse index; one bounded
  1024-event fanout pump consumes sim MovementUpdate without blocking the
  sim owner; post-world-ready bootstrap emits deterministic 204 creates
  before a session becomes fanout-ready; live cell churn reconciles
  visibility with 204/206 and recipient-local non-reused NetEntityIDs;
  205 is recipient-local state-lane traffic capped at <=10 Hz with the
  processed-input anchor exposed only to the owning session. Raw WS and
  heartbeat teardown now use the same flush-first WorldExit composition;
  failed disconnect cleanup retains the stale session for retry rather
  than orphaning world state. Ping/Pong runs at 15 s and stale Presence
  sweeps at 30 s.

- v0.3.22: freeze real gateway-to-sim ingress and staged world-presence
  lifecycle — one bounded 256-command Engine owner mailbox serializes
  gateway Add/Remove/Move without a broad sim mutex; 102 is decoded,
  rate-limited and routed through the active Presence EntityID with exact
  retry/protocol mappings; enter stages the sim entity before 217 and
  activates Presence only after the physical 219 + CompleteEnterWorld
  barrier; existing WorldExit remains the mandatory flush-first barrier
  for leave/takeover. Split remaining T5b into T5b1 ingress/lifecycle and
  T5b2 AOI fanout/heartbeat.

- v0.3.21: freeze M4 ephemeral presence/AOI core — gateway-owned
  active-presence epochs, exact 3-cell/49-cell base subscriptions,
  reverse subscriber index, session-local monotonic non-reused
  NetEntityIDs, visibility lifecycle/stale-handle protection,
  per-character active-presence token buckets, and monotonic heartbeat
  state; split M4-T5 into core T5a and runtime integration T5b.

- v0.3.20: freeze saver persistence composition — Store-independent
  T4a jobs bind through internal/persist to existing character/item/bank CAS
  operations; Store stale errors become saver stale while retaining the Store
  cause, saver stale forces T3c materialized-PG reload, saver lag is the exact
  low-cardinality snapshot-age histogram, and real PG crash/ambiguous-commit/
  shutdown proofs close T4b.

- v0.3.19: freeze snapshot-saver semantics — tracked persisted revisions,
  latest-wins immutable full snapshots, per-aggregate serialized CAS writes,
  periodic 60 s scheduling, critical write-through ordering, stale-CAS
  reconciliation blocking, and context-bounded shutdown flush; split M4-T4
  into saver core T4a and PG/observability/crash proof T4b.

- v0.3.18: freeze post-commit reconciliation — successful durable
  transactions fence every affected in-memory aggregate before notification
  delivery; sequential commit notices advance persisted revisions, while
  dropped/failed/gapped notices require a staged full materialized-PG reload
  before the next mutation. Ledger replay remains forbidden.

- v0.3.17: freeze cross-cell operation infrastructure — opaque
  Snowflake-style OpIDs, explicit owner routing, same-ID retry semantics,
  receiver apply-once ordering, bounded recent-op cache, handoff-preserved
  dedupe state, and synthetic aggregate proof; no real gameplay or PG.

- v0.3.16: freeze M4 cell handoff semantics — {cell,generation}
  ownership epochs, generation-1 initial ownership, canonical tick-start
  worklists guaranteeing at-most-once entity processing per tick, real
  collision-aware cross-cell movement transfer, explicit resident/migrating
  route states, preserved entity/history/control state, bounded migration
  MoveIntent routing, and queue-full-only retry semantics.

- v0.3.15: freeze M4 authoritative movement semantics — held-direction bit
  assignments and Godot-friendly yaw convention, normalized horizontal
  walk/run integration, persisted/latest coalesced movement controls,
  RFC1982 input sequencing and sample-tick sanity via the neutral serial32
  package, minimal point-query collision with deterministic substeps,
  run-gate/volume seams, 205-compatible sim movement results, and explicit
  M4-T2 vs M4-T5 transport/AOI ownership.

- v0.3.14: freeze deterministic M4 sim skeleton — fixed-step tick semantics
  (default 20 Hz with configured tick rate), no catch-up bursts, canonical
  cell/entity iteration, floor-correct XZ cell mapping, opaque internal
  EntityIDs, single-writer T1 ownership, explicit cross-cell handoff staging,
  and a two-second post-tick position-history ring.
- v0.3.13: clarify final M3 ACK/observability semantics — ACK epoch is
  atomically coupled to IN_WORLD lifecycle, normal write sequence allocation
  enforces max-unacked before assigning a seq, direct terminal frames remain
  outside the window, and frozen outbound metric names are typed as counters
  plus event-sampled histograms with bounded label sets.
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
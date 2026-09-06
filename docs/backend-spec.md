# Voxilian Backend SPEC (v0.3.23 — documentation only, no implementation)

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

### 7.4 AOI fanout and transport liveness (frozen, v0.3.23)

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
config); before admission cancellation may abort, after admission
the event is authoritative with a cap-1 completion signal and no
waiter goroutine. One queue is the visibility barrier: controls
order after earlier movements and before later ones, so no 205
precedes its bootstrap 204 and no stale 205 follows a 206. Shutdown
is idempotent: movements discarded, queued control waiters receive
`ErrFanoutClosed`, future movement dropped, future control
rejected, no send-on-closed panic, no restart required.

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

204 uses TryCritical (visibility transitions are reliable); 206
uses TryCritical with the old handle, then retires (critical
failure still retires — the recipient is already failed closed and
reconnects/resyncs; never preserve a stale handle for a dying
socket). New-handle 204 admission failure retires the handle via
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
can exist after sim removal, so no stale 205 follows. If reliable
removal cannot complete after real sim removal, every currently
indexed viewer is closed/resynced with mappings retired, the source
marked not-ready, and local teardown continues — never resurrecting
the entity or leaving stale addressable handles. Takeover order is
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
(Presence + Reaper + NowFunc + ticker factory) owns the sweep and
ping loops with manually-pulsable timers (no scattered
time.NewTicker); Server takes it as optional/explicit composition
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
bridges to M10-T4); `outbound.go` needs no semantic change — if
one appears necessary, freeze the spec first.

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
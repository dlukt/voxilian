# Voxilian Backend — Implementation Plan (v1.18)

> Source of truth for WHAT: `docs/backend-spec.md` (v0.3.34).
> This file is the WHAT-ORDER + WHO-DOES-IT tracker.
> If implementation discovers the spec is wrong, change the SPEC first
> (separate commit), then implement — never silently diverge.

## Working directory rule (binding)

```text
Backend working directory: backend/voxilian.
Unless explicitly prefixed otherwise, all backend paths in this plan
are relative to backend/voxilian/.
All Go commands run from backend/voxilian/.
Repository root is the Godot client project; do not create a client/ subtree.
Shared protocol fixtures live at repo-root testdata/protocol/ (both sides read).
```

## How to work this plan (read before picking up a task)

1. Pick ONE unchecked task `[ ]` in the earliest incomplete milestone
   (content tasks in M9 may run in parallel once M1 is done).
2. Keep the diff small: **≤ ~8 files, ≤ ~1500 lines** including tests.
   If it doesn't fit, split the task and note it here first.
3. Implement against the cited spec sections. Spec ambiguity → stop and
   ask (or fix the spec in a separate commit), don't invent protocol.
4. Definition of done for EVERY task:
   - from `backend/voxilian/`: `go build ./...`, `go vet ./...`, `gofmt -l .` clean;
   - new behavior covered by tests (unit, or testcontainers PG 18 where
     the task says so); full suite green;
   - golden vectors/fixtures committed where the task requires them;
   - docs touched only if the task says so.
5. Mark the task `[x]` **only when merged to `main`**, same commit or
   immediately after. Never batch-mark.
6. A milestone is done when ALL its tasks are `[x]` AND its exit
   criteria hold — then mark the milestone checkbox too.

Context budget: assume ~80–150k usable tokens per implementer session.
Tasks are scoped so one task ≈ one session. Anything bigger was split.

---

## M0 — Repo scaffolding & CI

Exit: `compose up` gives PG 18 + builder (migrate init step wired, no-op stub until M1); CI builds, vets, tests, publishes GHCR image.

- [x] **M0-T1** Extend the existing Go/Cobra scaffold (do NOT recreate the module). Under `backend/voxilian/`, add `internal/{config,gateway,sim,store,session,auth,admin,observe,world}`, extend `cmd/` to `serve/migrate/admin/seed` (stubs printing not-implemented), add `migrations/`, `queries/`, `sqlc.yaml`, `compose.yaml` (dev profile: `postgres:18-alpine` + build target + healthcheck + migrate init step wiring). Spec: §3, §10.
- [x] **M0-T2** Config + observability skeleton. `VOX_*` env + `config.yaml` loader (§10 config list); `/healthz` (liveness stub), `/readyz` (not-ready stub), `/metrics` (Prometheus, empty registry + build info); structured `slog` with `tick/cell/charID` fields wired. Tests: config precedence (file < env), endpoint smoke tests.
- [x] **M0-T3** Dockerfile + CI + GHCR publish. Multi-stage `Dockerfile` (build + `migrate` one-shot compatible); workflow: build, vet, fmt-check, `go test ./...`, build+push `ghcr.io/dlukt/voxilian` on `main` (prod compose pulls it, §10). Include `testcontainers-go` availability check job. (CI migration-execution check lives in M1, not here — M0 has no migrations yet.)
- [x] **M0-T4** Test harness conventions. Fake clock + seeded RNG helpers (`internal/simtest` or similar), testcontainers PG 18 helper, golden-file helper reading repo-root `testdata/protocol/*.hex`. One example test each. Spec: §12.
- [x] **M0 exit criteria met** (pre-M1 meaning: image builds; default `compose up` brings postgres 18 to healthy with nothing depending on stubs; migrate/app run profile-gated; CI green. Full-stack `up` gates on M1-T8/M3.)

## M1 — Migrations + CAS store layer (first per reviewer order)

Exit: all §8 tables (incl. prototype catalogs) exist via goose; `Store` covers CRUD + CAS per aggregate; stale-write + CI-migration checks green.

- [x] **M1-T1** Migration `0001`: `citext` extension + `accounts` + `characters` (+ partial unique indexes `chars_acct_slot_uidx`, `chars_name_uidx` via `CREATE UNIQUE INDEX ... WHERE deleted_at IS NULL`). Spec: §8.
- [x] **M1-T2** Migration `0002`: prototype catalog tables — `spell_protos`, `skill_protos`, `item_protos`, `mob_protos`, `shop_listings` (stable INTEGER IDs `CHECK 1..65535`, symbolic keys, versions; spec §8.2). Catalogs BEFORE their dependents so FKs are created inline, never via later ALTERs.
- [x] **M1-T3** Migration `0003`: `character_spells`, `character_skills` (+PKs, FKs → catalogs, ability CHECK 1–99, stat CHECKs 1–50 on characters). Spec: §8.
- [x] **M1-T4** Migration `0004`: `item_instances` (+`revision`, proto FK → `item_protos`), `item_locations` (all 5 kinds, full per-kind CHECKs, self-containment), `corpses`, `banks` (+`revision`). Spec: §8.
- [x] **M1-T5** Migration `0005`: `ledger` (`num_nonnulls` CHECKs), `kills`, `bans/mutes`. Spec: §8.
- [x] **M1-T6a** sqlc: accounts/characters (+slot-claimed create → map unique-violation to `slot_occupied`/`name_taken`). Spec: §8.
- [x] **M1-T6b** sqlc: spells/skills get/set, item+location txn helpers, corpses, banks. (All catalog access lives in T6d.) Spec: §8.
- [x] **M1-T6c** sqlc: ledger/kill appends, bans/mutes. Spec: §8.
- [x] **M1-T6d** sqlc + store: catalog registry — load-all per table + transactional upsert (stable ID + version rules, §8.2). This is the ONLY write API `voxilian seed` may use. Spec: §8.2.
- [x] **M1-T7a** `Store` CAS: character aggregate (root CAS → child spells/skills in same txn). Stale-revision test. Spec: §8.1, D7.
- [x] **M1-T7b** `Store` CAS: item aggregate (root CAS → location row in same txn). Stale-revision test. Spec: §8.1.
- [x] **M1-T7c** `Store` CAS: bank balance + `Store` interface assembly. Stale-revision test + metric. Spec: §8.1.
- [x] **M1-T8** `voxilian migrate up/down/status` wired to embedded migrations (one-shot container compatible) + CI migrate-up-against-service-PG check (moved here from M0). Spec: §8, §10.
- [x] **M1 exit criteria met** (all §8 tables exist via goose; internal/store owns §8 CRUD/query coverage and exposes safe domain operations + CAS for mutable aggregates; stale-write + CI-migration checks green).

## M2 — Binary protocol + golden vectors

Exit: every §6 opcode encodes/decodes both sides (Go done; Godot side stubs + fixtures ready for client plan); fuzz + golden suites green.

- [x] **M2-T1** Codec core (`internal/proto`): envelope `[opcode|msg_version|seq|tick|payload]`, LE integers, `string`/`array`/`cell`/`pos`/`angle` primitives, 64 KiB frame cap, string/array caps (1024; chat 512; accessToken 8 KiB), `msg_version` trailing-skip, `[u16 entryLen]` entry framing. Round-trip tests. Spec: §6 framing.
- [x] **M2-T2** Opcodes `100/101/200/201/202` (hello/welcome/reauth/reauth_ok/error) + `216/217/219` (character_list/op, world_ready) + `121–126` (char CRUD/ack/leave). Round-trip tests. Spec: §6.1, §6.2.
- [x] **M2-T3a** Intents codec `102–120` + `126` (incl. `inputSeq`/`yaw`, fixed-`u32` `105 use`, vendor+stable-listing `114 buy`). Round-trip tests. Spec: §6.3.
- [x] **M2-T3b** Entity/stat codec `203–210`, `213–215` (incl. `lastProcessedInputSeq`, entryLen framing). Round-trip tests. Spec: §6.3.
- [x] **M2-T3c** Container codec `211/212/218/220` (`216` belongs to M2-T2) + FREEZE the `211` entry layout here: document every field+width in §6 first (spec-edit commit), then implement. Round-trip tests. Spec: §6.3.
- [x] **M2-T4** Golden binary fixtures: checked-in hex vectors under repo-root `testdata/protocol/` for ≥1 message per opcode; Go decode test; fixture format README for the Godot client plan. Spec: §12.
- [x] **M2-T5** Fuzz + robustness: Go fuzz targets per decoder; malformed/truncated/oversized corpus tests; unknown-opcode and unknown-trailing-bytes tolerance tests; `seq`/`inputSeq` wraparound tests (modulo-2³² arithmetic). Spec: §12.
- [x] **M2 exit criteria met** (all opcodes round-trip; fuzz 60 s clean per target; fixtures committed).

## M3 — Session lifecycle + gateway

Exit: full §6.1 state machine live over real WS; char CRUD end-to-end against PG; takeover/kick/backpressure tested.

- [x] **M3-T1** WS server + session registry (`sessionID → {sub,accountID,charID?,conn,state,tokenExp}`, indexed by sub+character; per-account lifecycle guard mutex). States + per-opcode permission table enforced; `bad_state` errors. Tests: illegal-state matrix. Spec: §6.1, §7.
- [x] **M3-T2** Auth hookup (JWT validation behind interface; startup-JWKS baseline: one fetch at construction, immutable key set, no cache/rotation yet): `100 hello` → JWKS check → account auto-provision → `200 welcome`; `101 reauth`; 90 s hard deadline (intents rejected post-grace, then disconnect). Tests with forged/expired tokens. Spec: §6.2, §11.
- [x] **M3-T3a** Character creation/domain + PostgreSQL persistence: name/NFC/policy validation; stats + 45-point ability validation behind an injected creation-content seam; transactional root + abilities + starter inventory; list/find/delete persistence primitives; Testcontainers including concurrent double-create race. No WS semantics. Spec: §6.1, §8, §9.
- [x] **M3-T3b** Character WS handler: 121 list, 122 create, 123 delete + 216/217/202 mappings; per-account deletion/in-use serialization; 126 leave_world via fake WorldExit seam with flush-before-unbind; real WebSocket integration tests. Opcode 124 belongs wholly to M3-T4. Spec: §6.1, §8, §9.
- [x] **M3-T4a** Enter-world baseline lifecycle: opcode 124 decode/lookup; atomic AUTHENTICATED→CHARACTER_SELECTED+bind; 217 enter OK; fake streaming BaselineProvider emits ordered 203/218/220; 219 world_ready barrier; atomic CHARACTER_SELECTED→IN_WORLD; baseline failure rollback; full per-account serialization. No duplicate-login kick yet: an existing same-account CHARACTER_SELECTED/IN_WORLD session returns retry as explicit staging. Spec: §6.1, §7.1.
- [x] **M3-T4b** Duplicate-login/takeover: replace T4a retry staging with final kick-old semantics; quiesce/flush old world state before new baseline; atomically release old binding; best-effort 202 kicked + forced close; refactor session Connection so all normal and cross-session binary writes share one writer serialization; same/different character takeover and simultaneous-enter tests. Spec: §6.1, §7.
- [x] **M3-T5a** Outbound queue/backpressure core: per-session bounded two-lane writer; exact byte+message budgets; critical FIFO; explicit coalescible-state keys/newest-wins; synchronous critical SendFunc preserves physical-write barrier; non-blocking TryCritical/TryState producer seam; critical saturation/write timeout fail-closed; terminal kicked/auth-expiry bypass; config + deterministic slow-writer tests. Spec: §7.1.
- [x] **M3-T5b** ACK + observability + resync: opcode 125 cumulative ACK flow control after world_ready; max-unacked window with modulo-2^32 tests; Prometheus saturation/depth/coalescing/ACK metrics; slow-client full reconnect+fresh-baseline test; load-ish fake slow peer. Spec: §6.1, §7.1.
- [x] **M3 exit criteria met** (lifecycle fuzz/property test: random opcode sequences never violate state table).

## M4 — Deterministic sim skeleton

Exit: 20 Hz tick loop, cells, server-authoritative movement with reconciliation anchors, handoff, history ring, CAS saver.

- [x] **M4-T1** Tick loop + cell/entity skeleton: fixed-step configurable tick loop (default 20 Hz), XZ 32 m cells with floor-correct negative coordinates, one in-process sim writer, opaque monotonic EntityID registry, deterministic cell/entity iteration, injectable clock+RNG, 2-second post-tick position history. No movement/handoff/AOI. Spec: §4, §5.
- [x] **M4-T2** Movement semantics/integration: sim-domain MoveIntent matching 102 (inputSeq/heldDirs/run/yaw/sampleTick), RFC1982 input coalescing, fixed-step walk/run integration, minimal CollisionWorld + volume flags, vigor RunGate hook, 205-compatible MovementUpdate/reconciliation anchor, and defensive displacement anomaly hook. No gateway NetEntityID/AOI fanout; M4-T5 wires transport/rate-limits/fanout. Spec: §5, §6.3, §11.
- [x] **M4-T3a** Cell ownership + entity handoff: per-entity {cell,generation} ownership epochs, canonical tick-start worklist preventing same-tick double processing, real movement-triggered cross-cell transfer with preserved history/control state, explicit RESIDENT/MIGRATING route state, and bounded migration MoveIntent queue (queue saturation is the sole later 202 retry condition). No opID/dedupe, PG reconciliation, gateway/AOI wiring, or saver. Spec: §5.1, §5.4.
- [x] **M4-T3b** Cross-cell op infrastructure: opaque Snowflake-style u64 OpID generation (41 ms / 10 worker / 12 sequence), owner-addressed retry-safe delivery, apply-once receiver semantics, bounded per-entity recent-OpID cache preserved across handoff, and synthetic aggregate delivery/lost-ACK/retry tests. No real combat/trade, PG reconciliation, gateway, or bus. Spec: §5.1, §5.5.
- [x] **M4-T3c** Post-commit reconciliation infrastructure: persisted-revision reconciliation fence installed only after successful PG commit and before commit notifications; sequential notifications may advance memory directly, dropped/gapped/failed notification forces a complete materialized-PG reload before the aggregate's next mutation. Synthetic multi-owner proof + PG18 integration using existing revisioned bank roots only as test fixtures; no real trade/bank gameplay or saver. Spec: §5.1, §5.6, §8.1.
- [x] **M4-T4a** Snapshot saver core: revision-tracked durable aggregate
  registry; latest-wins immutable full-snapshot dirty coalescing; per-root
  serialized CAS callback execution; 60 s periodic Clock-driven flush;
  synchronous critical write-through; stale-CAS reconcile block; and
  context-bounded manual/shutdown flush. Store-independent deterministic
  tests; no pgx, Prometheus, gameplay fields, or runtime wiring.
  Spec: §8.1, §8.3.
- [x] **M4-T4b** Saver persistence/operations proof: production
  internal/persist composition for character/item/bank Store CAS snapshots;
  stale-CAS mapping into T4a + T3c forced-reload reconciliation; exact
  saver-lag histogram observability while reusing Store's stale counter;
  real PG18 normal flush/write-through, mid-transaction crash rollback,
  ambiguous-commit convergence, and context-bounded shutdown-flush tests.
  No gameplay/AOI/runtime wiring.
  Spec: §5.6, §8.1, §8.3, §10.
- [x] **M4-T5a** AOI/presence/handle/rate-limit core:
  gateway-owned active presence registry; exact 96 m / 3-cell
  Chebyshev subscriptions with reverse cell index; session-local
  monotonic non-reused NetEntityIDs and visibility resolution;
  per-character active-presence movement/general token buckets; and
  heartbeat timestamp/stale-sweep primitives. Deterministic churn,
  stale-handle, rate, property, and concurrency tests. No WebSocket,
  sim-ingress, protocol fanout, or runtime lifecycle wiring.
  Spec: §4, §7, §7.2.
- [x] **M4-T5b1** Sim-owner ingress + movement/rate/lifecycle:
  bounded single-writer Engine command mailbox; concurrency-safe queued
  AddEntity / RemoveEntity / SubmitMove; real opcode 102 decode and sim
  routing; existing per-presence move/intent rate enforcement with exact
  202 mappings; staged sim world entry; presence activation after
  world_ready/CompleteEnterWorld; normal leave and duplicate-takeover
  runtime cleanup through the existing WorldExit barrier.
  Deterministic/race/real-WS integration tests. NO AOI 204/205/206
  fanout yet. NO heartbeat runtime yet.
  Spec: §5.2.10, §5.3–§5.4, §6.3, §7, §7.2, §7.3.
- [x] **M4-T5b2** AOI fanout + transport liveness (REOPENED v0.3.24 corrective closure):
  Presence entity->viewer reverse index; in-memory EntityPresentation
  source; one bounded 1024-event fanout pump over sim MovementUpdate
  (non-blocking sink, drop-counted); post-world-ready deterministic
  204 bootstrap with readiness barrier; live AOI reconciliation with
  recipient-local non-reused NetEntityIDs (204/206 critical);
  recipient-local ≤10 Hz 205 state fanout (own anchor vs observer 0);
  flush-first exit/takeover with fanout remove; raw-disconnect reaper
  with stale retention; 15 s Ping/Pong + 30 s stale sweep; real WS
  two-client/crossing/slow-viewer proofs. No M5 gameplay, no M10
   baseline replacement.
   Corrective v0.3.24 scope (spec §7.1.13 + §7.4.3/§7.4.5/§7.4.6/§7.4.7):
   targeted outbound CancelState before every 206 (queued
   205 → canceled, in-flight 205 → completes first; no new metric,
   no seq/ACK debt); exact post-admission control completion with a
   race-free Close/admission boundary and non-blocking OnMovement;
   emergency fanout ForgetSession ready/throttle invalidation in the
   post-sim-remove fallback; TransportLiveness.Close ownership of
   all Ping loops (stop sweep + every pinger, no future pinger,
   race-safe stop vs Close).
   Spec: §4, §5.2.10, §6.3, §7, §7.1, §7.2, §7.3, §7.4.
- [x] **M4 exit criteria met** (movement + handoff race tests green; saver property tests green).

## M5 — Combat + vitals + death

Exit: M59 combat/vitals/death playable against stub mobs; formulas golden-tested.

- [x] **M5-T1** Offense/defense/hit + weapon tables: `(Off*55)/Def` 10–95%, weapon type/quality, 1 swing/s, vigor costs, 30/hit + ⅓-HP caps, severity text hooks. Golden vectors from `meridian59.md` §7. Spec: §9.1, meridian59 §7. Pure sim-domain math only: NO gateway/opcode-103 wiring (103..120 stay rate-gate-then-delegate), NO armor/resist/spell/vitals/death work, NO `entity.go` fields.
- [x] **M5-T2** Armor/shields/resists: `ModifyDefensePower/Damage`, block/parry/dodge rolls, ±100 resist clip, spell-vs-weapon reduction rules. Golden vectors. Spec: §9.2 (frozen v0.3.26).
- [x] **M5-T3a** Generic spell core (pure/value sim-domain mechanics):
  generic spell success chance + d100 roll, generic AttackSpell damage
  scaling, Mana Focus damage contract, mana-cost arithmetic,
  vigor/exertion gate + cost contract, reagent-availability/substitution
  contract, karma requirement/gate, BaseMaxHP/min-hitpoint gate, post-cast
  cooldown timing, cast/trance duration math, full vs failed cast
  resource-payment plan, item-cast differences, absolute/resistance policy
  representation. No live mutation, no gateway, no world/target lookup.
  Spec: §9.3a (frozen v0.3.27).
- [x] **M5-T3b** Special spell-damage archetypes (pure/value deterministic
  mechanics): touch attacks, wall periodic damage/timing, Earthquake/AoE
  falloff, Illusionary Wounds, Vampiric Drain / damage-derived side-effect
  hooks, other special spell-damage formulas needed by the MVP. Still no
  authoritative HP mutation, no room/world-object scheduler, no gateway, no
  real inventory/reagent mutation. Spec: §9.3b (frozen v0.3.28).
- [x] **M5-T4a** Authoritative vitals state/mutation/math core (pure/value
  sim-domain only): canonical `PlayerVitals` (HP/BaseMaxHP/MaxHP/Mana/
  MaxMana/Vigor/RestThreshold/Exertion-accumulator/Stomach), HP mutation
  trio (loss, normal heal, over-max heal), base-max/max-HP primitives,
  mana loss/capped/uncapped gain, initial/node/double-node/compute mana,
  strict HasVigor, general vs rest exertion accumulators, 10..100
  threshold, health/mana/rest pure intervals with resolved Jala/Focus
  seams, lazy seconds-based stomach decay + eat-capacity seam, durable
  JSON (`hp/base_max/max/mana/max_mana/vigor/threshold/stomach` +
  `exertion`, missing-means-zero) with creation-compatibility proof.
  Golden/property/fuzz-seed tests; NO timers/scheduling, NO entity.go/
  engine.go/ingress.go/saver.go changes, NO death. Spec: §9.4 (frozen
  v0.3.29), meridian59 §4.
- [x] **M5-T4b1** Live player-vitals entity integration (NO scheduling):
  attach/load validated `PlayerVitals` values to player entities under
  the single-writer model (value copies, no caller aliasing); player vs
  generic entity classification without overloading EntityID; immutable
  inspection distinguishing not-a-player from zero-valued vitals;
  owner-local mutation APIs composing the T4a production helpers (HP
  loss/normal/over-max heal, base/max HP adjust, mana loss/capped/
  uncapped gain, max-mana adjust, exertion + rest exertion, threshold);
  narrow non-blocking immutable dirty/event seam firing only on real
  state change; authoritative player vigor run gate (Vigor >= 10, not
  strict HasVigor) with generic entities keeping the injected M4 gate;
  handoff-preservation proof. NO regen/rest timers, NO acted-since-entry,
  NO stomach anchor, NO death, NO gateway, NO Store/persist imports.
  Spec: §9.4b.2–§9.4b.9, §9.4b.18–§9.4b.20.
- [x] **M5-T4b2** Deterministic vitals scheduling/runtime: health/mana
  due scheduling with NewHealth/NewMana create/cancel/keep semantics and
  acted-since-entry gating; rest lifecycle/deadlines (always re-arm,
  below-threshold recovery only); actedSinceEntry gate + owner-local
  hooks incl. movement/turn routing and resolved room-entry reset policy
  boundary; deterministic timer ordering (canonical entity order, fixed
  health->mana->rest slot order, at-most-one event per slot per Step)
  over the ONE CastTicks-equivalent ms->tick conversion with serial32
  due comparison; runtime resolved regen inputs (no live lookups);
  stomach sim-time anchor with fractional-second preservation and
  documented no-offline-digestion MVP; handoff/no-double-fire scheduling
  proof; manual-tick/manual-clock timer tests. v0.3.31 composition
  freeze: §9.4b.14a plain-value runtime-input ownership with validating
  update API and never-restart timing; §9.4b.17 frozen per-entity Step
  phase order (movement before vitals runtime, acted marked at input
  consumption, one tick-start worklist); §9.4b.22 live post-commit
  validity guard through the one commit seam; §9.4b.3a atomic
  player-initialization signature (vitals + runtime inputs validate
  before EntityID consumption, no default stats); §9.4b.10 explicit
  armed/present deadline bit.
  Spec: §9.4b.1, §9.4b.3a, §9.4b.10–§9.4b.17, §9.4b.19, §9.4b.21–§9.4b.22.
- [x] **M5-T5a** Pure/source-faithful death mechanics and immutable
  planning: disposition decision (avoided/cheap/normal from RESOLVED
  inputs), double-death guard decision, corpse policy constants, ordered
  drop plan from resolved per-item policy + PK-protection metadata,
  immediate advancement reset/halve plan, post-death vitals calculation
  (Validate-valid `PlayerVitals`), persistence-agnostic pending-death
  plan value, DeathCost validation, Portal-of-Life pure cost, delayed
  cost normalization/newbie scaling, delayed BaseMaxHP penalty via T4a
  composition, per-spell/per-skill penalty planning with exact frozen
  d100 boundaries + RNG consumption order, justice/guild hook flags.
  NO live mutation, NO Store/PG, NO corpse DB row, NO gateway/proto,
  NO Underworld teleport, NO respawn, NO opcode 120, NO migration.
  Spec: §9.5 (frozen v0.3.33), meridian59 §9.5.
- [x] **M5-T5b1a** Durable death schema + SQL primitives (depends on
  T5a; spec §9.5.8a): ONE narrow goose migration
  `0006_death_persistence.sql` (`pending_deaths` keyed by character:
  effective cost 0..100, whole-second death time, nullable corpse FK
  `ON DELETE SET NULL`, `portal_used` once-per-corpse flag, partial
  unique live-corpse guard; `item_pk_protections` keyed by item:
  victim FK + absolute expiry, item-aggregate child) plus low-level
  sqlc primitives (`Insert/Get/DeletePendingDeath`,
  `Upsert/Get/DeleteItemPKProtection`) for T5b1b to compose. NO
  `CommitDeathEntry`, NO character/item death CAS composition, NO
  kill/ledger composition, NO sim/gateway/proto change, NO Portal
  mutation, NO penalties, NO expiry worker.
- [ ] **M5-T5b1b** Atomic immediate death-entry Store transaction
  (depends on T5b1a): ONE atomic critical Store operation conceptually
  `CommitDeathEntry(ctx, plan)` covering pending-death recovery state,
  character immediate death state, corpse row (generated ID composed
  inside the txn — never fabricated by sim), droppable item
  relocation, PK-drop metadata persistence, advancement immediate
  reset/halve, kill/ledger audit rows in the same txn; deterministic
  lock/CAS order (character root first, item roots ascending ItemID);
  replay rejection via the `pending_deaths` PK mapped to
  `ErrDeathAlreadyPending`; stale-revision/crash/commit-ambiguity
  proof per §8.1/§8.3. Spec: §9.5.1, §9.5.8a, §9.5.18.
- [ ] **M5-T5b2** Durable delayed Underworld-exit penalties (depends on
  T5a + T5b1b): ONE
  separate atomic critical Store operation conceptually
  `CommitDeathPenalties(ctx, plan)` covering pending DeathCost
  consumption (Portal-of-Life lowers-only persistence), HP/ability
  penalty application through character/ability CAS, justice flag
  writes, clearing pending-death state, ledger rows in the same txn;
  penalties-exactly-once, never-skip, stale/crash/commit-ambiguity
  proof per §8.1/§8.3. Spec: §9.5.1, §9.5.11–§9.5.14.
- [ ] **M5-T5c** Live runtime + transport death integration (depends on
  T5b1b + T5b2 + T4b2): zero-HP → death orchestration on the sim owner
  (composing LoseHealth's ZeroHP hook), dead/Underworld lifecycle state,
  resolved world-target/placement seams (no hard-coded Underworld
  coordinates), rest/regen cancellation/composition with §9.4b runtime,
  C→S 120 RespawnAck handling, S→C 214 Death / 215 Respawn transport,
  reconnect/crash recovery through the durable pending-death state,
  final end-to-end death lifecycle proof. Spec: §9.5.1, §9.5.15,
  §9.5.16.
- [ ] **M5-T6** Personal/world-light intents: `115 rest`, `116 eat` (hunger/vigor effects), `105 use` (skill/item dispatch incl. Second Wind), `119 safety_toggle`, `117/118 → 209` chat (+channel rules, length caps, rate limits). Owner of these opcodes: this task, no other. Spec: §6.3, §9.
- [ ] **M5-T7** Authoritative attack/cast runtime integration (depends on
  M5-T1, M5-T2, M5-T3a, M5-T3b, M5-T4a,   M5-T4b1, M5-T4b2,
  M5-T5-complete (T5a+T5b1a+T5b1b+T5b2+T5c), M5-T6). Owns real C→S 103
  attack routing, real C→S 104 cast routing, typed sim-owner combat
  commands, composition of T1/T2/T3a/T3b mechanics, authoritative T4
  HP/mana/vigor mutation, T5 death handoff, T6 safety/personal-state
  interaction where required, runtime cooldown ownership, spell/weapon/
  loadout resolver seams, actual mana/exertion charging, reagent
  transaction/inventory seam, damage application, hit/cast result events,
  gateway result transport, scripted-duel integration proof.
  Spec: §9.3c (frozen v0.3.27).
- [ ] **M5 exit criteria met** (scripted duel bot via M5-T7: two M4+M5 entities fight to death, corpse lootable).

## M6 — Progression (use-based)

Exit: skills/HP advance by use per M59 math; trainers gate by learn points.

- [ ] **M6-T1** Ability improvement: success formula, two-gate +1%, 10 pts/15–22 min window, 75-swing checks, softcaps, write-through milestones. Golden vectors. Spec: §9, meridian59 §6.
- [ ] **M6-T2** HP advancement + learn points: highmark roll, Stam effects, newbie bonus, anti-buff halving; learn-points gate (16+Int, top-3 rule) at trainer NPCs; soft-delete-safe. Tests incl. boundary rolls. Spec: §9.
- [ ] **M6-T3** Forget/atrophy hooks: forget potions + Forget spell wiring (atrophy stays disabled per spec — config flag `atrophy=false`, code path present but off). Spec: §9.
- [ ] **M6 exit criteria met** (bot grinds 1→99 tracked skill on schedule curve within tolerance).

## M7 — Mobs + AI + loot

Exit: bestiary live with spawners, AI timers, caster mobs, loot rolls.

- [ ] **M7-T1** Mob protos + spawners: proto table (level/diff/karma/atk/resists/spells/loot TID) loaded from seed files; volume-bound spawners + respawn timers; HP/Off/Def/damage formulas golden-tested. Spec: §9, meridian59 §8.
- [ ] **M7-T2a** AI timers + flags: chase/move/attack staggers, aggro/flee/regroup/karma-aggro, spawn/wander behavior. Timer tests (fake clock). Spec: §5, §9.
- [ ] **M7-T2b** Caster mobs: `MonsterCastSpell` chance tables, mana gating, cast timing vs melee. Spec: §9.
- [ ] **M7-T2c** NPC movement: pathing on cell grid + Fine collision for NPC moves (server-validated, unlike M59 players), migrating-mob handoff races. Spec: §5, §9.
- [ ] **M7-T3** Loot: TID tables, enchanted-drop chances, corpse/world placement via `item_locations`, PK-tag rules. Distribution tests (χ²-ish sanity on seeded RNG). Spec: §9.
- [ ] **M7-T4** World item manipulation: `106 get`, `107 drop`, `108 put`, `109 give` (ownership transitions via item aggregate CAS, cross-cell via coordinator+opID). Stale-handle + race tests. Spec: §6.3, §8.1.
- [ ] **M7 exit criteria met** (bounded CI smoke soak 5–15 min: spawners/AI/loot stable, no stuck mobs in metrics; 24 h soak recorded manually at launch per M12-T4).

## M8 — Trade / bank / vault

Exit: atomic player trade + bank/vault with ledger audit; race tests green.

- [ ] **M8-T1** Trade state machine: offer/counter/accept/cancel (`110–113`, `212/213`), both-party re-confirm, single-PG-txn accept (ownership + ledger, §8.1), lawful-refusal + PK-tag rules, double-accept race test. Spec: §9, §6.3.
- [ ] **M8-T2** Bank + vault + vendor buy: per-character per-system balances, vault `item_locations` kind=3 with region, fees to ledger, `114 buy` against `220` listings (stable listing IDs), cross-cell trade via coordinator+opID path. PG-loss-mid-trade test (unready rejects). Spec: §9, §10.
- [ ] **M8 exit criteria met** (trade fuzz: random interleavings never duplicate/lose items or coins — ledger reconciles).

## M9 — Seed data: full M59 content port (parallelizable after M9-T1; each school/bestiary batch ≈ one session)

Exit: every school/skill/mob/weapon/armor in versioned seed files, `voxilian seed` idempotent into catalog tables.

- [ ] **M9-T1** Seed pipeline: file format + validator (CHECK-mirroring) + `voxilian seed` built ON the M1-T6d registry API (no direct pgx outside `store`) + idempotent version-ruled upsert. One sample school to prove it. The pipeline/registry must also carry the M3 creation metadata (new-character eligibility, initial ability values, free default spell, starter Mace/Coins protos, starter spawn/hometown, blocklist/reserved-name policy). Spec: §8.2 (new), §10 seed bullet.
- [ ] **M9-T2** School Shal'ille (all spells: costs/effects/reqs). Spec: meridian59 §5.
- [ ] **M9-T3** School Qor. Spec: meridian59 §5.
- [ ] **M9-T4** School Kraanan. Spec: meridian59 §5.
- [ ] **M9-T5** School Faren. Spec: meridian59 §5.
- [ ] **M9-T6** School Riija. Spec: meridian59 §5.
- [ ] **M9-T7** School Jala spell definitions (songs as room-enchant data). Hinder-matrix SYSTEM is phase 2 / non-MVP — port the data, do not gate MVP completion on the matrix. Spec: meridian59 §5.
- [ ] **M9-T8** Weaponcraft + utility skills (strokes, proficiencies, parry/dodge/block/disarm/second wind). Spec: meridian59 §5.
- [ ] **M9-T9** Bestiary batch 1 (protos + loot TIDs + spawner placements; alphabetical third 1/3). Spec: meridian59 §8.
- [ ] **M9-T10** Bestiary batch 2 (third 2/3). Spec: meridian59 §8.
- [ ] **M9-T11** Bestiary batch 3 (third 3/3) + bosses. Spec: meridian59 §8.
- [ ] **M9-T12** NPC roles (teachers/vendors/bankers/priests + vendor proto listings for `220`). Spec: meridian59 §8.
- [ ] **M9-T13** Weapons/armor/shields/ammo protos. Spec: meridian59 §7.
- [ ] **M9 exit criteria met** (`seed` from scratch reproduces full content; counts asserted in test).

## M10 — World sources + streaming

Exit: both `classic` (starter region) and `procedural` (seeded) playable; portals seamless; chunk pacing verified.

- [ ] **M10-T1** `WorldSource` interface + `world.toml` constants (chunk 16³, cell 32 m, AOI 96 m, volume flags, portal format — Godot-agreed). Spec: §4, §13.1/13.5.
- [ ] **M10-T2a** Classic starter region: hometown + surroundings + newbie area (M59-inspired layouts, NOT copies); volume flags. Spec: §4, D1.
- [ ] **M10-T2b** Classic Underworld region + volume flags + respawn targeting. Spec: §4, §9 death.
- [ ] **M10-T2c** Classic first dungeon band + portal remaps/snapshot swaps (no-loading-screen guarantee testable via session continuity). Spec: §4, D1.
- [ ] **M10-T3** Procedural generator v1: deterministic seeded terrain (versioned algo id), same cell/volume/flag pipeline; client-regenerable from `world{mode,seed,version}`. Determinism test (same seed → identical cells). Spec: §4.
- [ ] **M10-T4** Real baseline integration: swap M3's fake `BaselineProvider` (completed by T4a/T4b) for real cells/chunks/vendors; `world_ready` barrier + reconnect-mid-stream recovery + bandwidth-vs-low-end-budget measurement. Spec: §6.1, §6.3, §7.1.
- [ ] **M10 exit criteria met** (walk classic→portal→dungeon with zero session breaks, traced in test).

## M11 — Keycloak integration + auth hardening (backend-only; Godot PKCE handshake lives in a separate client plan)

Exit: E2E login → WS → world against configured realm using a headless test OAuth client; admin role enforced.

- [ ] **M11-T1** Realm/client config as code (realm export JSON in repo `deploy/keycloak/`): public client, PKCE S256, loopback redirect, `openid profile email offline_access`, `vox-admin` client role, token TTLs (decide here: propose access ~5 min; record decision in spec §13.4 via spec-edit commit). Spec: §11, §13.4.
- [ ] **M11-T2** Backend JWKS validation hardening (upgrades the M3-T2 immutable startup key set, does not create the first validator): background cache + TTL, key rotation, retry/backoff, stale-key behavior, `iss/aud` pinning, pre-auth per-IP rate limits, expired/misissued → `202 error{session_expired}`. Expanded adversarial tests (none-alg, wrong iss, expired, garbage). Spec: §11.
- [ ] **M11-T3** Admin path: `vox-admin` claim → WS admin role + `voxilian admin` CLI parity (kick/ban/save-now/spawn/give-logged). Negative tests (non-admin rejected). Spec: §10, §11.
- [ ] **M11 exit criteria met** (headless test OAuth client: login against dev Keycloak container → enter_world → reauth → expiry → grace → disconnect; Godot handshake tracked in client plan, not here).

## M12 — Ops hardening + load + launch readiness

Exit: prod compose deployable; outage/shutdown behaviors demonstrated; load gates pass.

- [ ] **M12-T1** Prod compose + GHCR: prod profile verified (external PG DSN, migrate one-shot, TLS termination per §13.6 decision — STILL OPEN, decide here), backup/PITR confirmation recorded in §13.6 (spec edit). Spec: §10.
- [ ] **M12-T2** PG-outage + shutdown drills: `/readyz` flip test, login/critical-op rejection test, 60 s movement grace test, SIGTERM sequence test (flush deadline honored), panic-bypass recovery test (CAS invariants hold). Spec: §10.
- [ ] **M12-T3** Load harness + gates: botnet (random-walk + attack + chatter) measuring tick p99, AOI bytes, saver lag, queue saturation on min-spec-shaped budget; gate numbers recorded; sharding decision data pack. Slow-client + reconnect-storm tests. Spec: §12.
- [ ] **M12-T4** Launch checklist: seed counts, realm export, runbook (migrate/rollback, ban/kick, snapshot-now, log triage), nightly 24 h soak result RECORDED (manual/nightly job — not an LLM blocking task), `docs/` updated, spec version bumped to v1.0-frozen.
- [ ] **M12 exit criteria met** (all prior milestones `[x]`; bounded smoke soaks green + 24 h result recorded with zero item/coin divergence in ledger reconciliation).

---

## Task index (for LLM dispatch: one row = one session)

| ID | Depends on | Touches (typical) |
|---|---|---|
| M0-T1…T4 | — | `backend/voxilian` root, compose, CI |
| M1-T1…T5 | M0 | `backend/voxilian/migrations/` only |
| M1-T6a…c, T7a…c, T8 | per-table predecessor migrations | `backend/voxilian/queries/`, `internal/store` |
| M2-T1, T2, T3a…c, T4, T5 | M0 | `backend/voxilian/internal/proto`, repo-root `testdata/protocol/` |
| M3-T1, T2 | M1-T8 (migrate CLI), M2-T1, M2-T2 | `backend/voxilian/internal/{auth,gateway,session,store}` |
| M3-T3a | M1-T7a (character CAS), M2-T2 | `backend/voxilian/internal/character`, `internal/store` |
| M3-T3b | M3-T3a | `backend/voxilian/internal/{gateway,session,character}` |
| M3-T4a | M1-T7a, M2-T3b, M2-T3c, M3-T3b | `backend/voxilian/internal/{gateway,session}` |
| M3-T4b | M3-T4a | `backend/voxilian/internal/{gateway,session}` |
| M3-T5a | M3-T4b | `backend/voxilian/internal/{gateway,config}` |
| M3-T5b | M3-T5a | `backend/voxilian/internal/{gateway,session,observe}` |
| M4-T1, T2 | M2-T3a/b, M3-T1 | `backend/voxilian/internal/sim` |
| M4-T5a | M2-T3a/b, M3-T1 | `backend/voxilian/internal/gateway` |
| M4-T5b1 | M4-T5a | `backend/voxilian/internal/{sim,gateway}` |
| M4-T5b2 | M4-T5b1 | `backend/voxilian/internal/gateway` (existing observe metrics only, no new family; no sim production change beyond a compile-time assertion if needed) |
| M4-T3a…c, T4a, T4b | M1-T7a…c (CAS), M3-T1 | `backend/voxilian/internal/sim`, `internal/store` |
| M5-T1/T2/T3a/T3b | M4-T1, M4-T2 | `backend/voxilian/internal/sim` (pure mechanics only: no live vitals, no gateway) |
| M5-T4a | M4-T1, M4-T2 | `backend/voxilian/internal/sim` (pure vitals state/math only: no entity fields, no timers, no gateway) |
| M5-T4b1 | M5-T4a, M4-T1, M4-T2 | `backend/voxilian/internal/sim` (player-entity vitals attachment/inspection/mutation/dirty seam/run gate; no scheduling) |
| M5-T4b2 | M5-T4b1 | `backend/voxilian/internal/sim` (deterministic vitals scheduling: deadlines, rest, acted-since-entry, stomach anchor) |
| M5-T5a | M5-T1, M5-T2, M5-T3a, M5-T3b, M5-T4a, M5-T4b1, M5-T4b2 | `backend/voxilian/internal/sim` (pure death mechanics/plans only: no live vitals, no store, no gateway) |
| M5-T5b1a | M5-T5a | `backend/voxilian/{migrations,queries,internal/store}` (durable death schema + SQL primitives; one narrow pending-death/PK migration) |
| M5-T5b1b | M5-T5b1a | `backend/voxilian/{queries,internal/store,internal/persist}` (atomic immediate death entry; no new migration) |
| M5-T5b2 | M5-T5a, M5-T5b1b | `backend/voxilian/{queries,internal/store,internal/persist}` (durable delayed penalties; no second migration unless audit requires) |
| M5-T5c | M5-T5b1b, M5-T5b2, M5-T4b2 | `backend/voxilian/internal/{sim,gateway}` (live death orchestration, 120/214/215, recovery) |
| M5-T6 | M4-T1, M4-T2 | `backend/voxilian/internal/sim` + gateway only for its own listed personal intents |
| M5-T7 | M5-T1, M5-T2, M5-T3a, M5-T3b, M5-T4a, M5-T4b1, M5-T4b2, M5-T5a, M5-T5b1a, M5-T5b1b, M5-T5b2, M5-T5c, M5-T6 | `backend/voxilian/internal/{sim,gateway}` (authoritative 103/104 runtime integration) |
| M6-T1…T3 | M5-T1…T5 | `backend/voxilian/internal/sim` (progression) |
| M7-T1 | M9-T1 (seed pipeline) + M1-T6b | `backend/voxilian/internal/sim`, `seed/` fixtures |
| M7-T2a…c, T3, T4 | M4-T1…T3a, M1-T7b | `backend/voxilian/internal/sim` |
| M8-T1, T2 | M1-T7a…c, M3-T1, M4-T3b/c | `backend/voxilian/internal/sim`, `internal/store` |
| M9-T1 | M1-T2 (catalog tables) + M1-T6d (registry API) | `backend/voxilian/seed/` + validator |
| M9-T2…T13 | M9-T1 | `backend/voxilian/seed/` content only |
| M10-T1…T2c | M4-T1 | `backend/voxilian/internal/world`, `seed/world` |
| M10-T3 | M10-T1 | generator only |
| M10-T4 | M3-T4a/T4b (fake baseline), M10-T1…T2c | `backend/voxilian/internal/{gateway,world}` |
| M11-T1…T3 | M3-T2 | `backend/voxilian/deploy/keycloak`, `internal/auth` |
| M12-T1…T4 | all | compose, docs, runbook |

## Appendix A — C→S opcode ownership (every gameplay opcode has exactly one owner)

| Opcode(s) | Owner task | Notes |
|---|---|---|
| 100/101 hello/reauth | M3-T2 (+M11-T2 hardening) | auth plane |
| 102 move | M4-T2 | M4-T2 owns authoritative move semantics; M4-T5b1 owns wire decode/rate/owner-mailbox routing; M4-T5b2 owns 205 AOI transport fanout; none reimplement movement rules. |
| 103/104 attack/cast | M5-T7 | combat plane. T1/T2/T3a/T3b own pure mechanics; T7 alone owns wire/runtime integration. |
| 105 use, 115 rest, 116 eat, 119 safety, 117/118 chat | M5-T6 | personal intents |
| 106/107/108/109 get/drop/put/give | M7-T4 | world items |
| 110–113 offer/counter/accept/cancel | M8-T1 | trade machine |
| 114 buy | M8-T2 | vendor listings |
| 120 respawn_ack | M5-T5c | death pipeline live integration |
| 121–123 char CRUD, 126 leave | M3-T3b | lifecycle |
| 124 enter_world | M3-T4a baseline, M3-T4b takeover | lifecycle |
| 125 ack | M3-T5b | flow control |

## Plan history

- v1.18: split M5-T5b1 into schema-first T5b1a (durable death schema +
  SQL primitives) + transactional T5b1b (atomic `CommitDeathEntry`)
  and freeze the T5b1 persistence architecture (spec v0.3.34 §9.5.8a:
  `pending_deaths` keyed by character with 0..100 cost, whole-second
  death time, nullable corpse FK `ON DELETE SET NULL` so pending death
  survives corpse expiry, `portal_used` once-per-corpse flag, PK-violation
  replay sentinel `ErrDeathAlreadyPending`, character-aggregate CAS
  ownership; `item_pk_protections` dedicated item-aggregate child with
  victim FK + absolute expiry and deterministic UPSERT replacement;
  `corpses` unchanged; generated corpse-ID composition inside T5b1b;
  character-first/ascending-item CAS order; kill rows only for
  character/mob killers; no invented death ledger kinds). Dependency
  shape T5b1a <- T5a <- T5b1b <- (T5b2, T5c); M5-T5-complete =
  T5a+T5b1a+T5b1b+T5b2+T5c. Docs only; T5b1a/T5b1b/T5b2/T5c/T6/T7 stay
  `[ ]`.

- v1.17: freeze M5 soldier-shield death outcome (spec v0.3.33 §9.5.14,
  `meridian59.md` §9.5): `Player.Killed` invokes the currently-used
  `SoldierShield` `OwnerDied(what=killer)` in the NORMAL-death branch
  with source default `logoff = FALSE`; complete immutable
  `soldierShieldDeathEffect` (no effect / delete shield / survive with
  exact post-death faction rank) replaces the insufficient bare
  `soldierShieldDied` boolean; rank 1..3 deleted, rank 4..10 survive
  with exact vectors 4->1, 5->1, 6->2, 7->3, 8->4, 9->5, 10->6, gated
  on Normal + shield present + `IsEnemyAttack(killer)`; `logoff = TRUE`
  stays in the separate logoff-ghost system, out of the T5a immediate
  `Killed` planner; unconditional `DeathNormal + FrenzyActive`
  rejection frozen. Docs only; M5-T5a stays `[x]`, T5b1/T5b2/T5c/T6/T7
  stay `[ ]`.

- v1.16: freeze M5 death semantics after the upstream source audit
  (spec v0.3.32 §9.5) and replace the oversized single M5-T5 row with
  four tasks: T5a pure/source-faithful death mechanics + immutable
  plans (disposition avoided/cheap/normal from resolved inputs,
  double-death guard, corpse constants, ordered drop plan + PK
  protection policy, advancement reset/halve plan, post-death vitals,
  persistence-agnostic pending-death plan value, DeathCost domain,
  Portal-of-Life pure formula, delayed cost scaling/HP penalty/ability
  penalties with exact d100 boundaries and RNG order, hook flags; no
  store/gateway/proto/migration), T5b1 durable immediate death-entry
  transaction (one narrow pending-death migration — current schema
  audited insufficient; corpse row, item relocation, PK-drop metadata,
  advancement reset, kill/ledger, crash/ambiguity proof), T5b2 durable
  delayed Underworld-exit penalties (separate atomic transaction,
  lowers-only Portal persistence, exactly-once proof), T5c live
  runtime + transport integration (zero-HP orchestration, 120/214/215,
  resolved world-target seams, recovery, end-to-end proof). M5-T7 now
  depends on T5-complete (T5a+T5b1+T5b2+T5c), not merely T5a.
  `meridian59.md` §9.5 rewritten against source: avoided-vs-cheap
  distinction (arena/prison/safe-player-attack avoid death entirely;
  frenzy/newbie-room/newbie-honor/token are cheap real deaths that
  still create a corpse), PFLAG_TUTORIAL inversion (flag TRUE =
  no-longer-newbie; still-newbie non-murderer gets cost/3, the
  experienced-or-murderer branch rolls HP), HP roll boundary `<=` vs
  ability rolls `>`/`<`, angel mail + mana (MaxMana/2)+2 override for
  still-newbie non-murderers, newbie-range deaths respawn at the
  newbie home rather than the Underworld, special-item artifact loss
  ordering before the token check, PK pointer 10 min policy separated
  from storage, corpse 600000/25000 ms constants, whole-second death
  time, once-per-corpse Portal flag, KOD division = C truncation
  toward zero. T5a/T5b1/T5b2/T5c, T6, T7, and M5 exit all stay `[ ]`.

- v1.15: narrow M5-T4b2 composition freeze (spec v0.3.31 §9.4b.3a/
  §9.4b.10/§9.4b.14a/§9.4b.17/§9.4b.22) closing the three gaps exposed
  by the verified T4b1 integration: runtime-input ownership (one
  plain-value PlayerVitalsRuntimeInputs snapshot on the player entity,
  validating owner-local update API, input changes never restart a
  running deadline, fire-time rest multiplier, no stale cache); exact
  per-entity Step phase ordering (movement consume/integrate/output ->
  health->mana->rest runtime -> history sample, in the ONE tick-start
  worklist, acted marked at input consumption before timers, Vigor-9
  same-tick walk documented); live post-commit validity (one narrow
  commit guard requiring after.Validate(), bit-identical state and no
  event on failure, T4a AdjustMaxMana/ComputeMaxMana pures stay
  unbounded; MaxMana 20-19 -> 1 allowed, 20-20 rejected). Plus atomic
  T4b2-complete player initialization (vitals AND runtime inputs
  validate before EntityID consumption, §9.4b.9 state installed
  atomically, no invented default Stamina/Mysticism) and the explicit
  deadline armed/present bit (due==0 is a valid wrapped deadline;
  restArmed IS the resting state). Task checkbox state unchanged:
  M5-T4b1 [x], M5-T4b2 [ ], T5/T6/T7 [ ], M5 exit [ ].

- v1.14: split M5-T4b into live entity integration T4b1 (player-vitals
  attach/classification/inspection, owner-local T4a-composing mutation
  surface, immutable dirty/event seam, Vigor>=10 player run gate,
  handoff proof; no scheduling) plus deterministic scheduling T4b2
  (deadline-slot runtime with one canonical ms->tick conversion,
  NewHealth/NewMana keep-deadline semantics, actedSinceEntry first-action
  gate, rest always-re-arm lifecycle, resolved regen inputs, stomach
  sim-time anchor, no-double-fire handoff proof, manual-clock tests) and
  freeze the runtime semantics (spec v0.3.30 §9.4b); M5-T5's formal
  dependency corrected from M5-T4a to T4b-complete (death composes
  owner-local vitals mutation and interacts with the rest lifecycle);
  M5-T7 deps updated to name T4b1/T4b2 explicitly; recorded
  `meridian59.md` timer-runtime audit facts (regen action gating is
  HP-only, flag reset exceptions, create/cancel/keep timer semantics,
  timer.c ordering, Vigor>=10 run threshold). M5-T4b1/T4b2/T5/T6/T7 and
  M5 exit stay `[ ]`.

- v1.13: split M5-T4 into pure core T4a + entity/scheduling T4b and
  freeze M5-T4a vitals semantics (spec v0.3.29 §9.4: canonical vitals
  value, HP/mana mutation trios, node arithmetic with Fey-only double,
  strict HasVigor/>20000 exertion rules, 10..100 threshold, pure
  health/mana/rest intervals with resolved seams, seconds-based lazy
  stomach decay with 1..100 post bound, durable JSON with
  missing-exertion-means-zero, T1/T2/T3 composition boundary; T4b owns
  entity attachment/scheduling/moved-gating/handoff/tick integration;
  old "timer tests" wording moves to T4b) + verified `meridian59.md`
  corrections (no Vale multiplier, stomach bound/units/vigor points,
  mana-only BOOST_DECAY). M5-T4a/T4b/T5/T6/T7 and M5 exit stay `[ ]`.

- v1.12: freeze M5-T3b special spell semantics (spec v0.3.28 §9.3b:
  touch proficiency/damage/duration/Holy Touch with dead viHit_Factor
  excluded, Illusionary Wounds /100 bases with MaxHP/3 then HP-1 caps
  and applied-amount refund, Vampiric Drain post-application heal,
  Earthquake severity/squared-falloff/modes, wall max-damage/lifetime/
  period/ordinary/illusionary mechanics with IW reuse) + verified
  `meridian59.md` corrections (wall families, IW precision). M5-T3b
  stays `[ ]`, M5 exit stays `[ ]`.
- v1.11: split M5-T3 into generic core T3a + special archetypes T3b and
  add M5-T7 authoritative 103/104 combat runtime after T1..T6 (spec
  v0.3.27 §9.3/§9.3a/§9.3b/§9.3c: T3a generic success/cost/timing/damage
  semantics, T3b boundary families, T7 runtime ownership with M5 exit
  after T7, repaired 103/104 ownership gap, no live vitals/gateway in
  T3a). M5-T1/T2 stay `[x]`; T3a/T3b/T4/T5/T6/T7 and M5 exit stay `[ ]`.
- v1.10: freeze M5-T2 defense mitigation semantics (spec v0.3.26 §9.2:
  T2 ownership/pipeline boundaries, T2-resolved PlayerDefense
  components, exact capability gates, shared skill-chance formula,
  Parry/Dodge-never-rolled with Block-only shield gating, DefensePower
  sums with shield exclusion, exact damage-reduction/damage-class/
  multi-modifier rules, block rating, resistance tag/matching/
  aggregation/transform contracts, durability/spellmod/catalog
  exclusions, golden vectors, property invariants) + verified
  `meridian59.md` corrections (capability mapping, piBlockBonus dead,
  disciple robe, per-side resistance clip, unified transform). M5-T2
  stays `[ ]`, M5 exit stays `[ ]`.
- v1.9: freeze M5-T1 weapon-combat semantics (spec v0.3.25 §9.1: T1/T2/T3/
  T4/T5 ownership boundaries, exact offense/defense/monster-rating/hit/
  weapon-table/damage-stage/cap/severity/cooldown/vigor contracts, no
  opcode-103 gateway wiring in T1, no `entity.go` fields) + verified
  `meridian59.md` corrections (ceil ⅓-cap on BaseMax both sides, buffed-Max
  `>=` forced severity, no-double-count damage order, strokeless vigor
  gate, optimistic cooldown arming). M5-T1 stays `[ ]`, M5 exit stays `[ ]`.
- v1.8: correct M4-T5b2 fanout closure semantics (spec v0.3.24) and
  REOPEN M4-T5b2 `[ ]` — targeted outbound state cancellation before
  206, exact post-admission control completion, race-free
  Close/admission boundary, emergency fanout ready/throttle
  invalidation, TransportLiveness.Close pinger ownership. M4 exit
  stays `[ ]`.
- v1.7: freeze M4-T5b2 AOI fanout + transport liveness in spec §7.4
  (viewer reverse index, in-memory presentation seam, bounded 1024-event
  pump, 204 bootstrap + readiness barrier, recipient-local ≤10 Hz 205,
  flush-first fanout remove, raw-disconnect reaper with stale retention,
  15 s Ping/Pong, 30 s sweep) (+spec v0.3.23).
- v1.6: split oversized M4-T5b into ingress/lifecycle T5b1
  (bounded 256-command Engine owner mailbox, real 102 decode/routing,
  per-presence rate enforcement with exact 202 mappings, staged world
  entry with Presence activation after the 219 + CompleteEnterWorld
  barrier, WorldExit flush-first leave/takeover) plus AOI
  fanout/heartbeat T5b2 (204/205/206, visibility bootstrap, Ping/Pong,
  stale sweep, disconnect cleanup); freeze semantics in spec §5.2.10 +
  §7.3 (+spec v0.3.22).
- v1.5: split oversized M4-T5 into AOI/presence core T5a
  (gateway-owned registry, 49-cell subscriptions, session-local
  NetEntityIDs, token buckets, heartbeat primitives; no runtime
  wiring) plus gateway↔sim integration T5b (ingress, fanout,
  lifecycle, Ping/Pong); freeze semantics in spec §7.2 (+spec v0.3.21).
- v1.4: freeze T4b persistence semantics in spec §8.3.11–16 (+§5.6.9
  RequireReload): internal/persist composition boundary, deep-copy job
  adapters, dual-sentinel stale mapping, T3c forced-reload bridge,
  exact saver-lag histogram, and real PG crash/ambiguity/shutdown proofs
  (+spec v0.3.20).
- v1.3: split oversized M4-T4 into saver core T4a (Store-independent) plus
  persistence/operations proof T4b (PG/observability/crash); freeze saver
  semantics in spec §8.3 (+spec v0.3.19).
- v1.2: review pass — migration order (catalogs 0002), M1-T6d registry API, M2 opcode split fix, `CollisionWorld` seam, corrected M3/M9 deps, backend-only M11, bounded soaks, (+spec v0.3.5: INTEGER IDs, numeric mob/vendor IDs, `entityEntry.proto`, seed versioning, catalog cache, exact display names).
- v1.1: review pass — working-directory rule, Dockerfile task, M0/M1 dep fix, catalog-table decision (+spec §8.2), fake-vs-real baseline split, AOI task, opcode matrix, task splits (M1/M2/M4/M7/M9/M10), corrected deps, Jala phase-2 note, backend-only M11, bounded soaks, display-name decision.

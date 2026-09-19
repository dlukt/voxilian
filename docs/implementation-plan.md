# Voxilian Backend — Implementation Plan (v1.43)

> Source of truth for WHAT: `docs/backend-spec.md` (v0.3.59).
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
- [x] **M5-T5b1b** Atomic immediate death-entry Store transaction
  (depends on T5b1a): ONE atomic critical Store operation conceptually
  `CommitDeathEntry(ctx, plan)` covering pending-death recovery state,
  character immediate death state, corpse row (generated ID composed
  inside the txn — never fabricated by sim), droppable item
  relocation, PK-drop metadata persistence, advancement immediate
  reset/halve, the existing kills audit row in the same txn when the
  resolved killer fits the existing character/mob identity domain; no
  death/drop ledger row; deterministic
  lock/CAS order (character root first, item roots ascending ItemID);
  replay rejection via the `pending_deaths` PK mapped to
  `ErrDeathAlreadyPending`; stale-revision/crash/commit-ambiguity
  proof per §8.1/§8.3. Spec: §9.5.1, §9.5.8a, §9.5.18.
- [x] **M5-T5b2a** Durable Portal-of-Life state transition (depends on
  T5a + T5b1b): ONE separate atomic critical Store operation
  conceptually `CommitPortalOfLife(ctx, req)` covering ONLY the
  character-root CAS plus the pending-death lowers-only cost update
  (`effective_cost = min(current, proposed)`, `portal_used = TRUE`
  even when the proposal does not lower, incl. cheap cost 0) against
  the durable target corpse ID (`corpse_id` match +
  `portal_used = false`; expired/NULL or different corpse rejected);
  zero ledger rows, zero kill rows; stale/crash/commit-ambiguity
  proof per §8.1/§8.3. Spec: §9.5.1, §9.5.10, §9.5.10a.
- [x] **M5-T5b2b** Exactly-once Underworld-exit penalty consumption
  (depends on T5a + T5b1b + T5b2a): ONE separate atomic critical
  Store operation conceptually `CommitDeathPenalties(ctx, req)`
  taking a COMPLETE already-resolved post-penalty CharacterSnapshot
  plus the raw durable `ExpectedPendingCost`
  (`DeathPenaltyInput.PendingCost`, NOT `DeathPenaltyPlan.ScaledCost`).
  Binding order: validate, Begin, `saveCharacterSnapshotTx` exactly
  ONCE (character root CAS FIRST internally, then complete spell/skill
  replacement), `GetPendingDeathByCharacter`, verify
  `pending.effective_cost == ExpectedPendingCost`,
  `DeletePendingDeathByCharacter` in the SAME transaction, commit
  once; success ALWAYS deletes the pending row (no `ClearPending`
  flag); `corpse_id` set/NULL and `portal_used` false/true are NOT
  rejection conditions; Store runs no T5a mechanics and imports no
  sim; writing ZERO ledger rows and ZERO kills rows; no separate
  ability CAS; penalties-exactly-once, never-skip,
  stale/crash/commit-ambiguity proof per §8.1/§8.3.
  Spec: §9.5.1, §9.5.11–§9.5.14, §9.5.11a.
- [x] **M5-T5c1** Multi-root critical Saver coordination (depends on
  the existing Saver/persistence foundation): the generic
  Store-agnostic `internal/sim` Saver primitive that owns the gates
  of MULTIPLE already-tracked aggregate roots simultaneously while
  ONE caller-supplied callback executes the underlying critical
  transaction. Canonical `AggregateKey.Less` gate order, no
  caller-slice reorder, execution-time revision capture, one
  critical dirty generation per participant, `pending.gen <=
  critical` supersession with newer-pending retention, conservative
  post-invocation error rule (ANY callback error reconcile-blocks
  ALL participants, cause + `ErrSaverReconcileRequired`), no
  blanket block before invocation, `MaxInt64` fail-closed, exact
  `expected+1` result validation, canonical result order, no new
  metric, no PG I/O. NO Store/persist/PG/gateway/proto/death-state/
  recovery-API work (those are T5c2–T5c4). Spec: §8.3.17, §9.5.1a.
- [x] **M5-T5c2a** Materialized death recovery reads + reload
  adapters (depends on T5b1b + T5b2a + T5b2b): Store read-only
  recovery loaders (`LoadDeathCharacterRecovery` /
  `LoadDeathItemRecovery`) returning the COMPLETE death-relevant
  aggregate state (character root + full spells/skills +
  pending-death child; item root + full location + PK-protection
  child) inside ONE `REPEATABLE READ, READ ONLY` transaction each,
  plus `persist` staged `sim.ReloadFunc` adapters reusing the
  existing `ReconcileSaver` bridge. No new migration, query, or
  generated code; no death write adapters. Spec: §9.5.1b, §8.1,
  §8.3.12–§8.3.13.
- [x] **M5-T5c2b** Critical death persistence adapters (depends on
  T5c1 + T5c2a + the existing T5b1b/T5b2a/T5b2b Store
  transactions): the `persist` write adapters executing
  `CommitDeathEntry` / `CommitPortalOfLife` /
  `CommitDeathPenalties` through `sim.Saver.WriteCriticalSet`
  (one-key sets for Portal/DeathPenalties), mapping
  execution-time Saver revisions into Store request
  `ExpectedRevision` fields. Spec: §9.5.1, §9.5.1a, §9.5.1b,
  §9.5.1c, §9.5.8a, §9.5.10a, §9.5.11a.
- [x] **M5-T5c3a** Player runtime identity + typed owner ingress
  (depends on the T4b1/T4b2 value domains it composes; `internal/sim`
  only): durable `CharacterID` association on the live player entity,
  one-live-entity-per-`CharacterID` invariant (live incl. `MIGRATING`),
  typed player initialization, typed concurrent
  `EnqueueAddPlayerEntity` owner command, identity preservation
  through handoff, identity cleanup on entity removal. No death
  state, relocation, persistence, recovery, Saver calls, Store
  types, gateway wiring, or protocol. Spec: §9.5.1d.
- [x] **M5-T5c3b** Death-safe relocation + runtime quiesce/
  reinitialization (depends on T5c3a + T5a + T4b2; `internal/sim`
  only): owner-local primitives for death transitions — resolved
  placement acceptance as `world.Vec3` (no hard-coded Underworld/
  newbie-home coordinates), atomic explicit same-/cross-cell
  relocation under existing ownership-generation rules, movement
  quiescence, rest/health/mana deadline cancellation for death,
  deterministic runtime reinitialization after accepted post-death
  state. No PG/persist/store/gateway/proto work. Spec: §9.5.1d +
  §9.5.1e (frozen T5c3b API + `NewVigor` source correction:
  post-death rest stays absent).
- [x] **M5-T5c3c1** Immediate-death lifecycle gate + attempt
  correlation (depends on T5c3a + T5c3b; `internal/sim` only):
  explicit player life-state gate (`Alive` / `DeathPersisting` /
  `AwaitingRespawn`), one correlated immediate-death persistence
  attempt at a time (per-entity ephemeral epoch + immutable
  `DeathAttemptToken`), owner-local begin-death transition over the
  existing `PlayerQuiesceForDeath` semantics, correlated
  owner-local post-death completion/install wrapper over the
  existing `PlayerInstallPostDeathState` (applied vs
  duplicate-idempotent), ordinary gameplay mutation/input rejection
  while death is in progress or awaiting respawn, retry-safe
  duplicate completion handling. No T5a plan composition, no
  double-death time calculation, no complete Character/Item
  capture, no Store-domain types, no Saver, no `CommitDeathEntry`,
  no PG, no workers/goroutines, no completion ingress, no zero-HP
  automatic dispatch, no gateway/protocol work.
  Spec: §9.5.1, §9.5.1f.
- [x] **M5-T5c3c2** Complete immutable immediate-death capture +
  Store-domain persistence mapping (depends on T5c3c1 + T5a +
  T5c2b): the complete immutable store-independent sim-domain
  capture sufficient to build the complete
  `store.DeathEntryRequest` (complete current character aggregate
  content + complete affected item aggregate content, T5a plans
  mapped onto that captured content, sim-domain → Store-domain
  mapping inside `internal/persist`) without importing `store`
  into `sim`, without re-reading older PG state over newer
   in-memory state, and without exposing raw sim
   `CriticalSetWrite`. Exact capture API deferred to its own
   pre-implementation audit/spec freeze.
   v1.30 note: consumes the corrected five-effect T5a
   `DeathAdvancementPlan` (spec v0.3.46 §9.5.6), including
   `CancelAdvancementTimer`, mapping `ResetGainFlags` to the durable
   `0x70` flag clear with NO active advancement deadline persisted on
   a Normal death and without inventing durable kill-target state.
   Scope and dependencies unchanged.
   Spec: §9.5.1, §9.5.1f.
- [x] **M5-T5c3c3a** Authoritative immediate-death owner
  completion + typed completion ingress (depends on T5c3b +
  T5c3c1 + T5c3c2; `internal/sim` only): one complete
  authoritative immediate-death completion value (token +
  placement + post-death vitals + runtime inputs + post-death
  durable shadow, and nothing else), atomic owner-only
  installation of placement/vitals/runtime/durable with the
  existing `DeathAttemptToken` correlation (Applied vs
  zero-mutation Duplicate, duplicate-before-payload-validation),
  and the typed bounded owner-ingress command for that
  completion on the SAME mailbox with frozen payload ownership.
  No `CommitDeathEntry`, no Saver, no Store, no PG, no recovery,
  no worker, no zero-HP dispatch, no gateway/protocol work.
  Spec: §9.5.1, §9.5.1h.
- [x] **M5-T5c3c3b** Bounded off-owner `CommitDeathEntry` +
  proven materialized recovery executor (depends on T5c3c3a +
  T5c2a + T5c2b; `internal/persist` + only the narrow sim
  support required by the already-frozen
  reconciliation/revision APIs; frozen v0.3.49 in §9.5.1h):
  bounded off-owner execution (no goroutine-per-death) using
  the existing `persist.CommitDeathEntry` with exact callback
  expected-revision observation, explicit T5c2a character +
  affected-item recovery on stale/ambiguous results (no blind
  replay), reconciliation of every Saver participant,
  conservative exact-`expected+1` plus semantic content proof
  before any completion, construction of the authoritative
  c3c3a completion value, and bounded retry/redelivery of the
  typed owner completion when owner ingress is temporarily
  unavailable (`ErrSimIngressFull` redelivery; engine-stop and
  mismatch errors terminal; unproven recovery returns the
  stable `ErrDeathCommitUnproven` sentinel fail-closed). A
  persistence worker MUST NEVER mutate a live sim entity; live
  entity replacement occurs ONLY through the typed c3c3a owner
  completion. T5c3c3c MUST NOT later use naive
  TrySubmit-after-begin semantics (separate reservation freeze).
  Spec: §9.5.1, §9.5.1h.
- [x] **M5-T5c3c3c1** Guaranteed bounded executor reservation
  + prepared activation seam (depends on T5c3c3b; `internal/persist`
  + one narrow store-independent interface in `internal/sim`):
  reservation of one bounded executor queue-capacity slot,
  the reservation state machine, prepare/freeze/map work
  BEFORE owner lifecycle mutation, activation that cannot
  fail with queue-full, cancellation/release, exact
  queue-permit accounting, preserving existing `TrySubmit`
  behavior. No `PlayerLifeState` mutation, no zero-HP
  routing, no double-death timestamps, no `Killed`
  sequencing, no T5a composition, no `DeathContext`
  resolution, no item/drop policy resolution, no Token
  handling, no placement selection, no gateway/protocol.
  Spec: §9.5.1, §9.5.1i.
- [x] **M5-T5c3c3c2** Zero-HP resolved T5a orchestration +
  double-death runtime gate (depends on T5c3c3a + T5c3c3b +
  T5c3c3c1 + T5a + T5c3c2): zero-HP `Killed` orchestration,
  source-order double-death runtime gate, ephemeral
  last-death-seconds state, resolved whole-second death-time
  input, Avoided / Cheap / Normal routing, resolved T5a
  planning, actual Token ItemID + restored rest threshold,
  complete per-item resolved drop/content facts,
  newbie-home vs Underworld resolved placement,
  construction of `ImmediateDeathCapture`, owner transition
  to `DeathPersisting` ONLY after reservation work has
  successfully prepared, activation of that already-prepared
   reserved work. No gateway/protocol. Exact gameplay API
   frozen in spec §9.5.1j (v0.3.51 Phase-A source freeze).
   Spec: §9.5.1, §9.5.1i, §9.5.1j.
- [x] **M5-T5c3d1** Authoritative pending-death owner state +
  immediate-completion handoff + respawn-release primitive
  (depends on T5c3c3a + T5c3c3b + T5c3c3c2 + T5c2a;
  `internal/sim` + the narrow `internal/persist` recovery
  mapper): store-independent `PendingDeathRuntime` value,
  owner-local pending state on the player entity,
  immediate-death completion carrying authoritative pending
  state (normal-success construction from `DeathEntryResult`,
  proven-lost-ack construction from the recovered pending
  snapshot), atomic owner install, pending inspection,
  authoritative recovery/hydration seam, owner-local
  respawn-release transition `AwaitingRespawn -> Alive`
  (NOT Underworld `LeaveHold`, NOT penalty consumption).
  No Portal calculation/commit, no Portal async worker, no
  Underworld `LeaveHold`, no `PlanDeathPenalties` /
  `CommitDeathPenalties`, no penalty RNG, no pending
  deletion, no gateway opcode 120, no 214 / 215 transport.
  Spec: §9.5.1, §9.5.1k.
- [x] **M5-T5c3d2a** Portal-of-Life owner attempt +
  immutable current-state capture + typed owner
  completion/abort (depends on T5c3d1 + T5a;
  `internal/sim` only): resolved
  `PortalOfLifeResolvedInput`, per-entity Portal-attempt
  epoch + `PortalAttemptToken`, `PlanPortalOfLife` +
  `ReducePendingDeathCost` composition, immutable
  complete current character capture, sim-domain Portal
  work-reservation interface, Prepare-before-attempt
  ordering, typed success completion (Applied vs
  duplicate-before-payload-validation) + definitive
  abort, same-mailbox typed completion/abort ingress,
  hydration serialization. No Store/PG/Saver/persist.
  Spec: §9.5.1, §9.5.1k.
- [x] **M5-T5c3d2b1** Reserved Saver critical slot /
  generation seam for active-player Portal persistence
  (depends on T5c1 + T5c3d2a; `internal/sim` only): one
  generic Store-independent Saver primitive reserving the
  existing per-key Saver gate(s) + the critical dirty
  generation(s) at Reserve time, executing later with
  execution-time expected revisions + normal CriticalSet
  success/error semantics. No Store, no persist, no Portal
  executor, no PG, no recovery.
  Spec: §9.5.1, §9.5.1k.
- [x] **M5-T5c3d2b2** Bounded off-owner Portal
  executor + Store mapping + in-critical-callback
  lost-ack proof (depends on T5c3d2a + T5c3d2b1 + T5b2a
  + T5c2a + T5c2b): Portal Store mapper, bounded Portal
  executor, executor queue reservation, concrete
  PortalOfLifeWorkReservation, Prepare-time Saver
  critical reservation, CommitPortalOfLife, in-callback
  materialized proof, normal/proven-lost-ack completion,
  definite-pre-execution abort, bounded same-mailbox
  redelivery. No Underworld penalties. Freezes the
  in-critical-callback recovery rule (no
  `ReconcileSaver` / `ResolveReconciled` after a proven
  Portal lost-ack).
  Spec: §9.5.1, §9.5.1k.
- [x] **M5-T5c3d3a** Authoritative Underworld-exit penalty
  owner attempt + gameplay quiesce + frozen retryable
  post-penalty capture + typed owner
  completion/retryable transition (depends on T5c3d1
  + T5c3d2a + T5c3d2b2 + T5a; `internal/sim` only):
  actual authoritative Underworld `LeaveHold` penalty
  event, narrowly resolved penalty inputs
  (`DefaultDeathCost` + `FrenzyActive`; every other
  input derived from the live entity), source flag
  interpretation (`MURDERER 0x000002`, `OUTLAW
  0x000008`, `HAUNTED 0x000100`, `TUTORIAL 0x000800`),
  exactly one `PlanDeathPenalties` call with
  deterministic owner RNG per live attempt, exact
  complete post-penalty `PlayerVitals` +
  `PlayerDurableState`, new appended
  `PlayerLifeDeathPenaltyPersisting` gameplay-quiesce
  life state, frozen retryable `DeathPenaltyCapture`
  (post-penalty state + pre-consumption
  `PendingBefore`), penalty attempt token/epoch,
   Store-independent provider/reservation interfaces
   with Reserve-before-RNG ordering, epoch++/attempt/
   life-lock BEFORE Prepare then Activate (corrected
   v1.41; supersedes Prepare-while-Alive),
   lock-on-Prepare/Activate-failure, exact-plan retry without
   RNG, definitive pre-Store retryable notification,
   typed same-mailbox completion/retryable ingress,
   owner-only final apply (exact Vitals+Durable install
   plus owner-local `reconcileHealth`, then pending clear;
   corrected v1.41), Portal /
  hydration / respawn-release serialization, `Step`
  quiesce. No Store, no PG, no Saver implementation,
  no queue/worker, no `CommitDeathPenalties`, no
  recovery, no lost-ack proof, no gateway/proto/session.
  C→S 120 `respawn_ack` is NOT the synonym for
  Underworld `LeaveHold` /
  `ApplyDeathPenalties` (T5c4 owns opcode 120 transport;
  T5c3d3a owns "player actually leaves the Underworld").
  Spec: §9.5.1, §9.5.1k.
- [x] **M5-T5c3d3b** Bounded penalty persistence executor +
  Store mapping + in-critical-callback
  pending-deletion/lost-ack proof (depends on T5c3d3a
  + T5c3d2b1 + T5b2b + T5c2a + T5c2b): concrete bounded
  `PenaltyExecutor` implementing
  `DeathPenaltyWorkProvider` (Reserve owns queue permit
  + Saver critical slot before owner RNG), CPU-only
  Prepare, Store mapper (`ExpectedPendingCost =
  PendingBefore.EffectiveCost`), exactly one
  `CommitDeathPenalties` in one reserved `Execute`
  callback, in-callback materialized proof
  (`E+1` + content + `Pending == nil`), proven-lost-ack
  success vs `ErrDeathPenaltyCommitUnproven`
  fail-closed, pre-Store-only retryable notification,
  owner success completion/redelivery. No gateway.
  Spec: §9.5.1, §9.5.1k.
- [ ] **M5-T5c4** Gateway death wire/state integration + reconnect E2E
  (depends on T5c3a + T5c3b + T5c3c1 + T5c3c2 + T5c3c3a + T5c3c3b + T5c3c3c1 + T5c3c3c2 + T5c3d1 + T5c3d2a + T5c3d2b1 + T5c3d2b2 + T5c3d3a + T5c3d3b + the existing M4
  gateway/presence/fanout foundation): gateway/state-machine routing, rate-gated C→S 120
  handling, critical S→C 214 / 215 delivery,
  session/Presence/NetEntityID composition, reconnect/end-to-end
  proof reusing the existing 120/214/215 codecs (no second
  protocol). Spec: §9.5.1, §9.5.1a, §9.5.1l (frozen v0.3.58 T5c4
  contract: authoritative 214/215 boundaries, recipient-local
  handles, ordering, teleport AOI reconcile, 120 correlation,
  typed release + atomic recovery ingress, error/disconnect/
  takeover semantics, `Alive` reconnect bootstrap with atomic
  pending hydration, narrow persist bootstrap adapter,
  `TryCritical` fail-closed; 120 != LeaveHold; proto unchanged)
  as corrected by §9.5.1m (frozen v0.3.59 reconnect bootstrap
  completeness: COMPLETE authoritative bootstrap incl. exact
  `Durable.Items` for directly character-owned `kind = 0`
  inventory in ascending item-id order; dedicated read-only
  `LoadPlayerBootstrapRecovery` in one `REPEATABLE READ` /
  `READ ONLY` transaction; exactly one minimal read-only
  `ListCharacterInventoryItems` query, no migration;
  `effective = bound(base + 0, 1, 70)` from the real durable
  base stats, powers `0`, multiplier `1`, no magic constants;
  all transport rules preserved). Reverted to `[ ]` by v1.43:
  T5c4 must not be marked complete while the reconnect
  implementation violates its own authoritative-state contract.
- [ ] **M5-T6** Personal/world-light intents: `115 rest`, `116 eat` (hunger/vigor effects), `105 use` (skill/item dispatch incl. Second Wind), `119 safety_toggle`, `117/118 → 209` chat (+channel rules, length caps, rate limits). Owner of these opcodes: this task, no other. Spec: §6.3, §9.
- [ ] **M5-T7** Authoritative attack/cast runtime integration (depends on
  M5-T1, M5-T2, M5-T3a, M5-T3b, M5-T4a,   M5-T4b1, M5-T4b2,
  M5-T5-complete (T5a+T5b1a+T5b1b+T5b2a+T5b2b+T5c1+T5c2a+T5c2b+T5c3a+T5c3b+T5c3c1+T5c3c2+T5c3c3a+T5c3c3b+T5c3c3c1+T5c3c3c2+T5c3d1+T5c3d2a+T5c3d2b1+T5c3d2b2+T5c3d3a+T5c3d3b+T5c4), M5-T6). Owns real C→S 103
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
| M5-T5b2a | M5-T5a, M5-T5b1b | `backend/voxilian/{queries,internal/store}` (durable Portal lowers-only transition; no new migration) |
| M5-T5b2b | M5-T5a, M5-T5b1b, M5-T5b2a | `backend/voxilian/{queries,internal/store,internal/persist}` (durable delayed penalties; no second migration unless audit requires) |
| M5-T5c1 | existing Saver/persistence foundation | `backend/voxilian/internal/sim` (generic multi-root critical Saver coordination only: no Store/persist/PG/gateway/proto/death-state/recovery work) |
| M5-T5c2a | M5-T5b1b, M5-T5b2a, M5-T5b2b | `backend/voxilian/internal/store` (death recovery reads) + `internal/persist` (staged reload adapters; no new SQL/migration/generated code) |
| M5-T5c2b | M5-T5c1, M5-T5c2a + existing M5-T5b1b/T5b2a/T5b2b Store transactions | `backend/voxilian/internal/persist` (critical death write adapters via `Saver.WriteCriticalSet`) |
| M5-T5c3a | T4b1/T4b2 value domains (no Store/persist/gateway work) | `backend/voxilian/internal/sim` (player runtime identity + typed owner ingress) |
| M5-T5c3b | M5-T5c3a, M5-T5a, M5-T4b2 | `backend/voxilian/internal/sim` (death-safe relocation + runtime quiesce/reinitialization; no PG/persist/store/gateway/proto) |
| M5-T5c3c1 | M5-T5c3a, M5-T5c3b | `backend/voxilian/internal/sim` (immediate-death lifecycle gate + attempt correlation; no Store/persist/PG/gateway/proto/T5a-composition work) |
| M5-T5c3c2 | M5-T5c3c1, M5-T5a, M5-T5c2b | `backend/voxilian/internal/sim` (store-independent capture) + `internal/persist` (Store-domain mapping; no new migration unless audit requires) |
| M5-T5c3c3a | M5-T5c3b, M5-T5c3c1, M5-T5c3c2 | `backend/voxilian/internal/sim` (authoritative immediate-death owner completion + typed completion ingress; no Store/persist/PG/Saver/worker/zero-HP/gateway work) |
| M5-T5c3c3b | M5-T5c3c3a, M5-T5c2a, M5-T5c2b | `backend/voxilian/internal/persist` + narrow sim reconciliation/revision support (bounded off-owner `CommitDeathEntry` execution, T5c2a recovery, no blind replay, no live-entity mutation) |
| M5-T5c3c3c1 | M5-T5c3c3b | `backend/voxilian/internal/persist` (guaranteed bounded executor reservation + prepared activation seam) + one narrow store-independent interface in `backend/voxilian/internal/sim` (no `PlayerLifeState` mutation, no zero-HP/T5a/double-death/placement/gateway work) |
| M5-T5c3c3c2 | M5-T5c3c3a, M5-T5c3c3b, M5-T5c3c3c1, M5-T5a, M5-T5c3c2 | `backend/voxilian/internal/sim` (zero-HP orchestration, double-death gate, resolved T5a/placement composition over the c3c3c1 reservation; no gateway work) |
| M5-T5c3d1 | M5-T5c3c3a, M5-T5c3c3b, M5-T5c3c3c2, M5-T5c2a | `backend/voxilian/internal/sim` (authoritative pending-death owner state, extended immediate-death completion, respawn-release primitive) + narrow `backend/voxilian/internal/persist` recovery mapper (no Portal/penalty/gateway work) |
| M5-T5c3d2a | M5-T5c3d1, M5-T5a | `backend/voxilian/internal/sim` (Portal-of-Life owner attempt, immutable current-state capture, typed owner completion/abort; no Store/persist/PG/Saver/worker work) |
| M5-T5c3d2b1 | M5-T5c1, M5-T5c3d2a | `backend/voxilian/internal/sim` (reserved Saver critical slot / generation seam only: existing-gate + critical-generation reservation, execution-time revisions; no Store/persist/PG/Portal-executor/recovery work) |
| M5-T5c3d2b2 | M5-T5c3d2a, M5-T5c3d2b1, M5-T5b2a, M5-T5c2a, M5-T5c2b | `backend/voxilian/internal/persist` + Store-domain Portal mapper (bounded off-owner `CommitPortalOfLife`, in-critical-callback lost-ack proof; no Underworld-exit penalties) |
| M5-T5c3d3a | M5-T5c3d1, M5-T5c3d2a, M5-T5c3d2b2, M5-T5a | `backend/voxilian/internal/sim` (authoritative Underworld-exit penalty owner attempt + gameplay quiesce + frozen retryable capture + typed completion/retryable; no Store/persist/PG/Saver/queue/worker/gateway work) |
| M5-T5c3d3b | M5-T5c3d3a, M5-T5c3d2b1, M5-T5b2b, M5-T5c2a, M5-T5c2b | `backend/voxilian/internal/persist` + Store-domain penalty mapper (bounded off-owner `CommitDeathPenalties` execution, in-critical-callback lost-ack proof; no gateway work) |
| M5-T5c4 | M5-T5c3a, M5-T5c3b, M5-T5c3c1, M5-T5c3c2, M5-T5c3c3a, M5-T5c3c3b, M5-T5c3c3c1, M5-T5c3c3c2, M5-T5c3d1, M5-T5c3d2a, M5-T5c3d2b1, M5-T5c3d2b2, M5-T5c3d3a, M5-T5c3d3b + existing M4 gateway/presence/fanout foundation | `backend/voxilian/internal/{sim,gateway}` (death wire/state integration + reconnect E2E, existing 120/214/215 codecs) |
| M5-T7 | M5-T1, M5-T2, M5-T3a, M5-T3b, M5-T4a, M5-T4b1, M5-T4b2, M5-T5a, M5-T5b1a, M5-T5b1b, M5-T5b2a, M5-T5b2b, M5-T5c1, M5-T5c2a, M5-T5c2b, M5-T5c3a, M5-T5c3b, M5-T5c3c1, M5-T5c3c2, M5-T5c3c3a, M5-T5c3c3b, M5-T5c3c3c1, M5-T5c3c3c2, M5-T5c3d1, M5-T5c3d2a, M5-T5c3d2b1, M5-T5c3d2b2, M5-T5c3d3a, M5-T5c3d3b, M5-T5c4, M5-T6 | `backend/voxilian/internal/{sim,gateway}` (authoritative 103/104 runtime integration) |
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
| 120 respawn_ack | M5-T5c4 | death pipeline live integration |
| 121–123 char CRUD, 126 leave | M3-T3b | lifecycle |
| 124 enter_world | M3-T4a baseline, M3-T4b takeover | lifecycle |
| 125 ack | M3-T5b | flow control |

## Plan history

- v1.43: correct M5-T5c4 reconnect bootstrap completeness
  (docs only, spec v0.3.58 -> v0.3.59 new §9.5.1m; no
  split, M5-T5 stays TWENTY-THREE tasks): the v0.3.58
  adapter mapped character/spells/skills/pending but
  installed `Durable.Items == nil` despite PG-owned
  inventory (no later hydration step), and resolved
  runtime inputs from hard-coded `10`/`10` instead of
  the durable base stats. Frozen corrections: COMPLETE
  authoritative bootstrap incl. the exact authoritative
  inventory item set; inventory membership is exactly
  directly character-owned `kind = 0` rows in ascending
  item-id order (ground/corpse/vault/contained excluded);
  dedicated read-only `LoadPlayerBootstrapRecovery` in
  ONE `REPEATABLE READ` / `READ ONLY` transaction
  (`LoadDeathCharacterRecovery` NOT widened); exactly
  one minimal read-only `ListCharacterInventoryItems`
  query authorized, no migration; `effective =
  bound(base + 0, 1, 70)` from the real durable base
  stats, powers `0`, multiplier `1`, no magic constants;
  all sixteen §9.5.1l transport boundaries preserved.
  T5c4 row spec pointer gains §9.5.1m; T5c4 reverted
  `[x]` -> `[ ]`. Checkbox state otherwise unchanged:
  T5a/T5b1a/T5b1b/T5b2a/T5b2b/T5c1/T5c2a/T5c2b/T5c3a/
  T5c3b/T5c3c1/T5c3c2/T5c3c3a/T5c3c3b/T5c3c3c1/T5c3c3c2/
  T5c3d1/T5c3d2a/T5c3d2b1/T5c3d2b2/T5c3d3a/T5c3d3b `[x]`,
  T5c4/T6/T7 and M5 exit `[ ]`.

- v1.42: freeze M5-T5c4 death wire/state integration +
  reconnect contract (docs only, spec v0.3.57 -> v0.3.58
  new §9.5.1l; no split, M5-T5 stays TWENTY-THREE tasks):
  sixteen frozen boundaries (214 at owner `DeathPersisting`
  via a narrow non-blocking presentation seam; 214 to
  ready viewers with recipient-local handles; 215 only
  after accepted owner completion via `WirePosition`;
  critical-FIFO 214-before-215; teleport AOI reconcile via
  existing fanout + one narrow relocation control;
  gateway-ephemeral 120 correlation on 215 admission;
  same-mailbox `EnqueuePlayerReleaseRespawn`; 120 error
  mapping; disconnect/takeover isolation; `Alive`
  reconnect bootstrap; narrow persist bootstrap adapter
  over `LoadDeathCharacterRecovery`; atomic
  `EnqueueAddPlayerEntityWithRecovery`; materialized-only
  crash recovery; `TryCritical` fail-closed; proto
  unchanged) with 120 != LeaveHold != ApplyDeathPenalties
  binding. T5c4 row spec pointer gains §9.5.1l; T5c4
  still `[ ]`. Checkbox state unchanged:
  T5a/T5b1a/T5b1b/T5b2a/T5b2b/T5c1/T5c2a/T5c2b/T5c3a/
  T5c3b/T5c3c1/T5c3c2/T5c3c3a/T5c3c3b/T5c3c3c1/T5c3c3c2/
  T5c3d1/T5c3d2a/T5c3d2b1/T5c3d2b2/T5c3d3a/T5c3d3b `[x]`,
  T5c4/T6/T7 and M5 exit `[ ]`.

- v1.41: correct M5-T5c3d3a penalty retry and health semantics (docs
  only, spec v0.3.56 -> v0.3.57 extends §9.5.1k; no split, M5-T5 stays
  TWENTY-THREE tasks): freeze the anti-reroll rule (no post-RNG
  fallible preparation/publication failure returns the player to an
  ordinary Alive state for replanning/reroll), correct the canonical
  first-attempt order to validate / Reserve (queue permit + Saver
  critical slot before RNG) / exactly one `PlanDeathPenalties` /
  freeze the exact capture / consume epoch + install the private
  attempt + lock to `PlayerLifeDeathPenaltyPersisting` BEFORE
  Prepare / Activate (superseding Prepare-while-Alive;
  Prepare-failure-Alive removed), Prepare/Activate failure after
  install keeps the lock/epoch/exact capture/plan/pending with
  `persistenceActive == false`, retry reuses the exact
  capture/token/plan with zero RNG, and freeze completion health
  semantics (install exact stored Vitals + Durable, run the existing
  owner-local `reconcileHealth` at the current tick, then clear
  pending/attempt to Alive; no `commitVitals`, no observer event, no
  mana/rest reconciliation; mana/rest slots bit-identical). Correct
  `meridian59.md` Justice summary + frozen flag values
  (`PFLAG_MURDERER 0x000002`, `PFLAG_OUTLAW 0x000008`,
  `PFLAG_HAUNTED 0x000100`, `PFLAG_PKILL_ENABLE 0x000400`,
  `PFLAG_TUTORIAL 0x000800`; no durable `PFLAG_REVENANT`;
  "revenants" = clearing `PFLAG_HAUNTED`; Outlaw cleared on the
  full-cost branch, Haunted on Frenzy/Chaos or the full-cost
  branch). d3b recovery contract unchanged; T5c4 ownership unchanged.
  Checkbox state unchanged: T5a/T5b1a/T5b1b/T5b2a/T5b2b/
  T5c1/T5c2a/T5c2b/T5c3a/T5c3b/T5c3c1/T5c3c2/T5c3c3a/
  T5c3c3b/T5c3c3c1/T5c3c3c2/T5c3d1/T5c3d2a/T5c3d2b1/
  T5c3d2b2/T5c3d3a `[x]`,
  T5c3d3b/T5c4/T6/T7 and M5 exit `[ ]`.

- v1.40: split M5-T5c3d3 into owner-first T5c3d3a +
  persistence-later T5c3d3b (docs only, spec
  v0.3.55 -> v0.3.56 extends §9.5.1k + §9.5.1; split only):
  T5c3d3a (authoritative Underworld-exit penalty owner
  attempt + gameplay quiesce + frozen retryable
  post-penalty capture + typed owner
  completion/retryable transition; depends on T5c3d1
  + T5c3d2a + T5c3d2b2 + T5a; `internal/sim` only;
  owns the `LeaveHold` owner event, narrow resolved
  inputs, source flag interpretation
  (MURDERER/OUTLAW/HAUNTED/TUTORIAL bits), exactly one
  `PlanDeathPenalties` per live attempt, complete
  post-penalty state, `PlayerLifeDeathPenaltyPersisting`
  quiesce, frozen retryable capture, token/epoch,
  Store-independent provider/reservation with
  Reserve-before-RNG, Prepare-while-Alive ordering,
  lock-on-activation-failure, exact-plan retry without
  RNG, pre-Store retryable notification, same-mailbox
  typed completion/retryable ingress, owner-only final
  apply; no Store/PG/Saver/queue/worker/gateway) +
  T5c3d3b (bounded penalty persistence executor +
  Store mapping + in-critical-callback
  pending-deletion/lost-ack proof; depends on T5c3d3a
  + T5c3d2b1 + T5b2b + T5c2a + T5c2b; freezes the
  binding future rule that concrete Reserve owns the
  queue permit + Saver critical slot before owner RNG
  rolls, Prepare is CPU-only, the mapper uses
  `PendingBefore.EffectiveCost`, and the proven
  lost-ack path returns E+1 inside the held-gate
  callback with no reconcile/resolve/replay; no
  gateway); split rationale frozen (full post-penalty
  Character persistence requires gameplay quiesce;
  infrastructure retry must not reroll penalty RNG);
  T5c3d3a now depends on
  T5c3d1+T5c3d2a+T5c3d2b2+T5a; T5c3d3b now depends on
  T5c3d3a+T5c3d2b1+T5b2b+T5c2a+T5c2b; T5c4 now depends on
  T5c3a+T5c3b+T5c3c1+T5c3c2+T5c3c3a+T5c3c3b+T5c3c3c1+
  T5c3c3c2+T5c3d1+T5c3d2a+T5c3d2b1+T5c3d2b2+T5c3d3a+T5c3d3b
  + the M4 gateway/presence/fanout foundation; M5-T7 now depends
  on the TWENTY-THREE-task M5-T5 set
  (T5a+T5b1a+T5b1b+T5b2a+T5b2b+T5c1+T5c2a+T5c2b+T5c3a+T5c3b+
  T5c3c1+T5c3c2+T5c3c3a+T5c3c3b+T5c3c3c1+T5c3c3c2+T5c3d1+
  T5c3d2a+T5c3d2b1+T5c3d2b2+T5c3d3a+T5c3d3b+T5c4); task-index rows updated
  accordingly. `meridian59.md` untouched.
  Checkbox state unchanged: T5a/T5b1a/T5b1b/T5b2a/T5b2b/
  T5c1/T5c2a/T5c2b/T5c3a/T5c3b/T5c3c1/T5c3c2/T5c3c3a/
  T5c3c3b/T5c3c3c1/T5c3c3c2/T5c3d1/T5c3d2a/T5c3d2b1/
  T5c3d2b2 `[x]`,
  T5c3d3a/T5c3d3b/T5c4/T6/T7 and M5 exit `[ ]`.

- v1.39: freeze M5 Portal persistence execution contract
  (docs only, spec v0.3.54 -> v0.3.55 extends §9.5.1k +
  §9.5.1; no split, M5-T5 stays TWENTY-TWO tasks):
  T5c3d2b2 (bounded off-owner Portal executor + Store
  mapping + in-critical-callback lost-ack proof; depends
  on T5c3d2a + T5c3d2b1 + T5b2a + T5c2a + T5c2b) execution
  contract — narrow d2a activation refinement
  (`ActivatePortalOfLifeWork() error`; queue-full
  impossible after Prepare; definitive pre-publication
  error rolls back in the same owner turn with
  `portalInFlight`/attempt cleared, pending/gameplay
  unchanged, and the incremented `portalEpoch` kept
  consumed; no typed abort for that path), bounded
  Portal executor lifecycle (fixed workers, bounded
  queue, one-shot `Run`, non-blocking reservation,
  DeathExecutor shutdown semantics), stable
  persist-domain sentinels, narrow
  `PortalExecutionStore` + typed owner-sink seams,
  Prepare ownership (queue permit + frozen work + d2b1
  Saver critical slot before `portalEpoch++`), Store
  mapper + request freeze, exactly-one reserved
  `Execute` with at-most-once Store, in-callback
  read-only lost-ack proof (`E+1` + content + pending
  proof, nil `CorpseID` valid; proven path returns
  `E+1` preserving newer `MarkDirty`, no
  `ReconcileSaver`/`ResolveReconciled`/replay; unproven
  path is `ErrPortalCommitUnproven` fail-closed and
  never owner-aborts once Store was invoked),
  definite-pre-execution abort + bounded same-mailbox
  redelivery, and the unchanged public T5c2b
  `CommitPortalOfLife` adapter. No Underworld-exit
  penalties; no Store production change; no SQL/query/
  migration/generated change. `meridian59.md` untouched.
  Checkbox state unchanged: T5a/T5b1a/T5b1b/T5b2a/T5b2b/
  T5c1/T5c2a/T5c2b/T5c3a/T5c3b/T5c3c1/T5c3c2/T5c3c3a/
  T5c3c3b/T5c3c3c1/T5c3c3c2/T5c3d1/T5c3d2a/T5c3d2b1 `[x]`,
  T5c3d2b2/T5c3d3/T5c4/T6/T7 and M5 exit `[ ]`.

- v1.38: split M5-T5c3d2b into reservation-first
  T5c3d2b1 + executor-later T5c3d2b2 (docs only, spec
  v0.3.53 -> v0.3.54 extended §9.5.1k + §9.5.1 + §8.3.17
  pointer; split only): T5c3d2b1 (reserved Saver
  critical slot / generation seam for active-player
  Portal persistence; depends on T5c1 + T5c3d2a;
  `internal/sim` only; owns the generic
  Store-independent reservation — existing per-key
  Saver gates + critical dirty generations reserved
  non-blockingly at Reserve time, later Execute with
  execution-time expected revisions + normal CriticalSet
  success/error semantics with newer-pending retention;
  no Store/persist/Portal-executor/PG/recovery; no d2a
  owner behavior change) + T5c3d2b2 (bounded off-owner
  Portal executor + Store mapping +
  in-critical-callback lost-ack proof; depends on
  T5c3d2a + T5c3d2b1 + T5b2a + T5c2a + T5c2b; freezes
  the future binding rule that Prepare owns the queue
  permit + frozen work + reserved Saver slot holding
  the existing Character gate before
  `portalEpoch++`/`portalInFlight`/Activate, the
  non-blocking Prepare-fails-retry rule, and the
  refined proven-lost-ack rule returning E+1 inside the
  held-gate callback while clearing only `gen <=
  reservedGeneration`); readiness correction frozen
  (generation reservation alone is insufficient — the
  Saver gate MUST also be held from Prepare, else a
  later write overtakes the old Portal capture);
  T5c3d3 now depends on
  T5c3d1+T5c3d2a+T5c3d2b1+T5c3d2b2+T5a+T5b2b+T5c2a+
  T5c2b; T5c4 now depends on
  T5c3a+T5c3b+T5c3c1+T5c3c2+T5c3c3a+T5c3c3b+T5c3c3c1+
  T5c3c3c2+T5c3d1+T5c3d2a+T5c3d2b1+T5c3d2b2+T5c3d3 + the M4
  gateway/presence/fanout foundation; M5-T7 now depends
  on the TWENTY-TWO-task M5-T5 set
  (T5a+T5b1a+T5b1b+T5b2a+T5b2b+T5c1+T5c2a+T5c2b+T5c3a+T5c3b+
  T5c3c1+T5c3c2+T5c3c3a+T5c3c3b+T5c3c3c1+T5c3c3c2+T5c3d1+
  T5c3d2a+T5c3d2b1+T5c3d2b2+T5c3d3+T5c4); task-index rows updated
  accordingly. `meridian59.md` untouched.
  Checkbox state unchanged: T5a/T5b1a/T5b1b/T5b2a/T5b2b/
  T5c1/T5c2a/T5c2b/T5c3a/T5c3b/T5c3c1/T5c3c2/T5c3c3a/
  T5c3c3b/T5c3c3c1/T5c3c3c2/T5c3d1/T5c3d2a `[x]`,
  T5c3d2b1/T5c3d2b2/T5c3d3/T5c4/T6/T7 and M5 exit `[ ]`.

- v1.37: split monolithic M5-T5c3d2 into owner-first
  T5c3d2a + persistence-later T5c3d2b (docs only, spec
  v0.3.52 -> v0.3.53 extended §9.5.1k; split only):
  T5c3d2a (Portal-of-Life owner attempt + immutable
  current-state capture + typed owner completion/abort;
  depends on T5c3d1 + T5a; `internal/sim` only; owns
  resolved input, Portal-attempt epoch/token,
  PlanPortalOfLife + ReducePendingDeathCost composition,
  complete current capture, sim-domain work-reservation
  interface, Prepare-before-attempt ordering, typed
  success completion/abort, same-mailbox ingress,
  hydration guard; no Store/PG/Saver/persist) +
  T5c3d2b (bounded off-owner Portal persistence +
  in-critical-callback lost-ack proof; depends on
  T5c3d2a + T5b2a + T5c2a + T5c2b; freezes the binding
  future rule that a proven Portal lost-ack MUST NOT
  use ReconcileSaver/ResolveReconciled but instead
  prove commit inside the CriticalSet callback and
  return success revision E+1 preserving newer
  snapshots; no Underworld-exit penalties); T5c3d3 now
  depends on T5c3d1+T5c3d2a+T5c3d2b+T5a+T5b2b+T5c2a+
  T5c2b (with the frozen note that LeaveHold MUST NOT
  consume pending penalties while a Portal attempt
  remains in flight); T5c4 now depends on
  T5c3a+T5c3b+T5c3c1+T5c3c2+T5c3c3a+T5c3c3b+T5c3c3c1+
  T5c3c3c2+T5c3d1+T5c3d2a+T5c3d2b+T5c3d3 + the M4
  gateway/presence/fanout foundation; M5-T7 now depends
  on the TWENTY-ONE-task M5-T5 set
  (T5a+T5b1a+T5b1b+T5b2a+T5b2b+T5c1+T5c2a+T5c2b+T5c3a+T5c3b+
  T5c3c1+T5c3c2+T5c3c3a+T5c3c3b+T5c3c3c1+T5c3c3c2+T5c3d1+
  T5c3d2a+T5c3d2b+T5c3d3+T5c4); task-index rows updated
  accordingly. `meridian59.md` untouched (source audit
  re-verified: corpse-once, whole-second age, age<60
  bonus branch, age/10-6 penalty, 5..80 bound,
  lowers-only SetDeathCost, used-even-without-decrease).
  Checkbox state unchanged: T5a/T5b1a/T5b1b/T5b2a/T5b2b/
  T5c1/T5c2a/T5c2b/T5c3a/T5c3b/T5c3c1/T5c3c2/T5c3c3a/
  T5c3c3b/T5c3c3c1/T5c3c3c2/T5c3d1 `[x]`,
  T5c3d2a/T5c3d2b/T5c3d3/T5c4/T6/T7 and M5 exit `[ ]`.

- v1.36: split M5-T5c3d into owner-first T5c3d1 +
  Portal-later T5c3d2 + Underworld-exit-last T5c3d3 (docs
  only, spec v0.3.51 -> v0.3.52 new §9.5.1k; split only):
  T5c3d1 (authoritative pending-death owner state +
  immediate-completion handoff + respawn-release
  primitive; depends on T5c3c3a + T5c3c3b + T5c3c3c2 +
  T5c2a; `internal/sim` + the narrow `internal/persist`
  recovery mapper; owns the store-independent
  `PendingDeathRuntime` value, owner-local pending
  state, extended immediate-death completion with
  normal-success construction from `DeathEntryResult`
  and proven-lost-ack construction from the recovered
  pending snapshot, atomic owner install, pending
  inspection, authoritative recovery/hydration seam,
  and the owner-local `AwaitingRespawn -> Alive`
  respawn-release transition that is NOT Underworld
  `LeaveHold`/penalty consumption; no Portal
  calculation/commit, no Portal async worker, no
  Underworld `LeaveHold`, no `PlanDeathPenalties` /
  `CommitDeathPenalties`, no penalty RNG, no pending
  deletion, no gateway opcode 120, no 214 / 215
  transport) + T5c3d2 (Portal-of-Life async runtime
  transition; depends on T5c3d1 + T5a + T5b2a + T5c2a +
  T5c2b; no Underworld-exit penalties) + T5c3d3
  (authoritative Underworld-exit penalty transition +
  exactly-once pending consumption; depends on T5c3d1 +
  T5c3d2 + T5a + T5b2b + T5c2a + T5c2b; C→S 120 remains
  NOT synonymous with `LeaveHold`); T5c4 now depends on
  T5c3a+T5c3b+T5c3c1+T5c3c2+T5c3c3a+T5c3c3b+T5c3c3c1+
  T5c3c3c2+T5c3d1+T5c3d2+T5c3d3 + the M4
  gateway/presence/fanout foundation; M5-T7 now depends
  on the TWENTY-task M5-T5 set
  (T5a+T5b1a+T5b1b+T5b2a+T5b2b+T5c1+T5c2a+T5c2b+T5c3a+T5c3b+
  T5c3c1+T5c3c2+T5c3c3a+T5c3c3b+T5c3c3c1+T5c3c3c2+T5c3d1+
  T5c3d2+T5c3d3+T5c4); task-index rows updated
  accordingly. `meridian59.md` untouched. Checkbox state
  unchanged: T5a/T5b1a/T5b1b/T5b2a/T5b2b/T5c1/T5c2a/T5c2b/
  T5c3a/T5c3b/T5c3c1/T5c3c2/T5c3c3a/T5c3c3b/T5c3c3c1/
  T5c3c3c2 `[x]`, T5c3d1/T5c3d2/T5c3d3/T5c4/T6/T7 and M5
  exit `[ ]`.

- v1.35: freeze M5 zero-HP death orchestration (docs only,
  spec v0.3.50 -> v0.3.51 new §9.5.1j; implementation
  deferred to Phase B): independent re-read of pinned
  `Meridian59/Meridian59@095c07b` (`player.kod::Killed`
  8-step order, `NewHealth` timer-only semantics, Token
  `NewUnused` threshold-restore-before-vitals order,
  `user.kod::UserGotoDeadRoom` newbie-home vs Underworld
  routing, `uworld.kod::LeaveHold` penalties-only,
  `soldshld.kod` classification-only non-scope;
  `meridian59.md` untouched); exact `internal/sim` contract
  (`ImmediateDeathItemPolicy`, `ImmediateDeathResolvedInput`,
  `ImmediateDeathDisposition`,
  `ImmediateDeathOrchestrationResult`, ephemeral
  `lastDeathSeconds` + inspection helper); source-faithful
  call sequence (structural pre-validation with probe
  disposition, double-death guard, stamp, branch disposition,
  Avoided HP=1 + reconcile, predicted-token Prepare-before-
  begin with no reservation API change, begin token-equality
  invariant, infallible same-turn Activate); lastDeath
  semantics (blocked/structural-error unchanged; Avoided/
  Cheap/Normal stamp; Prepare failure stays stamped);
  binding non-scope (T5c3d/T5c4/T6/T7/combat integration/
  Store/SQL/gateway). Checkbox state unchanged: T5c3c3c2
  still `[ ]`.

- v1.34: split M5-T5c3c3c into reservation-first T5c3c3c1 +
  orchestration-later T5c3c3c2 (docs only, spec v0.3.49 ->
  v0.3.50 new §9.5.1i; split only): T5c3c3c1 (guaranteed
  bounded executor reservation + prepared activation seam;
  depends on T5c3c3b; `internal/persist` + one narrow
  store-independent sim interface; reserve-before-begin
  capacity ownership, reserved/prepared/activated/cancelled
  one-shot state machine, prepare/freeze/map before owner
  mutation, queue-full-incapable activation, idempotent
  cancel with `ErrDeathReservationCanceled`, exact
  queue-permit accounting, `TrySubmit` validation-order
  preservation; no life-state/zero-HP/double-death/T5a/
  placement/gateway work) + T5c3c3c2 (zero-HP resolved T5a
  orchestration + double-death runtime gate; depends on
  T5c3c3a + T5c3c3b + T5c3c3c1 + T5a + T5c3c2; exact gameplay
  API deferred to its own Phase-A source freeze); freeze the
  `player.kod::Killed` source-order readiness note (pinned
  `Meridian59/Meridian59@095c07b`; resolve cost, double-death
  early return, `CancelRescue`, timestamp update, location
  capture, Avoided test, HP=1 Avoided return, Cheap/Normal
  pipeline; blocked attempts do NOT update lastDeath while
  Avoided/real deaths DO; §9.5.3 "deterministic tick clock"
  wording superseded for c3c3c2 by a resolved whole-second
  `GetTime()` scalar); T5c3d now depends on T5c3c1+T5c3c2+
  T5c3c3a+T5c3c3b+T5c3c3c1+T5c3c3c2+T5b2a+T5b2b+T5c2a+T5c2b;
  T5c4 now depends on T5c3a+T5c3b+T5c3c1+T5c3c2+T5c3c3a+
  T5c3c3b+T5c3c3c1+T5c3c3c2+T5c3d + the M4
  gateway/presence/fanout foundation; M5-T7 now depends on
  the EIGHTEEN-task M5-T5 set
  (T5a+T5b1a+T5b1b+T5b2a+T5b2b+T5c1+T5c2a+T5c2b+T5c3a+T5c3b+
  T5c3c1+T5c3c2+T5c3c3a+T5c3c3b+T5c3c3c1+T5c3c3c2+T5c3d+T5c4);
  task-index rows updated accordingly. `meridian59.md`
  untouched. Checkbox state unchanged: T5a/T5b1a/T5b1b/T5b2a/
  T5b2b/T5c1/T5c2a/T5c2b/T5c3a/T5c3b/T5c3c1/T5c3c2/T5c3c3a/
  T5c3c3b `[x]`, T5c3c3c1/T5c3c3c2/T5c3d/T5c4/T6/T7 and M5
  exit `[ ]`.

- v1.33: freeze M5 proven death persistence recovery (docs
  only, spec v0.3.48 -> v0.3.49; no scope, dependency, or
  checkbox change): freeze the c3c3b recovery contract in new
  §9.5.1h text (no blind `CommitDeathEntry` replay; exact
  callback expected revisions; `ErrSaverReconcileRequired`-only
  recovery trigger; every-participant worker-local
  reconciliation; exact-`expected+1` plus semantic content /
  pending / PK-protection proof; proven lost-ack completion
  with no replay; stable `ErrDeathCommitUnproven` fail-closed
  sentinel; `Applied`/`Duplicate` success;
  `ErrSimIngressFull` bounded redelivery; terminal
  engine-stop/mismatch errors; T5c3c3c reservation still
  deferred to its own freeze). `meridian59.md` untouched.
  Checkbox state unchanged: T5a/T5b1a/T5b1b/T5b2a/T5b2b/T5c1/
  T5c2a/T5c2b/T5c3a/T5c3b/T5c3c1/T5c3c2/T5c3c3a `[x]`,
  T5c3c3b/T5c3c3c/T5c3d/T5c4/T6/T7 and M5 exit `[ ]`.

- v1.32: freeze M5 immediate-death async completion split (docs
  only, spec v0.3.47 -> v0.3.48 new §9.5.1h; split only): replace
  the single M5-T5c3c3 with T5c3c3a (authoritative
  immediate-death owner completion + typed completion ingress,
  `internal/sim` only; depends on T5c3b + T5c3c1 + T5c3c2; owns
  the `ImmediateDeathCompletion` value, atomic owner-only
  placement/vitals/runtime/durable install with Applied vs
  Duplicate correlation, and the typed same-mailbox ingress
  command) + T5c3c3b (bounded off-owner `CommitDeathEntry` +
  materialized recovery executor; depends on T5c3c3a + T5c2a +
  T5c2b; no live-entity mutation; recovery algorithm deferred to
  its own audit) + T5c3c3c (zero-HP/T5a orchestration +
  double-death gate + bounded execution submission; depends on
  T5c3c3a + T5c3c3b + T5a + T5c3c2; no `DeathPersisting`
  stranding on saturation); T5c3d now depends on T5c3c1+T5c3c2+
  T5c3c3a+T5c3c3b+T5c3c3c+T5b2a+T5b2b+T5c2a+T5c2b; T5c4 now
  depends on T5c3a+T5c3b+T5c3c1+T5c3c2+T5c3c3a+T5c3c3b+T5c3c3c+
  T5c3d + the M4 gateway/presence/fanout foundation; M5-T7 now
  depends on the SEVENTEEN-task M5-T5 set
  (T5a+T5b1a+T5b1b+T5b2a+T5b2b+T5c1+T5c2a+T5c2b+T5c3a+T5c3b+
  T5c3c1+T5c3c2+T5c3c3a+T5c3c3b+T5c3c3c+T5c3d+T5c4); task-index
  rows updated accordingly. `meridian59.md` untouched. Checkbox
  state unchanged: T5a/T5b1a/T5b1b/T5b2a/T5b2b/T5c1/T5c2a/T5c2b/
  T5c3a/T5c3b/T5c3c1/T5c3c2 `[x]`,
  T5c3c3a/T5c3c3b/T5c3c3c/T5c3d/T5c4/T6/T7 and M5 exit `[ ]`.

- v1.31: freeze M5 complete death capture mapping (docs only,
  spec v0.3.46 -> v0.3.47 new §9.5.1g; no scope or dependency
  change): correct `ResetAtrophyFlags` (`player.kod`
  `ResetAtrophyFlags` negates EVERY entry in BOTH `plSpells` AND
  `plSkills`; Voxilian `AtrophyFlag = true` for every spell and
  every skill); freeze the store-independent `PlayerDurableState`
  shadow, validation, deep-freeze, additive full-state
  installation, advancement `{}`/`adv_points`/`gain_chance`
  contract, no-timer (`adv_timer_due` absence) encoding, Normal
  (`0x70` clear, all-spell+skill atrophy reset, deadline delete)
  vs Cheap (byte-for-byte preservation) advancement mapping,
  atomic owner-local begin+capture composing c3c1, opaque
  capture-order T5a keys, pure real-death builder (Normal drops
  + Token special relocation + resulting post-death shadow +
  sim-only killer + immutable ProtoID), `internal/persist` mm
  conversion (`math.Round(meters*1000)` int64; NaN/Inf/overflow
  rejected), mechanical Store mapper (zero `ExpectedRevision`
  placeholders; T5c2b stays the only revision injector), and the
  Saver/recovery/worker/gateway/proto non-scope. Correct the
  v1.30 history-text mistake that listed `M5-T5c3c1` as
  unchecked (tracker already `[x]`). Checkbox state unchanged:
  T5a/T5b1a/T5b1b/T5b2a/T5b2b/T5c1/T5c2a/T5c2b/T5c3a/T5c3b/
  T5c3c1 `[x]`, T5c3c2/T5c3c3/T5c3d/T5c4/T6/T7 and M5 exit `[ ]`.

- v1.30: freeze M5 death advancement timer fidelity (docs only,
  spec v0.3.46 corrected §9.5.6; no scope or dependency change):
  the Normal-death immediate advancement result is FIVE effects
  (points → 0, gain-chance integer half truncating toward zero,
  `ResetGainFlags`, `ResetAtrophyFlags`, advancement timer
  cancelled), carried by the pure T5a `DeathAdvancementPlan` as one
  explicit boolean intent (`CancelAdvancementTimer`: Normal = true,
  Cheap = false); `ResetGainFlags` owns the durable `0x70`
  (`PFLAG_DID_DAMAGE`/`PFLAG_TOOK_DAMAGE`/`PFLAG_DODGED`) clear
  while `poKill_target` stays ephemeral with no durable
   representation. T5c3c2 consumes the corrected plan (note on its
   entry; scope and dependencies unchanged); T5a stays `[x]`.
   Checkbox state unchanged:
   T5a/T5b1a/T5b1b/T5b2a/T5b2b/T5c1/T5c2a/T5c2b/T5c3a/T5c3b/T5c3c1 `[x]`,
   T5c3c2/T5c3c3/T5c3d/T5c4/T6/T7 and M5 exit `[ ]`.

- v1.29: freeze M5 immediate-death lifecycle split (docs only,
  spec v0.3.45 new §9.5.1f; split only): replace the single
  M5-T5c3c with T5c3c1 (immediate-death lifecycle gate + attempt
  correlation, `internal/sim` only; depends on T5c3a + T5c3b; no
  T5a composition, no durable capture, no Store/persist/PG,
  no workers, no completion ingress, no zero-HP dispatch, no
  gateway/proto) + T5c3c2 (complete immutable immediate-death
  capture + Store-domain persistence mapping; depends on T5c3c1 +
  T5a + T5c2b; exact capture API deferred to its own
  pre-implementation audit; re-reading older PG state over newer
  in-memory state is forbidden) + T5c3c3 (bounded off-owner death
  persistence/recovery + zero-HP orchestration + typed owner
  completion; depends on T5c3c1 + T5c3c2 + T5c2a + T5c2b; no
  goroutine-per-death, no blind replay); T5c3d now depends on
  T5c3c1 + T5c3c2 + T5c3c3 + T5b2a + T5b2b + T5c2a + T5c2b; T5c4
  now depends on T5c3a + T5c3b + T5c3c1 + T5c3c2 + T5c3c3 + T5c3d
  + the existing M4 gateway/presence/fanout foundation;
  M5-T5-complete is now
  T5a+T5b1a+T5b1b+T5b2a+T5b2b+T5c1+T5c2a+T5c2b+T5c3a+T5c3b+T5c3c1+
  T5c3c2+T5c3c3+T5c3d+T5c4 (FIFTEEN T5 tasks); M5-T7 depends on
  the new complete T5 set; §9.5.1f also freezes the
  `PlayerLifeState` gate, the ephemeral `DeathAttemptToken`
  epoch, the raw-vs-active resolver split, the owner-local
  begin/completion transitions, the ordinary gameplay gates, and
  the complete-state persistence gap (T5c3c1 constructs NO
  `store.DeathEntryRequest`). Checkbox state unchanged:
  T5a/T5b1a/T5b1b/T5b2a/T5b2b/T5c1/T5c2a/T5c2b/T5c3a/T5c3b `[x]`,
  T5c3c1/T5c3c2/T5c3c3/T5c3d/T5c4/T6/T7 and M5 exit `[ ]`.

- v1.28: freeze M5 death-safe runtime transition primitives
  (docs only, spec v0.3.44 new §9.5.1e; no scope or
  dependency change): correct the `meridian59.md` §9.5
  immediate-death timer wording against upstream
  `Meridian59/Meridian59@095c07b` (`player.kod` — real death
  calls `NewHealth`/`NewMana`/`NewVigor`, but `NewVigor`
  only bounds/draws Vigor and creates no rest timer, so
  post-death rest stays absent); freeze the exact T5c3b
  contract (`PlayerQuiesceForDeath` ephemeral quiesce with
  sequence-anchor/history/recent-OpID preservation and no
  lifecycle gate + `PlayerInstallPostDeathState` trusted
  explicit remap with all-or-nothing validation, existing
  handoff reuse, history-discontinuity reset, no
  `PlayerVitalsObserver` event, current-tick health/mana
  recreation). T5c3b still `internal/sim` only, still
  depends on T5c3a + T5a + T4b2. Checkbox state unchanged:
  T5a/T5b1a/T5b1b/T5b2a/T5b2b/T5c1/T5c2a/T5c2b/T5c3a `[x]`,
  T5c3b/T5c3c/T5c3d/T5c4/T6/T7 and M5 exit `[ ]`.

- v1.27: freeze M5 async death runtime ownership split (docs
  only, spec v0.3.43 §9.5.1d; split only): replace the single
  M5-T5c3 with T5c3a (player runtime identity + typed owner
  ingress, `internal/sim` only; no death/relocation/persistence/
  gateway work) + T5c3b (death-safe relocation + runtime
  quiesce/reinitialization, `internal/sim` only; depends on
  T5c3a + T5a + T4b2) + T5c3c (immediate-death async
  persistence/reconciliation state machine, `internal/sim` +
  `internal/persist`; depends on T5c3a + T5c3b + T5a + T5c2a +
  T5c2b; off-owner execution with typed owner completion, no
  entity mutation from persistence goroutines) + T5c3d
  (pending-death / Portal / Underworld-exit async lifecycle,
  `internal/sim` + `internal/persist`; depends on T5c3c + T5b2a
  + T5b2b + T5c2a + T5c2b; 120 `respawn_ack` is NOT the synonym
  for Underworld `LeaveHold`/`ApplyDeathPenalties`); T5c4 now
  depends on T5c3a + T5c3b + T5c3c + T5c3d + the existing M4
  gateway/presence/fanout foundation; M5-T5-complete is now
  T5a+T5b1a+T5b1b+T5b2a+T5b2b+T5c1+T5c2a+T5c2b+T5c3a+T5c3b+T5c3c+
  T5c3d+T5c4; M5-T7 depends on the new complete T5 set;
  §9.5.1d also freezes the T5c3a API, the typed bootstrap
  boundary, the §3 dependency direction, and the future
  off-owner execution rule. Checkbox state unchanged:
  T5a/T5b1a/T5b1b/T5b2a/T5b2b/T5c1/T5c2a/T5c2b `[x]`,
  T5c3a/T5c3b/T5c3c/T5c3d/T5c4/T6/T7 and M5 exit `[ ]`.

- v1.26: freeze M5 newbie-home no-pending death path (docs
  only, spec v0.3.42 §9.5.8b; no scope or dependency change):
  a real death with `NewbieHomeRespawn == true` keeps its
  corpse plan, post-death vitals, death-entry character
  persistence, optional token relocation, and optional kill
  audit, but `PlanPendingDeath` returns `DeathPhaseNone`
  (no delayed Underworld penalty phase) and `CommitDeathEntry`
  writes NO `pending_deaths` row; all other real deaths
  (incl. cost-zero Underworld cheap deaths) stay
  `DeathPhasePending`. `DeathEntryRequest` gains one
  caller-resolved `NewbieHomeRespawn bool` (`true` requires
  `EffectiveDeathCost == 0`, else `ErrInvalidDeathEntry`
  before Begin); the newbie-home transaction CASes the
  character root first, prechecks the pending row via the
  existing generated query (existing row →
  `ErrDeathAlreadyPending`, never stale), then items/corpse/
  optional kill with no pending insert; commit-ambiguity
  recovery stays materialized-state-only with no blind replay
  (T5c2a `Pending == nil` is correct for this path); T5c2b
  scalar capture unchanged; T5c3 remains blocked with no
  runtime/gateway/proto work. Checkbox state unchanged:
  T5a/T5b1a/T5b1b/T5b2a/T5b2b/T5c1/T5c2a/T5c2b `[x]`,
  T5c3/T5c4/T6/T7 and M5 exit `[ ]`.

- v1.25: freeze M5 critical death persistence adapters (docs
  only, spec v0.3.41 §9.5.1c; no scope or dependency change):
  T5c2b is the `internal/persist` composition layer between
  `sim.Saver.WriteCriticalSet` and the frozen
  `Store.CommitDeathEntry` / `CommitPortalOfLife` /
  `CommitDeathPenalties` transactions (no new migration/query/
  generated code, no Store method, no `sim` production change);
  DeathEntry participants are exactly character + one item root
  per `Items` element (zero items valid; duplicates fail before
  Store), Portal/Penalties are one-key critical sets;
  execution-time Saver revisions overwrite hostile
  `ExpectedRevision` inside the callback only; immutable
  request capture reusing the existing clone rules;
  `store.ErrStaleRevision` via `mapStale` vs semantic causes
  preserved; ZERO public result until Saver success; no
  automatic reconciliation/retry (T5c3 reconciles via T5c2a);
  commit-ambiguity recovery is materialized state, never blind
  replay. Checkbox state unchanged: T5a/T5b1a/T5b1b/T5b2a/
  T5b2b/T5c1/T5c2a `[x]`, T5c2b/T5c3/T5c4/T6/T7 and M5 exit
  `[ ]`.

- v1.24: freeze M5 materialized death recovery (docs only, spec
  v0.3.40 §9.5.1b; split only): replace M5-T5c2 with T5c2a
  (materialized death recovery reads + reload adapters; Store
  read-only composite loaders in ONE `REPEATABLE READ, READ ONLY`
  transaction each + `persist` staged `ReloadFunc` adapters reusing
  `ReconcileSaver`; depends on T5b1b + T5b2a + T5b2b; no new
  migration/query/generated code) + T5c2b (critical death
  persistence adapters via `Saver.WriteCriticalSet`, incl. one-key
  sets for Portal/DeathPenalties; depends on T5c1 + T5c2a + the T5b
  Store transactions); T5c3 now depends on T5a + T4b2 + T5c2a +
  T5c2b; M5-T5-complete is now
  T5a+T5b1a+T5b1b+T5b2a+T5b2b+T5c1+T5c2a+T5c2b+T5c3+T5c4; M5-T7
  depends on the new complete T5 set. Checkbox state unchanged:
  T5a/T5b1a/T5b1b/T5b2a/T5b2b/T5c1 `[x]`,
  T5c2a/T5c2b/T5c3/T5c4/T6/T7 and M5 exit `[ ]`.

- v1.23: freeze M5 death runtime integration layers (docs only,
  spec v0.3.39 §9.5.1a + §8.3.17; no scope grab, split only):
  replace the single M5-T5c with T5c1 (multi-root critical Saver
  coordination, `internal/sim`, Store-agnostic, no PG I/O; depends
  on the existing Saver/persistence foundation) + T5c2 (death
  persistence adapters + materialized recovery; depends on T5b1b +
  T5b2a + T5b2b + T5c1; ONLY layer that may add Store read APIs) +
  T5c3 (sim-owner death lifecycle + resolved placement; depends on
  T5a + T4b2 + T5c2) + T5c4 (gateway death wire/state integration
  + reconnect E2E on the existing 120/214/215 codecs; depends on
  T5c3 + M4 gateway/presence/fanout); M5-T5-complete is now
  T5a+T5b1a+T5b1b+T5b2a+T5b2b+T5c1+T5c2+T5c3+T5c4; M5-T7 depends on
  the new complete T5 set; opcode 120 ownership moves to M5-T5c4.
  Checkbox state unchanged: T5a/T5b1a/T5b1b/T5b2a/T5b2b `[x]`,
  T5c1/T5c2/T5c3/T5c4/T6/T7 and M5 exit `[ ]`.

- v1.22: freeze M5 death-penalty consumption ordering (docs only,
  spec v0.3.38 §9.5.11a; no scope or dependency change): binding
  T5b2b Store order is validate, Begin, `saveCharacterSnapshotTx`
  exactly ONCE (root CAS FIRST internally), `GetPendingDeathByCharacter`,
  raw-cost verification (`ExpectedPendingCost` =
  `DeathPenaltyInput.PendingCost`, NOT `DeathPenaltyPlan.ScaledCost`),
  same-txn `DeletePendingDeathByCharacter`, commit once; no manual
  pre-CAS, no helper split; pre-verification state tentative with
  atomic rollback; `corpse_id` set/NULL and `portal_used` false/true
  are not exit gates; Store knows no `DeathPenaltyPlan` (no sim
  import); success always consumes pending (no `ClearPending` flag).
  Checkbox state unchanged: T5a/T5b1a/T5b1b/T5b2a `[x]`,
  T5b2b/T5c/T6/T7 and M5 exit `[ ]`.

- v1.21: split M5-T5b2 into T5b2a (durable Portal-of-Life state
  transition) + T5b2b (exactly-once Underworld-exit penalty
  consumption) and freeze the phase-two contracts (spec v0.3.37
  §9.5.1/§9.5.10a/§9.5.11a: T5b2a <- T5a + T5b1b, T5b2b <-
  T5a + T5b1b + T5b2a, T5c <- T5b1b + T5b2a + T5b2b + T4b2;
  M5-T5-complete = T5a+T5b1a+T5b1b+T5b2a+T5b2b+T5c; Portal and
  Underworld-exit penalties each write ZERO ledger rows — the old
  "ledger rows in the same txn" T5b2 wording is not binding; no
  death-penalty ledger kind invented; characters.revision is the
  sole character aggregate CAS root with spells/skills as child
  rows through saveCharacterSnapshotTx; Portal persists
  immediately as its own transaction with once + lowers-only
  semantics against the durable target corpse ID; T5b2b future
  contract frozen to character-root-CAS-first + pending
  verification + same-txn delete). Docs only; T5b2a/T5b2b/T5c/T6/T7
  stay `[ ]`.

- v1.20: freeze M5 death-entry item relocation (docs only, spec v0.3.36;
  no scope or dependency change): normal death drops persist as GROUND
  placements at the death position (never `kind = 2` corpse-contained);
  the generated corpse ID serves `pending_deaths.corpse_id` only; the
  generic cheap drop plan stays empty while Token death carries a
  separate caller-resolved unuse/ground-relocation side effect owned as
  T5a pure `TokenDeath` signal / T5c resolution / T5b1b atomic
  persistence. T5a stays `[x]` (its pure signal is sufficient);
  checkbox state unchanged: T5a/T5b1a `[x]`, T5b1b/T5b2/T5c/T6/T7 and
  M5 exit `[ ]`.

- v1.19: documentation consistency correction only (no scope or
  dependency change): M5-T5b1b task text now states the §9.5.8a audit
  rule verbatim — the existing kills audit row in the same txn only
  when the resolved killer fits the existing character/mob identity
  domain, and no death/drop ledger row. The older "kill/ledger audit
  rows" phrasing contradicted the frozen ledger decision. Checkbox
  state unchanged: T5a/T5b1a `[x]`, T5b1b/T5b2/T5c/T6/T7 and M5 exit
  `[ ]`.

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

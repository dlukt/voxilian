# Voxilian Backend — Implementation Plan (v1.0)

> Source of truth for WHAT: `docs/backend-spec.md` (frozen v0.3.3).
> This file is the WHAT-ORDER + WHO-DOES-IT tracker.
> If implementation discovers the spec is wrong, change the SPEC first
> (separate commit), then implement — never silently diverge.

## How to work this plan (read before picking up a task)

1. Pick ONE unchecked task `[ ]` in the earliest incomplete milestone
   (content tasks in M9 may run in parallel once M1 is done).
2. Keep the diff small: **≤ ~8 files, ≤ ~1500 lines** including tests.
   If it doesn't fit, split the task and note it here first.
3. Implement against the cited spec sections. Spec ambiguity → stop and
   ask (or fix the spec in a separate commit), don't invent protocol.
4. Definition of done for EVERY task:
   - `go build ./...`, `go vet ./...`, `gofmt -l .` clean;
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

Exit: `compose up` gives PG 18 + builder; CI builds, vets, tests, publishes GHCR image.

- [ ] **M0-T1** Module layout + CLI skeleton. Create `internal/{config,gateway,sim,store,session,auth,admin,observe,world}`, `cmd/{serve,migrate,admin,seed}`, `migrations/`, `queries/`, `sqlc.yaml`, `compose.yaml` (dev profile: `postgres:18-alpine` + build target + healthcheck + migrate init step). Cobra subcommands stubbed (serve/migrate/admin/seed print not-implemented). Spec: §3, §10.
- [ ] **M0-T2** Config + observability skeleton. `VOX_*` env + `config.yaml` loader (§10 config list); `/healthz` (liveness stub), `/readyz` (not-ready stub), `/metrics` (Prometheus, empty registry + build info); structured `slog` with `tick/cell/charID` fields wired. Tests: config precedence (file < env), endpoint smoke tests.
- [ ] **M0-T3** CI + GHCR publish. Workflow: build, vet, fmt-check, `go test ./...`, migrate-up-against-service-PG check, build+push `ghcr.io/dlukt/voxilian` on `main` (prod compose pulls it, §10). Include `testcontainers-go` availability check job.
- [ ] **M0-T4** Test harness conventions. Fake clock + seeded RNG helpers (`internal/simtest` or similar), testcontainers PG 18 helper, golden-file helper (`testdata/*.hex` compare). One example test each. Spec: §12.
- [ ] **M0 exit criteria met** (compose up works, CI green on a trivial PR).

## M1 — Migrations + CAS store layer (first per reviewer order)

Exit: all §8 tables exist via goose; `Store` interface covers CRUD + CAS; stale-write test proves the revision guard.

- [ ] **M1-T1** Migration `0001`: `citext` extension + `accounts` + `characters` (+ partial unique indexes `chars_acct_slot_uidx`, `chars_name_uidx` via `CREATE UNIQUE INDEX ... WHERE deleted_at IS NULL`). Spec: §8.
- [ ] **M1-T2** Migration `0002`: `character_spells`, `character_skills` (+PKs, ability CHECK 1–99, stat CHECKs 1–50 on characters). Spec: §8.
- [ ] **M1-T3** Migration `0003`: `item_instances` (+`revision`), `item_locations` (all 5 kinds, full per-kind CHECKs with `IS [NOT] NULL`, self-containment CHECK), `corpses`, `banks` (+`revision`). Spec: §8.
- [ ] **M1-T4** Migration `0004`: `ledger` (`num_nonnulls` CHECKs), `kills`, `bans/mutes`. Spec: §8.
- [ ] **M1-T5** sqlc queries + generated code (`queries/*.sql` → `internal/store/gen`, `pgx/v5` emit): account/character CRUD, slot-claimed create (relies on partial unique index → map unique-violation to `slot_occupied`/`name_taken`), spell/skill get/set, item+location txn helpers, corpse/bank/ledger/kill appends. Spec: §8.
- [ ] **M1-T6** `Store` interface + CAS aggregate writes. Character aggregate save (root CAS `UPDATE ... WHERE revision=$expected` → child writes in same txn), item aggregate save, bank save. Unit + testcontainers tests: stale revision affects 0 rows, aborts txn, increments metric. Spec: §8.1, D7.
- [ ] **M1-T7** `voxilian migrate up/down/status` wired to embedded migrations (one-shot container compatible). Spec: §8, §10.
- [ ] **M1 exit criteria met** (migrations apply cleanly to fresh PG 18; CAS test suite green).

## M2 — Binary protocol + golden vectors

Exit: every §6 opcode encodes/decodes both sides (Go done; Godot side stubs + fixtures ready for client plan); fuzz + golden suites green.

- [ ] **M2-T1** Codec core (`internal/proto`): envelope `[opcode|msg_version|seq|tick|payload]`, LE integers, `string`/`array`/`cell`/`pos`/`angle` primitives, 64 KiB frame cap, string/array caps (1024; chat 512; accessToken 8 KiB), `msg_version` trailing-skip, `[u16 entryLen]` entry framing. Round-trip tests. Spec: §6 framing.
- [ ] **M2-T2** Opcodes `100/101/200/201/202` (hello/welcome/reauth/reauth_ok/error) + `216/217/219` (character_list/op, world_ready) + `121–126` (char CRUD/ack/leave). Round-trip tests. Spec: §6.1, §6.2.
- [ ] **M2-T3** Opcodes `102–120` intents (incl. `inputSeq`/`yaw`, fixed-`u32` `105 use`, vendor+stable-listing `114 buy`) + `203–215/218/220` deltas (incl. `lastProcessedInputSeq`, chunk fragments ≤60 KiB, shop_list). `211` entry layout FROZEN here per entry-schema task: document every field+width in §6 (spec edit commit first). Spec: §6.3.
- [ ] **M2-T4** Golden binary fixtures: checked-in hex vectors for ≥1 message per opcode; Go decode test; fixture format documented for the Godot implementer (placeholder `client/tests/` dir + README). Spec: §12.
- [ ] **M2-T5** Fuzz + robustness: Go fuzz targets per decoder; malformed/truncated/oversized corpus tests; unknown-opcode and unknown-trailing-bytes tolerance tests; `seq`/`inputSeq` wraparound tests (modulo-2³² arithmetic). Spec: §12.
- [ ] **M2 exit criteria met** (all opcodes round-trip; fuzz 60 s clean per target; fixtures committed).

## M3 — Session lifecycle + gateway

Exit: full §6.1 state machine live over real WS; char CRUD end-to-end against PG; takeover/kick/backpressure tested.

- [ ] **M3-T1** WS server + session registry (`sessionID → {sub,accountID,charID?,conn,state,tokenExp}`, indexed by sub+character; per-account lifecycle guard mutex). States + per-opcode permission table enforced; `bad_state` errors. Tests: illegal-state matrix. Spec: §6.1, §7.
- [ ] **M3-T2** Auth hookup (JWT validation behind interface; test fake + real JWKS path stub for M11): `100 hello` → JWKS check → account auto-provision → `200 welcome`; `101 reauth`; 90 s hard deadline (intents rejected post-grace, then disconnect). Tests with forged/expired tokens. Spec: §6.2, §11.
- [ ] **M3-T3** Character CRUD over WS: `121–124` + `217` results + error codes (`name_taken/slot_occupied/bad_stats/bad_budget/character_in_use`); `126 leave_world` (AOI clear, flush, →AUTHENTICATED); single-txn create per §8.1. Testcontainers tests incl. concurrent double-create race (one wins). Spec: §6.1, §8, §9.
- [ ] **M3-T4** `enter_world` baseline: `CHARACTER_SELECTED` → snapshots + paced `218`s + `220`s → `219 world_ready` → `IN_WORLD` barrier verified (no `world_ready` before baseline fully written). Duplicate-login/takeover: kick-old, quiesce/flush-before-baseline, per-account serialization test (two racing `enter_world` → exactly one `IN_WORLD`). Spec: §6.1.
- [ ] **M3-T5** Backpressure: two-lane bounded outbound queues (coalescible vs critical), `125 ack` flow control, slow-client disconnect + full-resync test, saturation metrics, cell-owner non-blocking rule (no indefinite block; fail-closed disconnect). Load-ish test with fake slow peer. Spec: §7.1.
- [ ] **M3 exit criteria met** (lifecycle fuzz/property test: random opcode sequences never violate state table).

## M4 — Deterministic sim skeleton

Exit: 20 Hz tick loop, cells, server-authoritative movement with reconciliation anchors, handoff, history ring, CAS saver.

- [ ] **M4-T1** Tick loop + cell grid + entity registry. 20 Hz sim, 32 m cells, single-writer-per-cell structure (one process), injectable clock+RNG, per-entity position-history ring (2 s @ 20 Hz). Determinism test (fixed seed → identical trace). Spec: §4, §5.
- [ ] **M4-T2** Movement integration: `102` intents → integrate (walk 3.5/run 7 m/s, vigor gate hook) → `205` with `lastProcessedInputSeq`; server-side collision vs voxel/world source stub; anomaly tripwire (speed/teleport → correct+log). Reconciliation-anchor unit tests (processed-seq monotonicity). Spec: §5, §6.3, §11.
- [ ] **M4-T3** Cell ownership + handoff: epoch/generation, migrate-queue routing (`202 error{retry}` only on saturation), cross-cell `DamageIntent{opID}` + idempotent apply + bounded dedupe carried on handoff; trade-coordinator PG-txn + opID commits; post-commit-failure reload rule (test: drop notification → aggregate reconciled from PG before next mutation). Spec: §5.1.
- [ ] **M4-T4** Snapshot saver: 60 s dirty-queue + critical write-through paths, aggregate-root CAS writes, stale-write metric, shutdown flush with deadline. Crash-injection tests (kill mid-save → invariants hold). Spec: §8.1, §10.
- [ ] **M4 exit criteria met** (movement + handoff race tests green; saver property tests green).

## M5 — Combat + vitals + death

Exit: M59 combat/vitals/death playable against stub mobs; formulas golden-tested.

- [ ] **M5-T1** Offense/defense/hit + weapon tables: `(Off*55)/Def` 10–95%, weapon type/quality, 1 swing/s, vigor costs, 30/hit + ⅓-HP caps, severity text hooks. Golden vectors from `meridian59.md` §7. Spec: §9, meridian59 §7.
- [ ] **M5-T2** Armor/shields/resists: `ModifyDefensePower/Damage`, block/parry/dodge rolls, ±100 resist clip, spell-vs-weapon reduction rules. Golden vectors. Spec: §9.
- [ ] **M5-T3** Spell damage + touch/walls/AoE: `rand*(50+power/2)/99`, wall ticks, touch scaling, Illusionary-Wounds rules, quake falloff; mana/vigor/reagent/karma gates; cast/post-cast timing. Spec: §9.
- [ ] **M5-T4** Vitals/regen/hunger: HP=level caps, mana+nodes, exertion/rest/thresholds, regen tick formulas, stomach decay. Golden vectors + timer tests (fake clock). Spec: §9, meridian59 §4.
- [ ] **M5-T5** Death pipeline: corpse + full droppable drop (PK tags), advancement wipe/halve, Underworld-region respawn, leaving penalties (Stam saves), Portal-of-Life hook; single-txn state+ledger. Crash-during-death test. Spec: §9, §8.1.
- [ ] **M5 exit criteria met** (scripted duel bot: two M4+M5 entities fight to death, corpse lootable).

## M6 — Progression (use-based)

Exit: skills/HP advance by use per M59 math; trainers gate by learn points.

- [ ] **M6-T1** Ability improvement: success formula, two-gate +1%, 10 pts/15–22 min window, 75-swing checks, softcaps, write-through milestones. Golden vectors. Spec: §9, meridian59 §6.
- [ ] **M6-T2** HP advancement + learn points: highmark roll, Stam effects, newbie bonus, anti-buff halving; learn-points gate (16+Int, top-3 rule) at trainer NPCs; soft-delete-safe. Tests incl. boundary rolls. Spec: §9.
- [ ] **M6-T3** Forget/atrophy hooks: forget potions + Forget spell wiring (atrophy stays disabled per spec — config flag `atrophy=false`, code path present but off). Spec: §9.
- [ ] **M6 exit criteria met** (bot grinds 1→99 tracked skill on schedule curve within tolerance).

## M7 — Mobs + AI + loot

Exit: bestiary live with spawners, AI timers, caster mobs, loot rolls.

- [ ] **M7-T1** Mob protos + spawners: proto table (level/diff/karma/atk/resists/spells/loot TID) loaded from seed files; volume-bound spawners + respawn timers; HP/Off/Def/damage formulas golden-tested. Spec: §9, meridian59 §8.
- [ ] **M7-T2** AI brains + timers: chase/move/attack staggers, aggro/flee/regroup flags, karma-aggro, caster `MonsterCastSpell` chance tables, pathing on cell grid + Fine collision for NPC moves. Handoff-race tests for migrating mobs. Spec: §5, §9.
- [ ] **M7-T3** Loot: TID tables, enchanted-drop chances, corpse/world placement via `item_locations`, PK-tag rules. Distribution tests (χ²-ish sanity on seeded RNG). Spec: §9.
- [ ] **M7 exit criteria met** (overnight soak: spawners/AI/loot stable, no stuck mobs in metrics).

## M8 — Trade / bank / vault

Exit: atomic player trade + bank/vault with ledger audit; race tests green.

- [ ] **M8-T1** Trade state machine: offer/counter/accept/cancel (`110–113`, `212/213`), both-party re-confirm, single-PG-txn accept (ownership + ledger, §8.1), lawful-refusal + PK-tag rules, double-accept race test. Spec: §9, §6.3.
- [ ] **M8-T2** Bank + vault: per-character per-system balances, vault `item_locations` kind=3 with region, fees to ledger, cross-cell trade via coordinator+opID path. PG-loss-mid-trade test (unready rejects). Spec: §9, §10.
- [ ] **M8 exit criteria met** (trade fuzz: random interleavings never duplicate/lose items or coins — ledger reconciles).

## M9 — Seed data: full M59 content port (parallelizable after M1)

Exit: every school/skill/mob/weapon/armor in versioned seed files, `voxilian seed` idempotent.

- [ ] **M9-T1** Seed pipeline: file format + validator (CHECK-mirroring) + idempotent upsert-by-proto-id + `voxilian seed` wiring. One sample school to prove it. Spec: §10 seed bullet.
- [ ] **M9-T2** Schools Shal'ille + Qor (all spells: costs/effects/reqs). Spec: meridian59 §5.
- [ ] **M9-T3** Schools Kraanan + Faren. Spec: meridian59 §5.
- [ ] **M9-T4** Schools Riija + Jala (incl. hinder matrix data, songs as room enchants). Spec: meridian59 §5.
- [ ] **M9-T5** Weaponcraft + utility skills (strokes, proficiencies, parry/dodge/block/disarm/second wind). Spec: meridian59 §5.
- [ ] **M9-T6** Bestiary A–M (protos + loot TIDs + spawner placements for classic starter region). Spec: meridian59 §8.
- [ ] **M9-T7** Bestiary N–Z + bosses + NPC roles (teachers/vendors/bankers/priests). Spec: meridian59 §8.
- [ ] **M9-T8** Weapons/armor/shields/ammo protos + vendor `220` listings. Spec: meridian59 §7.
- [ ] **M9 exit criteria met** (`seed` from scratch reproduces full content; counts asserted in test).

## M10 — World sources + streaming

Exit: both `classic` (starter region) and `procedural` (seeded) playable; portals seamless; chunk pacing verified.

- [ ] **M10-T1** `WorldSource` interface + `world.toml` constants (chunk 16³, cell 32 m, AOI 96 m, volume flags, portal format — Godot-agreed). Spec: §4, §13.1/13.5.
- [ ] **M10-T2** Classic starter region: hometown + surroundings + newbie area + Underworld region + first dungeon band (M59-inspired layouts, NOT copies); volume flags; portal remaps + snapshot swaps (no-loading-screen guarantee testable via session continuity). Spec: §4, D1.
- [ ] **M10-T3** Procedural generator v1: deterministic seeded terrain (versioned algo id), same cell/volume/flag pipeline; client-regenerable from `world{mode,seed,version}`. Determinism test (same seed → identical cells). Spec: §4.
- [ ] **M10-T4** Chunk streaming: `218` pacing (≤N/tick/session), `world_ready` barrier integration test, reconnect-mid-stream recovery, bandwidth measurement vs low-end budget. Spec: §6.3, §7.1.
- [ ] **M10 exit criteria met** (walk classic→portal→dungeon with zero session breaks, traced in test).

## M11 — Keycloak integration + client handshake

Exit: real OIDC login → WS → world against configured realm; admin role enforced.

- [ ] **M11-T1** Realm/client config as code (realm export JSON in repo `deploy/keycloak/`): public client, PKCE S256, loopback redirect, `openid profile email offline_access`, `vox-admin` client role, token TTLs (STILL OPEN in §13.4 — decide here: propose access ~5 min). Spec: §11, §13.4.
- [ ] **M11-T2** Backend JWKS validation hardening: cache TTL + rotation backoff, `iss/aud` pinning, pre-auth per-IP rate limits, expired/misissued → `202 error{session_expired}`. Adversarial tests (none-alg, wrong iss, expired, garbage). Spec: §11.
- [ ] **M11-T3** Admin path: `vox-admin` claim → WS admin role + `voxilian admin` CLI parity (kick/ban/save-now/spawn/give-logged). Negative tests (non-admin rejected). Spec: §10, §11.
- [ ] **M11 exit criteria met** (end-to-end login against dev Keycloak container → enter_world → reauth → expiry → grace → disconnect).

## M12 — Ops hardening + load + launch readiness

Exit: prod compose deployable; outage/shutdown behaviors demonstrated; load gates pass.

- [ ] **M12-T1** Prod compose + GHCR: prod profile verified (external PG DSN, migrate one-shot, TLS termination per §13.6 decision — STILL OPEN, decide here), backup/PITR confirmation recorded in §13.6 (spec edit). Spec: §10.
- [ ] **M12-T2** PG-outage + shutdown drills: `/readyz` flip test, login/critical-op rejection test, 60 s movement grace test, SIGTERM sequence test (flush deadline honored), panic-bypass recovery test (CAS invariants hold). Spec: §10.
- [ ] **M12-T3** Load harness + gates: botnet (random-walk + attack + chatter) measuring tick p99, AOI bytes, saver lag, queue saturation on min-spec-shaped budget; gate numbers recorded; sharding decision data pack. Slow-client + reconnect-storm tests. Spec: §12.
- [ ] **M12-T4** Launch checklist: seed counts, realm export, runbook (migrate/rollback, ban/kick, snapshot-now, log triage), `docs/` updated, spec version bumped to v1.0-frozen.
- [ ] **M12 exit criteria met** (all prior milestones `[x]`; soak 24 h bots with zero item/coin divergence in ledger reconciliation).

---

## Task index (for LLM dispatch: one row = one session)

| ID | Depends on | Touches (typical) |
|---|---|---|
| M0-T1…T4 | — | repo root, compose, CI |
| M1-T1…T4 | M0 | `migrations/` only |
| M1-T5…T7 | M1-T1…T4 | `queries/`, `internal/store` |
| M2-T1…T5 | M0 | `internal/proto`, `client/tests` fixtures |
| M3-T1…T5 | M1-T5, M2-T1…T3 | `internal/gateway`, `internal/session` |
| M4-T1…T4 | M3-T1 | `internal/sim`, `internal/world` |
| M5-T1…T5 | M4-T1…T2 | `internal/sim` (combat/vitals/death) |
| M6-T1…T3 | M5-T1…T4 | `internal/sim` (progression) |
| M7-T1…T3 | M4, M1-T5 | `internal/sim`, `seed/` |
| M8-T1…T2 | M3, M4-T3 | `internal/sim`, `internal/store` |
| M9-T1…T8 | M1 | `seed/` + validator |
| M10-T1…T4 | M4 | `internal/world`, `seed/world` |
| M11-T1…T3 | M3-T2 | `deploy/keycloak`, `internal/auth` |
| M12-T1…T4 | all | compose, docs, runbook |

# Voxilian Backend — Implementation Plan (v1.2)

> Source of truth for WHAT: `docs/backend-spec.md` (v0.3.6).
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
- [ ] **M0 exit criteria met** (pre-M1 meaning: image builds; default `compose up` brings postgres 18 to healthy with nothing depending on stubs; migrate/app run profile-gated; CI green. Full-stack `up` gates on M1-T8/M3.)

## M1 — Migrations + CAS store layer (first per reviewer order)

Exit: all §8 tables (incl. prototype catalogs) exist via goose; `Store` covers CRUD + CAS per aggregate; stale-write + CI-migration checks green.

- [ ] **M1-T1** Migration `0001`: `citext` extension + `accounts` + `characters` (+ partial unique indexes `chars_acct_slot_uidx`, `chars_name_uidx` via `CREATE UNIQUE INDEX ... WHERE deleted_at IS NULL`). Spec: §8.
- [ ] **M1-T2** Migration `0002`: prototype catalog tables — `spell_protos`, `skill_protos`, `item_protos`, `mob_protos`, `shop_listings` (stable INTEGER IDs `CHECK 1..65535`, symbolic keys, versions; spec §8.2). Catalogs BEFORE their dependents so FKs are created inline, never via later ALTERs.
- [ ] **M1-T3** Migration `0003`: `character_spells`, `character_skills` (+PKs, FKs → catalogs, ability CHECK 1–99, stat CHECKs 1–50 on characters). Spec: §8.
- [ ] **M1-T4** Migration `0004`: `item_instances` (+`revision`, proto FK → `item_protos`), `item_locations` (all 5 kinds, full per-kind CHECKs, self-containment), `corpses`, `banks` (+`revision`). Spec: §8.
- [ ] **M1-T5** Migration `0005`: `ledger` (`num_nonnulls` CHECKs), `kills`, `bans/mutes`. Spec: §8.
- [ ] **M1-T6a** sqlc: accounts/characters (+slot-claimed create → map unique-violation to `slot_occupied`/`name_taken`). Spec: §8.
- [ ] **M1-T6b** sqlc: spells/skills get/set, item+location txn helpers, corpses, banks. (All catalog access lives in T6d.) Spec: §8.
- [ ] **M1-T6c** sqlc: ledger/kill appends, bans/mutes. Spec: §8.
- [ ] **M1-T6d** sqlc + store: catalog registry — load-all per table + transactional upsert (stable ID + version rules, §8.2). This is the ONLY write API `voxilian seed` may use. Spec: §8.2.
- [ ] **M1-T7a** `Store` CAS: character aggregate (root CAS → child spells/skills in same txn). Stale-revision test. Spec: §8.1, D7.
- [ ] **M1-T7b** `Store` CAS: item aggregate (root CAS → location row in same txn). Stale-revision test. Spec: §8.1.
- [ ] **M1-T7c** `Store` CAS: bank balance + `Store` interface assembly. Stale-revision test + metric. Spec: §8.1.
- [ ] **M1-T8** `voxilian migrate up/down/status` wired to embedded migrations (one-shot container compatible) + CI migrate-up-against-service-PG check (moved here from M0). Spec: §8, §10.
- [ ] **M1 exit criteria met** (migrations apply cleanly to fresh PG 18; CAS suites green; CI migration check green).

## M2 — Binary protocol + golden vectors

Exit: every §6 opcode encodes/decodes both sides (Go done; Godot side stubs + fixtures ready for client plan); fuzz + golden suites green.

- [ ] **M2-T1** Codec core (`internal/proto`): envelope `[opcode|msg_version|seq|tick|payload]`, LE integers, `string`/`array`/`cell`/`pos`/`angle` primitives, 64 KiB frame cap, string/array caps (1024; chat 512; accessToken 8 KiB), `msg_version` trailing-skip, `[u16 entryLen]` entry framing. Round-trip tests. Spec: §6 framing.
- [ ] **M2-T2** Opcodes `100/101/200/201/202` (hello/welcome/reauth/reauth_ok/error) + `216/217/219` (character_list/op, world_ready) + `121–126` (char CRUD/ack/leave). Round-trip tests. Spec: §6.1, §6.2.
- [ ] **M2-T3a** Intents codec `102–120` + `126` (incl. `inputSeq`/`yaw`, fixed-`u32` `105 use`, vendor+stable-listing `114 buy`). Round-trip tests. Spec: §6.3.
- [ ] **M2-T3b** Entity/stat codec `203–210`, `213–215` (incl. `lastProcessedInputSeq`, entryLen framing). Round-trip tests. Spec: §6.3.
- [ ] **M2-T3c** Container codec `211/212/218/220` (`216` belongs to M2-T2) + FREEZE the `211` entry layout here: document every field+width in §6 first (spec-edit commit), then implement. Round-trip tests. Spec: §6.3.
- [ ] **M2-T4** Golden binary fixtures: checked-in hex vectors under repo-root `testdata/protocol/` for ≥1 message per opcode; Go decode test; fixture format README for the Godot client plan. Spec: §12.
- [ ] **M2-T5** Fuzz + robustness: Go fuzz targets per decoder; malformed/truncated/oversized corpus tests; unknown-opcode and unknown-trailing-bytes tolerance tests; `seq`/`inputSeq` wraparound tests (modulo-2³² arithmetic). Spec: §12.
- [ ] **M2 exit criteria met** (all opcodes round-trip; fuzz 60 s clean per target; fixtures committed).

## M3 — Session lifecycle + gateway

Exit: full §6.1 state machine live over real WS; char CRUD end-to-end against PG; takeover/kick/backpressure tested.

- [ ] **M3-T1** WS server + session registry (`sessionID → {sub,accountID,charID?,conn,state,tokenExp}`, indexed by sub+character; per-account lifecycle guard mutex). States + per-opcode permission table enforced; `bad_state` errors. Tests: illegal-state matrix. Spec: §6.1, §7.
- [ ] **M3-T2** Auth hookup (JWT validation behind interface; test fake + real JWKS path stub for M11): `100 hello` → JWKS check → account auto-provision → `200 welcome`; `101 reauth`; 90 s hard deadline (intents rejected post-grace, then disconnect). Tests with forged/expired tokens. Spec: §6.2, §11.
- [ ] **M3-T3** Character CRUD over WS: `121–124` + `217` results + error codes (`name_taken/slot_occupied/bad_stats/bad_budget/character_in_use`); display-name rules per spec §9 (charset/NFC/length/blocklist — decided, no ambiguity left); `126 leave_world` (AOI clear, flush, →AUTHENTICATED); single-txn create per §8.1. Testcontainers tests incl. concurrent double-create race (one wins). Spec: §6.1, §8, §9.
- [ ] **M3-T4** `enter_world` baseline against a fake `BaselineProvider`/`WorldStream` interface (NOT the real world): `CHARACTER_SELECTED` → snapshots + paced `218`s + `220`s → `219 world_ready` barrier verified. Duplicate-login/takeover: kick-old, quiesce/flush-before-baseline, per-account serialization test. M10-T4 swaps the fake for real cells/chunks/vendors. Spec: §6.1.
- [ ] **M3-T5** Backpressure: two-lane bounded outbound queues (coalescible vs critical), `125 ack` flow control, slow-client disconnect + full-resync test, saturation metrics, cell-owner non-blocking rule (no indefinite block; fail-closed disconnect). Load-ish test with fake slow peer. Spec: §7.1.
- [ ] **M3 exit criteria met** (lifecycle fuzz/property test: random opcode sequences never violate state table).

## M4 — Deterministic sim skeleton

Exit: 20 Hz tick loop, cells, server-authoritative movement with reconciliation anchors, handoff, history ring, CAS saver.

- [ ] **M4-T1** Tick loop + cell grid + entity registry. 20 Hz sim, 32 m cells, single-writer-per-cell structure (one process), injectable clock+RNG, per-entity position-history ring (2 s @ 20 Hz). Determinism test (fixed seed → identical trace). Spec: §4, §5.
- [ ] **M4-T2** Movement integration: `102` intents → integrate (walk 3.5/run 7 m/s, vigor gate hook) → `205` with `lastProcessedInputSeq`; server-side collision against a MINIMAL `CollisionWorld` seam owned HERE (tiny interface: solid-check + volume flags at a position); anomaly tripwire (speed/teleport → correct+log). M10's richer `WorldSource` implements/embeds this seam, never replaces it. Reconciliation-anchor unit tests (processed-seq monotonicity). Spec: §5, §6.3, §11.
- [ ] **M4-T3a** Cell ownership + entity handoff: epoch/generation, migrate-queue routing (`202 error{retry}` only on saturation). Handoff tests. Spec: §5.1.
- [ ] **M4-T3b** Cross-cell op infrastructure: `opID` generation/delivery/dedupe/retry (bounded cache) against a SYNTHETIC aggregate operation — no real trade logic here. Spec: §5.1.
- [ ] **M4-T3c** Post-commit reconciliation infrastructure: synthetic durable commit + dropped notification → aggregate reloaded from PG before next mutation. (Real trade semantics belong solely to M8.) Spec: §5.1, §8.1.
- [ ] **M4-T4** Snapshot saver: 60 s dirty-queue + critical write-through paths, aggregate-root CAS writes, stale-write metric, shutdown flush with deadline. Crash-injection tests (kill mid-save → invariants hold). Spec: §8.1, §10.
- [ ] **M4-T5** AOI + presence + inbound limits: cell enter/exit subscriptions, `204/205/206` fanout to subscribers, presence registry updates, per-character inbound token buckets (movement/intent caps). Subscription-churn tests. Spec: §4, §7.
- [ ] **M4 exit criteria met** (movement + handoff race tests green; saver property tests green).

## M5 — Combat + vitals + death

Exit: M59 combat/vitals/death playable against stub mobs; formulas golden-tested.

- [ ] **M5-T1** Offense/defense/hit + weapon tables: `(Off*55)/Def` 10–95%, weapon type/quality, 1 swing/s, vigor costs, 30/hit + ⅓-HP caps, severity text hooks. Golden vectors from `meridian59.md` §7. Spec: §9, meridian59 §7.
- [ ] **M5-T2** Armor/shields/resists: `ModifyDefensePower/Damage`, block/parry/dodge rolls, ±100 resist clip, spell-vs-weapon reduction rules. Golden vectors. Spec: §9.
- [ ] **M5-T3** Spell damage + touch/walls/AoE: `rand*(50+power/2)/99`, wall ticks, touch scaling, Illusionary-Wounds rules, quake falloff; mana/vigor/reagent/karma gates; cast/post-cast timing. Spec: §9.
- [ ] **M5-T4** Vitals/regen/hunger: HP=level caps, mana+nodes, exertion/rest/thresholds, regen tick formulas, stomach decay. Golden vectors + timer tests (fake clock). Spec: §9, meridian59 §4.
- [ ] **M5-T5** Death pipeline: corpse + full droppable drop (PK tags), advancement wipe/halve, Underworld-region respawn, leaving penalties (Stam saves), Portal-of-Life hook; single-txn state+ledger. Crash-during-death test. Spec: §9, §8.1.
- [ ] **M5-T6** Personal/world-light intents: `115 rest`, `116 eat` (hunger/vigor effects), `105 use` (skill/item dispatch incl. Second Wind), `119 safety_toggle`, `117/118 → 209` chat (+channel rules, length caps, rate limits). Owner of these opcodes: this task, no other. Spec: §6.3, §9.
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

- [ ] **M9-T1** Seed pipeline: file format + validator (CHECK-mirroring) + `voxilian seed` built ON the M1-T6d registry API (no direct pgx outside `store`) + idempotent version-ruled upsert. One sample school to prove it. Spec: §8.2 (new), §10 seed bullet.
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
- [ ] **M10-T4** Real baseline integration: swap M3's fake `BaselineProvider` for real cells/chunks/vendors; `world_ready` barrier + reconnect-mid-stream recovery + bandwidth-vs-low-end-budget measurement. Spec: §6.1, §6.3, §7.1.
- [ ] **M10 exit criteria met** (walk classic→portal→dungeon with zero session breaks, traced in test).

## M11 — Keycloak integration + auth hardening (backend-only; Godot PKCE handshake lives in a separate client plan)

Exit: E2E login → WS → world against configured realm using a headless test OAuth client; admin role enforced.

- [ ] **M11-T1** Realm/client config as code (realm export JSON in repo `deploy/keycloak/`): public client, PKCE S256, loopback redirect, `openid profile email offline_access`, `vox-admin` client role, token TTLs (decide here: propose access ~5 min; record decision in spec §13.4 via spec-edit commit). Spec: §11, §13.4.
- [ ] **M11-T2** Backend JWKS validation hardening: cache TTL + rotation backoff, `iss/aud` pinning, pre-auth per-IP rate limits, expired/misissued → `202 error{session_expired}`. Adversarial tests (none-alg, wrong iss, expired, garbage). Spec: §11.
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
| M3-T1, T2, T5 | M1-T8 (migrate CLI), M2-T1, M2-T2 | `backend/voxilian/internal/{gateway,session}` |
| M3-T3 | M1-T7a (character CAS), M2-T2 | `backend/voxilian/internal/{gateway,session}` |
| M3-T4 | M1-T7a, M2-T3b, M2-T3c, M3-T3 | fake baseline + barrier (real world in M10-T4) |
| M4-T1, T2, T5 | M2-T3a/b, M3-T1 | `backend/voxilian/internal/sim` |
| M4-T3a…c, T4 | M1-T7a…c (CAS), M3-T1 | `backend/voxilian/internal/sim`, `internal/store` |
| M5-T1…T6 | M4-T1, M4-T2 | `backend/voxilian/internal/sim` (combat/vitals/intents) |
| M6-T1…T3 | M5-T1…T5 | `backend/voxilian/internal/sim` (progression) |
| M7-T1 | M9-T1 (seed pipeline) + M1-T6b | `backend/voxilian/internal/sim`, `seed/` fixtures |
| M7-T2a…c, T3, T4 | M4-T1…T3a, M1-T7b | `backend/voxilian/internal/sim` |
| M8-T1, T2 | M1-T7a…c, M3-T1, M4-T3b/c | `backend/voxilian/internal/sim`, `internal/store` |
| M9-T1 | M1-T5 (catalog tables) + M1-T6d (registry API) | `backend/voxilian/seed/` + validator |
| M9-T2…T13 | M9-T1 | `backend/voxilian/seed/` content only |
| M10-T1…T2c | M4-T1 | `backend/voxilian/internal/world`, `seed/world` |
| M10-T3 | M10-T1 | generator only |
| M10-T4 | M3-T4 (fake baseline), M10-T1…T2c | `backend/voxilian/internal/{gateway,world}` |
| M11-T1…T3 | M3-T2 | `backend/voxilian/deploy/keycloak`, `internal/auth` |
| M12-T1…T4 | all | compose, docs, runbook |

## Appendix A — C→S opcode ownership (every gameplay opcode has exactly one owner)

| Opcode(s) | Owner task | Notes |
|---|---|---|
| 100/101 hello/reauth | M3-T2 (+M11-T2 hardening) | auth plane |
| 102 move | M4-T2 | intents → integration |
| 103/104 attack/cast | M5-T1…T3 | combat plane |
| 105 use, 115 rest, 116 eat, 119 safety, 117/118 chat | M5-T6 | personal intents |
| 106/107/108/109 get/drop/put/give | M7-T4 | world items |
| 110–113 offer/counter/accept/cancel | M8-T1 | trade machine |
| 114 buy | M8-T2 | vendor listings |
| 120 respawn_ack | M5-T5 | death pipeline |
| 121–124 char CRUD, 126 leave | M3-T3/M3-T4 | lifecycle |
| 125 ack | M3-T5 | flow control |

## Plan history

- v1.2: review pass — migration order (catalogs 0002), M1-T6d registry API, M2 opcode split fix, `CollisionWorld` seam, corrected M3/M9 deps, backend-only M11, bounded soaks, (+spec v0.3.5: INTEGER IDs, numeric mob/vendor IDs, `entityEntry.proto`, seed versioning, catalog cache, exact display names).
- v1.1: review pass — working-directory rule, Dockerfile task, M0/M1 dep fix, catalog-table decision (+spec §8.2), fake-vs-real baseline split, AOI task, opcode matrix, task splits (M1/M2/M4/M7/M9/M10), corrected deps, Jala phase-2 note, backend-only M11, bounded soaks, display-name decision.

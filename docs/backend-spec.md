# Voxilian Backend SPEC (v0.2 — documentation only, no implementation)

> Status: DRAFT for discussion. Normative keywords: MUST / SHOULD / MAY.
> Companion doc: `docs/meridian59.md` (game-mechanics reference, source of all
> numbers/formulas below). Nothing here is implemented yet.

## 0. Locked decisions

| # | Decision | Value |
|---|---|---|
| D1 | World model | **Seamless + embedded interiors**: one continuous overworld; dungeons / Underworld / guildhalls are embedded regions/interiors in the same coordinate space (separate coordinate bands or y-layered volumes), NOT boxed instances |
| D2 | Transport | **WebSocket** (Godot 4.7.2 client). Single WS per session, JSON envelopes for MVP (binary later if profiling demands) |
| D3 | Topology | **Single world process** for MVP. Sessions, presence, and rate limits are in-process memory. No Redis, no external bus. Spatial workers are a later scale-out, designed for but not built |
| D4 | Persistence | **Snapshot + write-through**: PG is authoritative; sim keeps hot state in memory, snapshots every N sec + write-through on critical events (death, trade, logout, guild/faction change) |
| D5 | DB stack | **PostgreSQL 18 + pgx + sqlc + goose**. No other datastore. Current pinned versions in §2 |

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
- Coordinates: world units in meters, float64 server-side; voxel chunk size
  and origin defined by `internal/world` and shared with the Godot client
  (single source: `world.toml` or generated constants — DECISION NEEDED §13.1).
- Spatial index: uniform grid cells (e.g. 32 m; tune) → each cell has an owner
  worker (MVP: all local). Neighbor lookup for melee/AoE/AOI.
- AOI/interest: per-session subscription = cells within view radius R
  (DECISION NEEDED §13.2; start R ≈ 96–128 m) + interior volume override.
  Gateway sends: full snapshot on cell-enter, then deltas at tick; entity
  despawn on cell-exit. MUST throttle: movement states ≤ 10 Hz per entity in
  AOI; combat/vital events immediate.
- Embedded interiors: dungeons/Underworld/guildhall volumes flagged
  `INTERIOR`; entry by walking through portal volumes (position continuity
  preserved — seamless feel), NOT by teleport RPC, except death-respawn and
  admin summons (explicit, logged).
- Portals/doors/locks (M59 shatter-lock, guildhall keys) are volume
  edge-rules evaluated by sim.

## 5. Sim loop & tick rates (ported from M59, adapted)

| Loop | Rate | Notes |
|---|---|---|
| Movement integration + cell handoff | 20 Hz | Server-authoritative; client sends intents ≤ 10 Hz, sim clamps speed (sprint = M59 haste/run rules) |
| Mob AI (chase/move/attack decision) | chase 6–7 Hz equivalent, move/attack 1 Hz (M59: 150 ms / 1500 ms / 1000 ms) | Per-mob staggered timers, not global lockstep |
| Melee/ranged attacks | max 1 swing/s per attacker (M59 `IsOkayAttackTime`) | Hit `(Off*55)/Def` 10–95%; damage + caps per `meridian59.md` §7 |
| Spell casts | per-spell `cast_time` + 2 s post-cast | Mana/vigor/reagent/karma gates per spec |
| HP/mana/vigor regen | event-driven timers per entity (M59 `CalculateHealthTime/ManaTime`) | Same formulas; sanctuary ×2/×3; faction regen phase 2 |
| Advancement/HP-gain rolls | on kill events | Same highmark math; write-through to PG on +1 HP / +1% milestone |
| Snapshot saver | every 60 s dirty-entities + on critical events | Write-through: death, trade accept, logout, char create, guild/faction change (phase 2) |

- MUST: sim uses injectable clock + RNG for deterministic tests.
- MUST: all damage/rolls happen server-side; client-sent damage values are
  rejected (anti-cheat carries M59's buffed-Max halving, PK loot tags, reagent
  checks).

## 6. WebSocket protocol (v0 envelopes, JSON)

- Connect: `wss://host/play` → `{hello, clientVersion, token}` → server
  `{welcome, serverTick, worldConstants{chunk, aoiRadius, tickRates}}` or
  `{error}`. Token = opaque session token issued at login (DECISION §13.4),
  validated against the in-memory session registry, mapped to
  account/character loaded from PG.
- C→S intents: `move/{dir+flags}`, `attack/{target}`, `cast/{spell,target}`,
  `use/{skill/item}`, `get/drop/put/give`, `offer/counter/accept/cancel`,
  `buy`, `rest/stand`, `eat`, `say/say_group`, `safety_toggle`, `respawn_ack`.
  Unknown/rate-limited intents → `{error}` (no disconnect on first offense).
- S→C deltas: `cell_snapshot`, `entity_{create,move,remove}`, `stat` (M59
  `{value,min,max,curmax}` shape — reuse for HUD), `stat_group`, `message/said`,
  `effect`, `inventory_delta`, `offer_update`, `trade_result`, `death`,
  `respawn`. Every S→C carries `tick` + monotonic `seq` per session for
  ordering; client acks highest applied `seq` for resync decisions.
- MUST: server reconciles movement (client predicts, server corrects with
  authoritative position when divergence > epsilon).
- SHOULD: protocol version negotiated at hello; breaking changes bump
  `protoVersion` and reject old clients with a clear error.

## 7. In-memory ephemeral state (no external store)

Sessions, presence, and rate limits live in gateway-owned in-memory
registries (guarded maps / `sync.Map`; single process, no cross-instance sync).

- Sessions: `token → {accountID, charID, connectedAt, lastActivity}` with
  sliding expiry (e.g. 30 min; refresh on activity). Logout/death do NOT rely
  on it. Token hashes stored, not raw tokens.
- Presence: `charID → {conn, currentCells, heartbeatAt}` (heartbeat 15 s,
  sweep every 30 s). AOI cell sets derived from sim positions, deleted on
  disconnect.
- Rate limits: per-character token buckets in memory (movement/intent caps).
- Cross-cell fanout: in-process channels from sim to gateway (same shape a
  future bus would carry, so sharding later only swaps the transport).
- Restart semantics (accepted tradeoff of D3): sessions drop on restart —
  clients re-login; presence rebuilds on connect; authoritative sim state
  restores from PG snapshot + write-through ledger. MUST document this in the
  client reconnect flow (`{error: session_expired}` → re-login → resync).

## 8. PostgreSQL schema (authoritative; migrations via goose, queries via sqlc)

Tables (D4; M59 property names in parens where ported):
- `accounts(id, email CITEXT UNIQUE, password_hash, created_at)`; Argon2id
  hashes (PHC string), never plaintext.
- `characters(id, account_id FK, name CITEXT UNIQUE, gender, face JSONB,
  might/intellect/stamina/agility/mysticism/aim SMALLINT,
  karma INT, hometown TEXT, pos POINT/double[3], vitals JSONB
  {hp,base_max,max,mana,max_mana,vigor,threshold,stomach},
  advancement JSONB {adv_points, adv_timer_due, gain_chance, school_casts},
  flags INT (murder/outlaw/safety/pk bits), created_at, updated_at,
  deleted_at NULL)` — soft-delete keeps corpses/audits consistent.
- `character_spells(character_id, spell_id, ability SMALLINT 1–99, atrophy_flag BOOL)`,
  `character_skills(...)` — PK(char, id).
- `items(id, character_id NULLABLE FK (NULL = in-world/corpse), proto TEXT,
  slot TEXT, qty INT, hits INT, enchants JSONB, pos NULLABLE, created_at)` —
  single-owner invariant enforced in SQL + app-level transactions.
- `corpses(id, character_id, pos, created_at, expires_at)` + `corpse_items`.
- `banks(account_or_char, system TEXT, balance BIGINT)` (two systems like M59:
  Tos/Jasper-shared vs Kocatan — rename to world regions later).
- `vaults(character_id, region TEXT, item_id)` (item storage, fee log in ledger).
- `ledger(id, kind, actor, counterparty NULL, amount/qty, item_id NULL,
  created_at)` — append-only money/item movements (trade/bank/vault/loot).
- `kills(id, killer_kind, killer_id, victim_kind, victim_id, pos, at)` for
  advancement audit + karma/justice phase 2.
- `bans/mutes` minimal for admin MVP.
- Indexes: characters(account_id), items(character_id), corpses(expires_at),
  ledger(actor, at), kills(victim, at). CHECK constraints on stat/ability
  ranges (1–50 creation / 1–99 ability) so bad sim code fails loudly.
- sqlc: `queries/*.sql` → `internal/store/gen/`; migrations embed via
  `go:embed`; `voxilian migrate up/down/status`.

## 9. Gameplay services (what sim MUST enforce; numbers in `meridian59.md`)

- Creation: validate 6×(1–50) + sum ≤ 200 + 45-pt ability budget (L2=25 else
  10); grant Blink + Mace + 500 (+leaving-newbie-zone package); karma seed.
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
- `compose.yaml` (dev): `postgres:18-alpine` + `voxilian` build target;
  named volume; healthcheck; `voxilian migrate up` as init step.
  Prod deployment target: DECISION NEEDED (§13.6).
- Observability: `/healthz` (PG check + sim liveness), `/readyz` (sim loaded),
  `/metrics` (ticks, AOI fanout, intent rates/errors, saver lag, WS sessions);
  structured slog with `tick`, `cell`, `charID` fields; panic → supervised
  restart with sim snapshot-on-shutdown best-effort.
- Admin (cobra `voxilian admin ...` + WS admin role): create account/character,
  grant/revoke, kick/ban, save-now, spawn/teleport (logged), give (logged,
  dev-only flag).

## 11. Security

- Passwords Argon2id; session tokens 256-bit CSPRNG, only hashes kept in the
  in-memory registry; WS requires TLS in prod (terminate at proxy or Go —
  DECISION §13.6).
- Authoritative sim (§5 anti-cheat); per-intent in-memory rate limits;
  movement speed/teleport anomaly detection → correct + log, ban on repeat.
- No secrets in repo; `.env` local only; PG least-privilege role for app
  (migrate role separate).

## 12. Testing

- Unit: formulas (hit/damage/regen/advancement/learn-points) with golden
  vectors from `meridian59.md`; property tests for caps/clips.
- Integration (testcontainers PG 18): creation, trade atomicity,
  death/corpse/respawn, snapshot restore, double-accept race.
- Load: bot harness (N clients random-walk + attack) measuring tick p99,
  AOI fanout bytes, saver lag — gates sharding decision with data.

## 13. Open questions (please decide together)

1. **Chunk/cell constants**: voxel chunk size (16³?), sim cell size (32 m?),
   AOI radius (96–128 m?), portal volume format — need Godot-side agreement
   before protocol freeze.
2. **Movement authority details**: client intent rate (10 Hz?), max speeds
   per state, correction epsilon, lag-compensation for melee (none MVP?).
3. **Characters/account limits** (e.g. 4/account?), name rules, deletion.
4. **Session token format + expiry**, password-reset flow (out of scope MVP?),
   age/rating handling.
5. **World authoring**: hand-built starter region vs procedural seed; format
   (Tiled/custom → `world.toml`?); who owns the map pipeline.
6. **Prod target**: single VPS + compose vs managed PG vs k8s; TLS
   termination; backups/PITR for PG.
7. **MVP content scope**: which spell schools / skills / mobs ship first
   (proposal in §9: 2 schools × 4 spells, 4 weapon skills + dodge/block/parry,
   6 mobs) — confirm or cut further.
8. **Protocol encoding**: stay JSON for all of MVP, or define the binary frame
   format now for movement deltas?

## 14. Version history

- v0.2: drop Redis; sessions/presence/rate-limits in memory; PG-only stack.
- v0.1: initial spec (seamless + embedded interiors, WebSocket, single
  process, snapshot + write-through, PG 18 + Redis 8).
# Meridian 59 — Game Mechanics Research (for Voxilian clone)

> Source: vendored open-source tree in `Meridian59/` (GPLv2, © Andrew & Chris Kirmse).
> License note: code is GPLv2 — **copyleft**. Do not copy KOD/C verbatim into a
> closed backend. Reimplement mechanics from this spec; keep this doc as a
> clean-room design reference. “Meridian” is a registered trademark — use a
> different name (Voxilian is fine). Game content (art/rooms/audio) is **not**
> included in the open-source release.
>
> Researched 2026-09-03 against `Meridian59/{manual,doc,kod,blakserv,resource}`.
> Paths below are relative to `Meridian59/` unless noted. KOD = Blakod gameplay
> scripts (`kod/**/*.kod`, ~1232 files); `blakserv/*.c` = generic VM/server;
> `clientd3d/` = dumb renderer; `doc/design/*` = authoritative design notes.

---

## 1. What Meridian 59 is (scope for the clone)

- First 3D graphical MMO (1996, 3DO/Archetype). Top-down-ish first-person,
  flat-shaded 2.5D rooms (DOOM/BSP style), sprites for actors, tile grid.
- Server-authoritative: client renders + sends intents; **all rules live on
  the server** (`blakserv` VM + `kod` game code). Ideal model for Go backend +
  Godot voxel client.
- No classes. Skill/level-less in the modern sense: 6 attributes fixed at
  creation, ~150 spells in 6 schools + weapon skills, all 1–99% improved
  **by use**. HP (“level”) grows by killing. Heavy PvP / guild / faction /
  justice / death-penalty game, not a quest-rail game.
- World: ~265 rooms (`resource/rooms/*.roo`): 5 cities, wilderness grid,
  dungeons (Orc Caves, Necropolis/Brax, sewers, crypts), temples, guildhalls,
  Underworld (death), newbie zone (Raza).
- Session footprint is tiny by design (1995 <9600 bps target, 1 move/sec
  throttle). A Go server + Godot client can easily exceed fidelity.

### 1.1 Code map (where each rule lives)

| Area | Location |
|---|---|
| Design truth | `doc/design/Mechanics/{stats,combat,weapons,monsters,treasure,karma,faction bonuses,logoff,rooms}.htm`, `doc/design/Backstory/gods.htm` |
| Server manual (arch/protocol, **not** game rules) | `manual/chap{1..5}.tex`, `manual/app{a,b,c}.tex`, `doc/{protocol,general,overview}.tex/.txt` |
| Player logic (12.6k lines — HP/mana/vigor/creation/death/advance) | `kod/object/active/holder/nomoveon/battler/player.kod` |
| Player networking | `.../battler/player/user.kod` |
| Monster base + AI (5.8k lines) | `.../battler/monster.kod` |
| Monsters (66 files) | `.../battler/monster/*.kod` + `towns/ temples/ factions/ council/ ...` |
| Spells (~150) | `kod/object/passive/spell/*.kod` + `atakspel/ boltspel/ touchatk/ persench/ roomench/ walspell/ multicst/ boon/ dmspell/ jala/ hinder/` |
| Skills | `kod/object/passive/skill/{stroke,profic,parry,dodge,block,disarm,secwind,assess}.kod` |
| Weapons/armor/shields | `kod/object/item/passitem/{weapon,armor,defmod/shield}.kod` |
| Constants (schools, IDs, flags, damage types) | `kod/include/blakston.khd` (2963 lines) |
| Creation budget, char-info packets | `kod/util/system.kod`, `kod/include/blakston.khd` |
| Tuning knobs | `kod/util/settings.kod` |
| Loot tables (~40) | `kod/object/passive/trestype/*.kod` |
| Spawn/placement (~60 wilderness/dungeon wrappers) | `kod/object/active/holder/room/monsroom/*.kod` |
| Rooms | `kod/object/active/holder/room*.kod`, `resource/rooms/*.roo` (265) |
| Guilds / factions / quests | `kod/object/passive/guild*.kod`, `kod/util/parlia.kod`, `factgame*/`, `questengine.kod`, `quest*.kod` |
| Generic engine (no game rules) | `blakserv/*.c`, `blakcomp/` (KOD compiler), `roomedit/` |

Key insight for Voxilian: M59 cleanly separates **engine** (movement, timers,
persistence, packets) from **game code** (KOD). Mirror that: Go backend owns
sim + persistence; Godot owns voxels/render/input only.

---

## 2. Character creation

Handler: `player.kod:PlayerNewCharInfo`, offer packet `system.kod:SendCharInfo`.

- **Gender/face:** M/F + 5 face parts + hair (14) / skin (4) variants. Cosmetic
  only. For voxels: palette/face-seed equivalent is enough for MVP.
- **6 attributes, each 1–50, sum ≤ 200.** Invalid packet → joke punishment
  char (3/1/4/1/5/9). Order: Might, Intellect, Stamina, Agility, Mysticism, Aim.
- **Spells/skills budget: 45 points.** Only level ≤ 2, `OfferToNewCharacters`
  spells/skills offered. Cost: level 2 = 25 pts, else 10 pts. Exceeding 45 →
  hacker path, grants nothing. Free default: `Blink` spell only, no free
  skills.
- **Karma seed:** Qor-only start −20, Shalille-only +20, both/neither 0
  (`piKarma` stored in hundredths, −10000..+10000).
- **Start vitals:** HP 20 / BaseMaxHP 20 / MaxHP 20; Mana = `15 + Myst/5`;
  Vigor 100, rest-threshold 80, exertion 0, stomach 0.
- **Start inventory:** Mace + 500 coins (+1000 + reagents for 5 casts of each
  known spell when leaving newbie zone; +5 apples; +2000 if server uptime > 5h
  anti-farm rule). Newbie zone Raza `RID 1011–1018`.
- **Placement:** hometown room + `Born_year`. No class/race choice.

Clone takeaway: creation is point-buy, not classes. Keep 6 stats + 200 budget +
45-pt ability budget. It produces real build diversity (fighter needs
Might/Aim/Stam/Agi; mage needs Myst/Int; hybrids split).

---

## 3. Attributes (fixed for life; only buffs modify)

Effective = `bound(base + mod, 1, 70)` (`player.kod:GetMight...`, `MAXIMUM_STAT=70`).
Raw display capped at 50. Base stats **never increase by use** — only `*Mod`
via spells (Boons), items, disease. Design: `doc/design/Mechanics/stats.htm`,
`combat.htm`.

| Attr | Governs |
|---|---|
| **Might** | Carry: `BulkMax = WeightMax = 1700 + Might*20` (e.g. 10 → 1900). Melee damage: `dmg += ((100+bound(Might-25,0,40)) * dmg)/100` → +0% ≤25, up to +40% at 65+. Requisite stat + starting % for brawling/hammer/mace/axe. |
| **Intellect** | Learning: initial improve chance `+= ChanceToIncrease * Int/100` (up to +50% at 50). Lowers total-learn-point cost (~20 pts at 50 Int ≈ 2 extra schools). Protects vs atrophy, Illusionary Wounds (`17+(50-Int)/10` dmg), scroll use, Seduce. Requisite for **Jala + Riija**. |
| **Stamina** | HP ceiling `100 + Stamina` (hard cap 150). HP regen time `((125-Stam)*t)/100` (±25%). HP-gain roll difficulty (see §6). % immunity per death skill-loss check (`random > Stam` to lose). Rest tick `1000+30*(51-Stam)` ms. Requisite for **Kraanan**. |
| **Agility** | ~1/5 of defense: `Defense += Agi*4`. Default requisite for **all weapon skills**; Disarm value = Agility. |
| **Mysticism** | Mana pool start `15 + Myst/5` (15–25). Mana regen `150000 + (25-Myst)*1000` ms base. Node meld `((5+Myst)/10)+3` per node (3–8; Fey/Vale double 6–16). ~4/5 of spell attack power; touch-attack `Myst*3/2`; wand hit `bound(Myst+Aim,10,90)`. Default requisite for **spells**; specifically **Qor/Shalille/Faren**. |
| **Aim** | ~1/2 of attack: `Offense += Aim*4`. Requisite + start % for archery/sword/shortsword/fire(bow). Wand hit as above. |

Success formula (all skills/spells), `skill.kod` / `spell.kod` / `stats.htm`:

```text
chance% = ((100 - reqStat) * ability / 100) + reqStat [+ modifiers]
```

Examples: req 10 at ability 20/50/90 → 29/55/91; req 30 → 44/65/93;
req 50 → 60/75/95. Spells clamp 5–95. High stats give high floor, ability
gives ceiling — newbies with 40 Aim still hit sometimes with 20% sword.

---

## 4. Vitals: HP (= level), Mana, Vigor, Food

### 4.1 HP — there is no XP level; HP *is* level

- `GetLevel() == GetBaseMaxHealth()`. Start 20.
- `GainBaseMaxHealth`: `bound(base+amt, 20, 100+Stamina)`, then hard `≤150`.
- Buffed `Max_health` can exceed base (spells/items); `Health` can vampire to
  `2×Max`. `MarkOfDishonor` caps healing to base.
- Monster level vs player BaseMaxHP is the difficulty comparison in
  advancement (`AdvancementCheck`).

### 4.2 Mana

- Start `15 + Myst/5`. Recomputed (`ComputeMaxMana`): base + melded nodes +
  items + enchantments. Standard node `((5+Myst)/10)+3`; Fey/Vale double.
  Myst 25: start 20, +6 per normal node.
- Over-max (boosts) decays at 30 s/tick.

### 4.3 Vigor (stamina bar, 1–200, start 100, rest-threshold default 80)

- 10000 exertion = 1 vigor. Positive exertion spends; negative (rest/food/
  Relay/Invigorate) recovers. Rest ticks only up to threshold; threshold
  10–100 settable, forced 10 under Second Wind.
- Gates: `HasVigor(amount)` required to swing/cast. Fail still costs half.
- Typical costs: melee swing ~0.2, punch 0.5, unarmed 1.0, Disarm 1.0,
  spells default 2 vigor + mana, walls/nodeburst up to 30.
- Rest tick `1000+30*(51-Stam)` ms (≈1.0–2.5 s); full 0→80 ≈ 200 s. Room
  `SANCTUARY` ×2, `TRIPLE_HEAL` ×3. Faction vigor discount exists.

### 4.4 Regen (worked examples)

Constants: `BASE_REGEN_TIME=150000`, `REST_TIME=2500`, `BOOST_DECAY=30000`.

- **HP/tick:** `t = ((200-Vigor)²/6+1000)`, `× (125-Stam)/100`,
  `× 100/bound(MaxHP,40,100)`, minus faction bonus, ±Jala Restorate,
  clamp 1–60 s. Example Vigor 100 / Stam 25 / MaxHP 40 → ~6.7 s per HP;
  20→40 ≈ 133 s (matches “~150 s” comment).
- **Mana/tick:** `t = (150000+(25-Myst)*1000) * 200/bound(Vigor,1,..) /
  bound(MaxMana,1,..)`, minus faction, ±Rejuvenate/ManaFocus, clamp 1–60 s
  (0.5 s min in-room). Example Myst 25 / Vigor 100 / MaxMana 20 → 15 s per mana.
- **Vigor:** rest tick above, +1 vigor/tick while resting.

### 4.5 Hunger

`Stomach` 0–100. Decays `12/1000 ticks` → full→empty ≈ 833 s (~14 min).
Checked only on eat: reject if `stomach+filling > 100` (“too full”).
`EatSomething(nutrition, filling)`: vigor `+= nutrition/10`-ish
(`AddExertion(-10000*nutrition)`), stomach `+= filling`. Foods e.g. Apple
10/24, Bread 20/40, MeatPie 30/50, InkyCap 50/25 (best vigor), Drumstick 9/30,
ChaosFood 200/0 (frenzy ignores stomach). HP-gain path force-feeds 200.

---

## 5. Schools, spells, skills

IDs `blakston.khd`: Shalille 1, Qor 2, Kraanan 3, Faren 4, Riija 5, Jala 6,
DM 7, Weaponcraft(Fencing) 10, Brawling 11. Divisions: Combat (Kraanan +
weapons, req Stam/Might/Aim/Agi), Magic (Shal/Qor/Faren, req Myst),
Lore (Jala/Riija, req Int).

- Spell base: `viMana` (mana), `viSpellExertion` (vigor 0–100),
  `viCast_time` ms trance (Heal 600 … HolyWeapon 30000), `piMinHitPoints`
  (need HP to cast, e.g. Shatter 40, Jig 30), `plReagents` (e.g. Herbs×1;
  bypassed by Reagent Ring charges / immortals), `viOutlaw/viHarmful` flags.
- Skill base: `viSkill_level`, `viChance_to_Increase` (default 25),
  `viskillExertion`, `vbCheck_Exertion`, `vbAutomatic` (passive parry/dodge/
  block still roll).
- Karma per cast/learn: Qor `−10×level`, Shalille `+10×level`, else 0. Opposite
  schools block each other via `KarmaCheck`.
- Teachers are `MOB_TEACHER` monsters (priests, generals, minstrels, Lich).
  No gold cost in code — gate is knowledge/karma/quest (temple L3+ needs
  disciple quest). Weapon teachers: sergeants, smiths (scimitar quest-gated).

### 5.1 Shalille (heal/protect) — teacher sells Forget-Qor 2500

Minor Heal L1 (3 mana, Herbs×1, `1d5+(power+1)/20+karma/20`); Heal; Cure
Poison L2; Cure Disease L3; Major Heal L5 (20 mana, Herbs×5); Hospice (HoT);
Holy Touch L2 (12 mana touch, bonus vs evil); Spiritual Hammer L3 (conjured
weapon); Holy Weapon L2 (30 s dedicate); Detect Evil, Holy Symbol (fear
undead), Seance, Identify L3 (15 s), Rescue L4 (16 mana, 15 s base delay —
teleport to safety), Purge L5 (strip helpful ench, O/H), Mark of Dishonor L4
(block casting, O/H), Forces of Light L4 (room buff), Qor Bane L4 (room hurts
Qor casters), Truce L4 (pacify nature), Umbrella L5 (anti-undead), Dazzle L4
(blind, O/H), Final Rites L5, Purify L4, Reveal L5, Resist Acid/Evil, Portal
of Life L6 (30 mana — corpse portal, mitigates death penalty), Bond L6
(50+1/tick item enchant).

### 5.2 Qor (vile) — teacher sells Forget-Shalille; Swap needs Riija adeptness

Cloak L1 (anti-track), Darkness L1 (darken room), Detect Good, Resist Good,
Acid Touch L2, Fade L2, Karahol’s Curse L2 (battle buff), Unholy Weapon L2,
Defile L3 (0 mana ritual), Enfeeble L3 (drain STR, O/H), Silence L3 (room
mute, O/H, minHP 10), Poison Fog wall L3 (20 mana), Vampiric Drain L3
(10 mana ranged, heal half), Animate L4, Curse Weapon L4, Shadow Rift L4
(30+1/tick teleport), Hold L4 (15 mana paralyze, O/H, no-newbie-offense),
Shalille Bane L4, Splash of Acid L4 (10 mana ranged), Blind L5, Blood
Inheritance L5 (50+1/tick weapon), Death Link L5 (soul room), Invisibility L5,
Nodeburst L5 (1 mana +30 vigor — node overdrive), Swap L5 (random exchange,
O/H), Gaze/Paralyze L6.

### 5.3 Kraanan (battle) — priests + generals + smiths

Create Food/Weapon L1, Glow L1, Relay L1 (give vigor), Bless L2 (accuracy),
Enchant Weapon L2, Haste L2 (run fatigue), Resist Poison L2, Super Strength
L2, Detect Invis L3, Free Action L3 (anti-hinder), Magic Shield L3, Martyr’s
Battleground L3 (room combat buff), Night Vision L3, Shroud L3 (gear vs
magic), Deflect L4 (reflect barrier), Discordance L4, Dispel Illusion L4,
Eagle Eyes L4, Mana Bomb L4 (AoE, O/H), Mend L4 (repair), Armor of Gort L5
(deflect field), Hunt L5 (50+1/tick track via prism, H), Killing Fields L5
(room +dmg bloodlust), Resist Magic L5, Anti-Magic Aura L6 (room shock),
Shatter Lock L6 (50+1/tick guildhall open, auto-outlaw).

### 5.4 Faren (nature/elements) — priests + heretics + wizards + ghost

Light, Mystic Touch (transfer mana), Fog wall, Sweep (gather items), Zap
touch, Bramble Wall L2 (H), Icy Fingers / Touch of Flame L2, Resist Cold/Fire
L2, Heat L3 (room, O/H), Mana Focus L3, Resist Shock L3, Shocking Fury L3
(ranged), Web wall L3 (H), Winds L3 (worsen ranged), Blast of Fire L4
(ranged), Brittle L4 (age weapon, O/H), Explosive Frost L4, Fire Wall L4 (H),
Fireball L4 bolt (7 mana), Earthquake L5 (12 mana room AoE, O/H), Lightning
L5 bolt (10 mana), Ring of Flames L5 (H), Sandstorm L5 (O/H), Shatter L5
(O/H, minHP 40), Lightning Wall L6 (H), Spore Burst L6 (paralyze cloud, H).

### 5.5 Riija (illusion/trick) — monks + assassins; Blink excluded from learn points

Anonymity L1, Blink L1 (15 mana room teleport — the free starter),
Feign Death L1 (minHP 5), Flash L1 (interrupt trance, O/H), Shadow Form L1,
Bait L2 (taunt mobs), Eavesdrop L2, Foresight L2 (reveal victim stats, O/H),
Forget L2 (victim forgets ability, O/H), Illusionary Firewall L3 (fake wall,
H), Illusionary Form L3, Illusionary Wounds L3 (absolute damage, leaves 1 HP,
⅓ cap — see §7), Reflection L3 (mirror summon), Vertigo L3 (O/H), Dement L4
(O/H), Apparition L4 (illus attacker, O/H), Denial L4 (unrealize wounds),
Artifice L5 (random enchant), Morph L5 (turn to beast), Evil Twin L5 (dark
double, H), Elusion L6 (escape), Seduce L6 (control weak mob, O/H), Summon
Cow L6 (joke, 5+1/tick).

### 5.6 Jala (bard songs, room enchants that hinder other schools)

All need HP (`piMinHitPoints` 1–30) + Ruby/Sapphire/Diamond reagents.
Invigorate L1 (room vigor), Mirth/Melancholy L1 (buff/debuff), Truth L1
(hinder Riija, H), Civility L2 (hinder Faren, H), Conciliation L2 (hinder
Kraanan, H), Warp Time L2, Distill L3 (spell→potion), Profane/Sacred Resonance
L3 (hinder Shal/Qor, H), Restorate L3 (room HP regen), Spellbane L3 (block
casting above power, H), Disharmony L4 (hinder Jala, H), Jig L4 (force dance,
minHP 30), Mana Convergence L5 (boost effectiveness), Rejuvenate L5 (room mana
regen), Crystalize Mana L6 (catch mana to crystals).

### 5.7 Weaponcraft & utility skills

Slash L1 (melee hit), Punch L1, ShortSword L2 (Aim), Brawling L2 (Might),
Sword L3 (Aim), Axe L3 (Might), Hammer L3 (Might), Mace (Might, most common
starter), Scimitar L4 (Agi, quest-gated), Parry L4 (passive, needs weapon),
Fire L4 (bow shot, Aim), Archery L5 (Aim), Dodge L1 (passive), Block L1
(passive, needs shield), Disarm L1 (strip weapon), Second Wind L1 (vigor
recovery, HP-gated teacher), Unarmed base, Thrust/Kick/Assess (level-50
sentinel = DM/special, not learnable ranks).

---

## 6. Improvement (the core progression loop — no XP)

### 6.1 Ability % (1–99)

Stored `[num*100+ability]` in `plSpells/plSkills`. Start
`InitialAbility = bound(random(req/2, req)/2, 1, 25)` — higher requisite stat
→ higher start (1–25%). Hard cap 99. Softcap: if `ability > 2*req − 1`,
second-chance `/= 4` (spells) or `/5` (skills).

### 6.2 Learn by teacher (acquire)

`MONSTER_LEARN_GRADIENT=15`. Flow: `PlayerCanLearn == SUCCESS` → `AddSpell/
AddSkill` (initial ability + school totals + advancement points: 2 spells /
4 skills). Fail reasons: already have / no base / karma / impossible /
need-more-% (`10*(need-have)+BASE`). Temple 3+ needs disciple quest.

### 6.3 Improve by use (+1%)

Every successful cast (`CastSpell`) / skill use (`DoSkill`) / weapon swing
(split 60/40 stroke vs proficiency) / dodge-parry-block (`AdvancementCheck`)
calls `ImproveAbility`. Two gates:

1. `GetInitialChance = viChance_to_Increase * (1 + Int/100) + faction + flag`
   vs d100.
2. `GetSecondaryChance = 60 + req − 10*level − TotalLearnPoints(except school)`;
   `factor = bound(2*difficulty − ability + 10, 50, 100)` (difficulty = monster
   level clamped 1–150 else 75 spells / 60 skills); `+= SchoolCasts/level/7`
   (spells); softcap divide; `× AdvancementRate/100`; `bound(1+req/10, 99)`;
   d100 roll → `+1%` + “You advanced…” + reset school casts.

**Rate limit:** `ADVANCEMENT_LIMIT=10` per `ADVANCE_TIMER 15–22 min`; expiry
zeroes points (+ atrophy check if enabled — currently **off**). Weapon swings:
check every 75 swings + swing-count bonus. Hard rooms can `/20` the chance.

### 6.4 Learn points (permission, not currency)

`MaxLearnPoints=16`, `POINTS_SLOPE=7`, `MIN_NEEDED=75`.
`Need = Points*7 + (297 − 16*7) − Int*2*7/5`, scarcity `/3` (prev L1) or `×2/3`
(prev L2); need top-3 % at previous level (`aHigh+bHigh+cHigh`, L1 auto 99s).
16 ≈ L6+L3+L2 (10+4+2) or 2×L6 with Int. Blink + DM excluded. Scry shows debug.

### 6.5 HP gain (kill to earn, must do damage)

`AdvancementCheck`: not safe/arena/illusion; compare monster level vs BaseMaxHP:
`mobLvl > base` → `gain 2` (3 if took-damage + killing blow), roll; `mobLvl+5
> base` → `gain 1` no roll; else 10% “spit” message. Newbie <30 HP +1. Halved
if buffed `Max > 2×Base` (anti-cheese). `×HPGainMultiplier (1)`.
Roll: `highmark=(index+1)*index`, `index=base*(100-Stam)/100` (40 HP/25 Stam →
index 30, highmark 930); `rand(1,highmark) < gain_chance + bound((mob-base)/5,
0,10)` and `base < 101+Stam` → `+1 MaxHP`, heal full, force-feed 200,
`gain_chance = −(base/2) − (50-Stam)/2 if >30`. Stamina is the HP-stat.

### 6.6 Forgetting / atrophy / caps

- **Forget potions** (7 schools, e.g. Shal priest sells Forget-Qor 2500):
  wipe a whole school (+ side effects). **Forget spell** (Riija L2): victim
  loses abilities.
- **Atrophy:** unused entries lose −1 (−2 if no mate) on advancement-timer
  expiry — **disabled** (`IsAtrophyOn=FALSE`). Int protects.
- **Caps:** ability 1–99 (start 1–25); learn 16+Int; advance 10/15–22 min;
  HP 20–150 (`100+Stam`); stats 1–70 (display 50); mana 15+Myst/5 + nodes;
  vigor 1–200 (rest to threshold).

---

## 7. Combat & damage

All KOD (`battler.kod`, `player.kod`, `monster.kod`, `weapon.kod`,
`stroke.kod`, `defmod.kod`). C server is only the VM; D3D client only animates.

### 7.1 Hit roll

```text
chance = (Offense * 55) / Defense, bound 10–95; hit if chance >= d100
```

- Player **Offense** = `Stroke*3 + Prof*2 + Aim*4 + BaseMaxHP*3/2`,
  `+ weapon.ModifyHitRoll + faction + attack_mods + flag%`, bound 1–1000.
  Ranged without line-of-sight halved. 1 swing/sec for all weapons
  (`IsOkayAttackTime 1000 ms`); spells use `PostCast 2 s`.
- Player **Defense** = `Parry*2 + Block + Dodge*3 + Agi*4 + BaseMaxHP*3/2`,
  `+ armor ModifyDefensePower + faction + flag%`, bound 1–1000. Zero components
  if no weapon/shield or `NO_FIGHT/NO_MOVE`, or `CanParry/Block/Dodge` false.
  Miss text weighted over parry/block/dodge/avoid pools.
- Monster **Off/Def** = `3*Level + 60*Difficulty`, bound 1–1500 (can exceed
  player cap — endgame hits hard). Dodge = `Difficulty*10`. Palsy ×3/4.
- Example: Orc (45/6) → 495. Mid player (50/50/40 HP → ~430 off / ~450 def)
  → ~52–60% hit each way. Yeti (170/9) → 1050 — needs buffs/numbers/magic.

### 7.2 Weapon damage

Base types (`weapon.kod`): Bludgeon hit 75 / dmg 4–8 / disarm −5 / spell +0 /
range 2; Thrust hit 125 / dmg 3–8 / disarm +10 / spell −10 / range 3; Slash
hit 0 / dmg 5–11 / disarm 0 / spell −15 / range 2. Quality: Low +0/−1/−5/+5,
High +50/+1/+5/−5, Nerudite +25/+1/+0/+5, plus `HitBonus/DamageBonus`
(enchant). `GetDamage` + attribute mods; 75% chance −1 weapon HP per hit.

Stroke: `dmg = weapon.GetDamage() * DamageFactor/100` (Slash 80, Fire/bow 90),
`+ (Prof+1)*5/100 + ((100+bound(Might−25,0,40))*dmg)/100` (Fire uses Aim).
Unarmed `d4 + factors (Brawling)`. Then `+ attack_mods + faction SoldierShield
dmg` (above caps).

**Caps** (`player.kod`): min 1; `≤ BaseMaxHP/3` if victim `HP < 2×Max` and not
outlaw/murderer (anti-one-shot); **≤ 30/hit** (faction +15% above caps).
Monsters: `bound(dmg,1,..)`, no cap. Severity text: 1–5 nick, 6–15 wound,
>15 damage, `$` slay; 1/3 maxHP forces “damage”.

Order: armor `ModifyDefenseDamage` → `ResistanceCheck` → bonuses → caps →
`LoseHealth`.

### 7.3 Weapons (selection; `weapon/*.kod`, `ranged/bow/*.kod`, `ammo/*.kod`)

Melee: Axe (slash), Hammer (bludgeon), Longsword (thrust), Mace (bludgeon/low,
200–250 hits, starter), Shortsword (thrust/low), Scimitar (slash/high,
350–400), Mystic Sword (thrust/high/magic), Nerudite Sword (+25/+1, 450–500,
shatter 15–30 self), Hunter Sword (slash + vigor drain 2–4 + spell proc,
600), Riija Sword (magic/slash, 700–750), Spiritual Hammer (magic/bludgeon/
holy), Gold Sword, Unique (pierce), Black Dagger (0 dmg, assassin-game only).
Default melee 250–300 hits.

Ranged: `RangedWeapon` (Archery prof, Fire stroke, no parry, `Spell_mod −25`,
damage = ammo + bow bonus, hit degrades with wear, range from bow). Bows:
Bow 100/0, BattleBow +2/17, Longbow 0/100, MagicBow 50, NeruditeBow +2/20
(100–150), PracticeBow −3/30 (400–500), Crossbow 100 (bolts). Ammo dflt 1–5
pierce; Fire 2–6 + fire spell; Silver 2–6 (5–10 vs undead); Nerudite 2–6,
10% 3–8 + shatter.

### 7.4 Armor / shields (`defmod.kod`, `armor/*.kod`, `shield/*.kod`)

`Defense_bonus` 1–50 → `ModifyDefensePower`; `Damage_reduce` → `random(r/3,r)`
capped `dmg−1`; **0 vs pure spell, ×2/3 vs weapon+spell**; 50% chance armor
takes damage per defending hit; `Spell_modifier` penalizes casting.

Leather +50/0/±0 (300–350, WEAP_ALL 5); Chain −50/2/−15 (550–650, thrust 5,
pierce 25, bludgeon 15, slash 20); Scale −100/4/−20 (+heat 3, bludgeon 10);
Plate −200/6/−30 (+heat 4, fire −10 = weak, shock −15 = weak); Nerudite
−150/5/−20 (850–900, fire/shock/cold/acid +20, quake −20, 75% dmg chance,
shatter self); Robe +20/0/+10 (mage gear); Disciple robe +5/+10 (WEAP −10,
fire −20, shock +15); Helm +25/1/−5 (SPELL_ALL +15); Simple helm +20/1/−5
(bludgeon 10); Circlet +5; Ivy +10; Royal shirt +10; Gauntlet +15 (attack).

Shields: `Block = bound(BlockSkill + bonus, 1, 120)`; reduction only on
successful `Block.SuccessChance(+bonus)`. Metal 5/1 (+slash 10); Gold 10/1
(+slash/bludgeon/thrust 10); Knight 15/2 (SPELL −20, pierce 10); Orc 20/2
(pierce 15, holy −20); Guild/Soldier 20/2 (pierce 10); Torch 50 hits only.
250–300 hits (guild 350–400).

### 7.5 Spell damage (base `random(min,max) * (50+power/2)/99` + ManaFocus; resisted unless Absolute)

Fireball 8–12 (7 mana, bolt); Lightning 13–18 (10, bolt); Blast of Fire
15–23 (8, range 4); Explosive Frost 13–25 (8, r4); Shocking Fury 12–20 (8,
r5); Splash Acid 16–25 (10, r3); Vampiric Drain 12–18 + heal half (10+4v,
r4); Illusionary Wounds `17+(50−Int)/10` vs players / `30−diff*2` vs mobs
×power/100, leaves 1 HP, ⅓ cap, **ignores resist** (10+3v, r4); Earthquake
`rand(5,9)×severity×falloff`, full ≤8 sq (12, room AoE); Walls `bound(power/6,
1,16)`/tick; Touch 3–9 base scaled `d/2+(d/2)*power/99+1` (Acid 4–8, Flame
4–9, Holy 3–6 ×2 vs undead ±karma, Icy 5–8, Zap 3–8; 6–12 mana melee);
Nodeburst/ManaBomb mana-based; Slitherbolt 12–19 (DM only).

### 7.6 Resistances (`battler.kod:ResistanceCheck`)

Largest resist + worst weakness among `plResistances`, clipped [−100,100];
`>0: dmg*(100−v)/100`, else vuln amplify. Sources: armor/shield tables,
`ResistanceEnchantment` (`power/2+1`, 1–50, 5–10 min), Fire/Cold/Shock/Acid
rings (50), necklaces/charms, Gort/MShield/Umbrella (defense-mod, not resist
list). Types `blakston.khd`: spell ALL/FIRE/SHOCK/COLD/HOLY/UNHOLY/ACID/QUAKE/
HUNTERSWORD; weapon ALL/NONMAGIC/MAGIC/HIT/BLUDGEON/PIERCE/THRUST/SLASH/WHIP/
CLAW/BITE/STING/ACID/UNARMED/PUNCH/KICK/NERUDITE/SILVER. No armor-penetration
stat exists.

---

## 8. Monsters & AI

Base `monster.kod`; brains `passive/brain/{blind,paralyze,revbrain}` +
`BRAIN_*` (`ORIGINAL 1, REVENANT 2, BLIND 3, PARALYZED 4, GUARD 5,
NONAGGRESSIVE 6, KARMA_AGGRESSIVE 7`). Flags `MOB_*`/`AI_*` (`AGGRESSIVE,
FLEE_FRIGHT, SWITCHALOT, REGROUP, NOMOVE, WIZARD_KILLER, WALKTHROUGH_WALLS,
HYPERAGGRO, OPTIMAL_RANGE, ...`), speeds (`SLOW/AVG/FAST`), timers
(`CHASE 150 ms, MOVE 1500 ms, ATTACK 1000 ms, FREEZE 15 s`), states
(LIMBO/ATTACK/CHASE/WAIT/MOVE). Casters have `piMana` + `MonsterCastSpell`
(chance-gated, e.g. OrcWizard Slitherbolt 40% / Purge 35%). No player-style
mana/vigor otherwise. Design: `doc/design/Mechanics/monsters.htm`.

Formulas: **HP** = `Level` (<40) else `1.2×Level`, `Fuzzy(4)` = 0.75–1.25×.
**Off/Def** = `3×Level + 60×Difficulty` (1–1500). **Dmg** default
`Level/rand(10,15)` else `piMin/MaxDamage`. Hit `Off/Def×55`, 10–95%.

Sample (HP = estimate, Off/Def = formula; loot `TID_*` in `trestype/`):

| Monster (file) | Lvl/Diff/Karma | HP | Off/Def | Attack / AI | Loot / home |
|---|---|---|---|---|---|
| Ant | 40/4/−10 | 48 | 360 | Bite, aggro, wiz-killer, slow | Wimpy-med; Badlands |
| Orc | 45/6/−40 | 54 | 495 | Slash, aggro, flee-fright, switch | Tooth 40%, scimitar/shield/leather; Orc Caves 2500–05 |
| Spider | 50/4/−30 | 60 | 390 | Bite + poison, regroup, aggro | Med-tough; Spider Lair R35 |
| DeathSpider | 75/5/−30 | 90 | 525 | Bite, 75 SPELL_ALL/MAGIC resist | Black-spider; deep caves |
| Centipede | 30/5/+15 | 30 | 390 | Bite, fast | Wimpy-med; wild grid |
| Rats (dusk/snow/giant) | 50/6, 60/4, 30/1 | 60/72/30 | 510/420/150 | Claw, fast, switch | Rat table (money/herbs); sewers |
| Zombie | 55/4 | 66 | 405 | Slash | Zombie (bread/arsenic); Tos/Marion crypts |
| Skeleton (+3 variants) | 75/5 | 90 | 525 | Thrust | Tough; crypts/necropolis |
| Mummy (+black) | 25/2 | 25 | 195 | Newbie-tier | Newbie/mummy; Raza, Marion crypt |
| Troll | 90/8/−75 | 108 | 750 | Aggro | High-human; Vale A5/A6 |
| Scorpion | 55/8 | 66 | 645 | Sting | Med-tough; desert |
| Ent / Evil Ent | 50/4/+40 | 60 | 390 | Whip, nomove, karma-aggro, 10% Lightning | Ent (darkberry/sapphire); FeyForest |
| Frogman | 70/5/+80 | 84 | 510 | Pierce, karma-aggro, 65 WEAP_NONMAGIC | Frogman; Konima jungle |
| Iceperson | 90/8/+75 | 108 | 750 | Pierce+cold, ranged cast | Ice-person; IceCave R750 |
| OrcWizard | 80/7/−60 | 96 | 660 | Bludgeon + Slitherbolt/Purge/KillingFields/ManaBomb/Forget | Orc-wizard; Orc deep |
| AvarShaman | 100/8/+80 | 120 | 780 | Pierce+quake ranged + Hold/ManaBomb/Sandstorm/Shatter/Quake | Avar-shaman; Konima 2110–44 |
| Ghost (Far’nohl) | 200/5/−100 | 240 | 900 | Fast, walls, 25% Fireball, undead, 90 NONMAGIC/−50 MAGIC/HOLY-weak | Ghost; Castle throne R40 |
| Lich (+dead) | 200/9/−100 | 240 | 1140 | Unholy bolt, hyperaggro, walls, switch, wiz-killer; **teacher** | Lich (ivy/plate/mystic/invis/fire); Brax 825–33 |
| Yeti | 170/9/0 | 204 | 1050 | Claw, slow, optimal-range, Discordance, 90 slash/pierce, −45 fire-weak | Very-tough; tundra/icecave |
| Lupogg King / Kriipa / Thrasher / Revenant | 200/9, 160/6, 150/7, 55 | 240/192/180/66 | 1140/840/870/465 | King spit/breath/rumble; Revenant sight 9999, walls, vigor drain | King/dragonfly/thrasher; boss lairs |

NPC roles reuse monster class: `MOB_TEACHER/SELLER/BUYER/COND_SELLER/BANKER/
VAULTMAN/LAWFUL/GUARD`, innkeepers, questers, faction leaders, Justicar clerk.

---

## 9. World, economy, meta systems

### 9.1 Rooms & zones (265 `.roo` + KOD wrappers; IDs `blakston.khd:RID_*`)

Tos 50–76 (starter: bank, Adventurer Hall, Kraanan arena+Watcher, Familiars
inn); Barloque 101–114+970–971 (bar, Justicar court+jail, hall, inn,
merchant/smithy/vault/port, sewers, Assassin HQ R110); Cor Noth 150–156;
Marion 200–205 (+crypt 2600–01); Jasper 350–382 (+sewers, sewer-king 380);
Wilderness A–P grid 511–669 (Vale 532, Faronath 537, Ileria 544, Lake 568,
Cragged 578, Bob 585, Ukgoth 599); Temples Shal R48 / Kraanan R801 / Qor R802
/ Faren Badlands / Riija KE1 R2151; Brax/Necropolis 825–833 (Lich endgame);
Castle Victoria 38–41+850–856 (Princess/Duke, ghost); Orc Caves 2500–10;
Kocatan 2000–15 + Konima jungle 2100–54; Guildhalls 701–715 + GM hall 700;
University 902; Underworld 1; Raza newbie 1011–18; Guests 1001–08; Rentables
10000+.

Room flags: `SAFE_DEATH NO_COMBAT NO_PK GUILD_PK_ONLY NO_MAGIC HOMETOWN LAMPS
SANCTUARY ANTI_MAGIC KILL_ZONE GUEST SAFELOGOFF TRIPLE_HEAL JIG NO_MOB_COMBAT`;
terrain CITY/SHOP/TEMPLE/CASTLE/FOREST/MOUNTAIN/SEWERS/CAVES/LAIR/MYSTICAL/
NECROPOLIS. Inns = hometown + safe-logoff + 2–3× heal + rentals. Shops per
town (apoth/smith/merchant/tailor/bowmaker, markup `MERCHANT_*`, faction 5%
off, Duke murderer-exception). Banks: Tos/Jasper shared vs Kocatan separate;
vaults (item storage, fee): Barloque/Kocatan.

### 9.2 Guilds (`guild.kod`, halls 701–715, creator NPC)

Max 400, min 3 (<3 → warn → disband; 6-min periods, 240 periods ≈ 24 h grace).
Ranks Guildmaster/Mistress, Lieutenant (max 2), Lord/Lady, Sir/Madame,
Apprentice + voting/support/ascension/abdicate. Rent/6 min:
`5 + 10 if secret + 1/member − 2/ally + 5/enemy`, hall cut /10, overdue max 480
periods (~48 h) → disband. War: 50k to exit mutual war, winner 60%,
peace delay 2 h. `GUILD_PK_ONLY` + `AllowGuildAttack` (both need guild/token/
murder/shield); arena guild-vs-guild styles. <30 base-HP death → auto-quit.

### 9.3 Factions (`parlia.kod`, `factgame*/`, Duke/Princess/Rebel NPCs)

Neutral/Duke/Princess/Rebel (max 3). Duke (Akardius): Riija/Qor/Kraanan power/
learn, vigor −30%, buy-as-murderer. Princess (Kateriina): Shal/Faren/Jala
power/learn, def +200, mana-regen −700 ms, 5% discount. Rebels (Jonas
D’Accor): skill learn, hit +125, HP-regen −700 ms, dmg +2. Scaling by
power (tokens + territory 0–1000 each) + intriguing (<30 HP / unfactioned
fractions 20–25%). Attacking same-faction/intriguing boots you.

### 9.4 Justice, PvP, karma

- Enable: `PKILL_ENABLE_HP=30` base-HP (or guild/murderer/outlaw) → `PKILL`
  flag (lockable); server-wide safe (Sacred Haven) flag possible; `SAFETY`
  toggle blocks illegal attacks.
- Rooms: `NO_COMBAT` blocks all; `NO_PK` blocks PvP; `GUILD_PK_ONLY` guild/
  token/murder/shield only; `KILL_ZONE` always; `NO_MOB_COMBAT` no mobs;
  arena + Chaos Night bypass; `NO_MAGIC/ANTI_MAGIC` for spells.
- Flags `MURDERER/OUTLAW` shown in object desc; kills broadcast (“murdered/
  slaughtered”); PK loot tagged 10 min (only PKable can pick); Revenants haunt
  killers (sight 9999, walls, vigor drain); Assassin Game (Roq daggers, 4 gems,
  50% hit, primary/secondary only, Underworld no-penalty); Justicar elected
  6-day cycle (Barloque court 970 + jail 971, 12 pardons murderer→outlaw→free).
- Karma −100..+100 (stored ×100): kill tends toward victim karma (player-kill
  table, /4 for monsters); swing person 10 / monster 7 / vamp 6 / defile 5 /
  minor-heal 1; >75/<−75 very hard. Gates Qor/Shalille. Examples: Lich/Ghost
  −100, Troll −75, Orc −40, Spider −30, Ant −10, Ent +40, Frogman/Avar +80.

### 9.5 Death / ghost / logoff (`player.kod:Killed/ApplyDeathPenalties`, `logghost.kod`)

- Cheap (no drop/penalty, HP=1): arena non-real, prison OutOfGrace,
  `SAFE_DEATH`, frenzy, newbie Raza/honor, Token death.
- Else: corpse at death pos, drop all droppable (PK-tagged), advancement 0,
  gain-chance halved, teleport Underworld (HP 1, Mana 1, Vigor /4 capped 50),
  angel mail (+half mana if not murderer). On leaving (Portal of Life can
  mitigate): outlaw/haunted cleared if full cost; newbie cost /3 else
  `DeathCost%` (default 100) chance −1 MaxHP (Stam save); −1 spell/skill >5
  (−2 if murderer; Stam save each); <30 → quit guild + re-evaluate PK.
- Unsafe logoff leaves ghost 10 min (`LogoffPenaltyGhostTime=600 s`); return
  in time = no penalty, else escalating (mail → best stack → best item →
  =death by 10th; `EquivDeath=10`, decay 1/day); >35% net server loss in
  10 min = amnesty (anti-PK-hit-and-run + router failsafe).

### 9.6 Economy / trading

Currency shillings. `OFFER/GIVE/TAKE/COUNTER/ACCEPT/CANCEL`, `BUY/BUY_ITEMS`
packets. `BUYER` offers, `SELLER` lists (+`COND_SELLER` hidden), template vs
finite stock; `LAWFUL` refuses murderers. Banks (balance/deposit/withdraw;
2 systems), vaults (item storage). Loot economy via `TID_*` weights
(e.g. Orc Tooth 40%, Lich Ivy 20% + wands 5%) + `Item_att_chance/Diff_seed`
enchanted drops (Lich 10/10 vs Orc 5/1).

### 9.7 Quests (`questengine.kod`, `quest/node.kod`, `lore/library.kod`)

Party max 5, active max 100. Types: bring-item/message, kill, kill+bring-
carcass, courier, collect (mushroom/book). Restrictions
(`Q_PLAYER_*`: faction/murder/outlaw/newbie…), prizes (faction join, outlaw
pardon…), penalties. Hinter/quester NPC lists. Quests gate schools (disciple),
scimitar, guildhall locks — not the main progression (kills + use are).

### 9.8 Server architecture (for the Go port)

- **Split:** dumb client (render/sprites/packets) / generic C server (accounts,
  sessions, Blakod VM, timers, GC, persistence) / KOD game code (all rules).
  Port as: Godot = render/input/predict; Go = sim + persist + authoritative
  timers/rooms/AI.
- **Blakod VM lessons:** `Send` (recursive call, depth guard) vs `Post`
  (queued after top-level — fixes NPC-speech ordering); inheritance via
  `propagate`; objects/lists/strings/resources/tables/timers as ID heaps with
  `kodbase.txt` name↔id map for reloads; single-threaded interpreter
  (2678 builtins) — Go can parallelize rooms but keep per-room single-writer.
- **Main loop:** next-timer wait capped 500 ms → epoll/kevent → parse →
  top-level message → fire one timer. Two threads (main + socket). Movement
  throttled 1/s.
- **Sys timers** (`blakserv.cfg [Auto]`): garbage (stop-and-sweep lists→
  objects→strings→timers), save-all (game/strings/dynarscs), Blakod-hour
  (lighting/weather), interface-update, pool-reset.
- **Protocol** (`doc/protocol.txt`, `protocol.khd`): `<len checksum len seqno
  ...>` TCP, epoch seqno vs GC. C→S: MOVE/TURN/ATTACK/SHOOT/CAST/USE/APPLY/
  GET/DROP/PUT/GIVE/OFFER/BUY/LOOK/INVENTORY/SAY/SAY_GROUP/ACTION/USERCOMMAND/
  ADMIN/MAIL/NEWS… S→C: CREATE/REMOVE/MOVE/TURN/CHANGE/LOOK/MESSAGE/SAID/
  EFFECT/PLAYER/ROOM_CONTENTS/INVENTORY/STAT/STAT_GROUP/OFFER/BUY_LIST/
  PLAY_WAVE/MIDI/MUSIC/LIGHT/SECTOR_MOVE/WALL_ANIMATE/ADD_ENCHANTMENT…
  Resources by id (FTP updater `club/` + `packages.txt`), speech raw.
- **`club/` is not guilds** — Win32 auto-updater. Don’t mirror it.

---

## 10. Implications for Voxilian (Go backend + Godot 4.7.2 voxels)

1. **Keep the server-authoritative split.** Godot sends intents (move/attack/
   cast/use/give/offer/buy/say); Go simulates rooms, timers, AI, regen,
   advancement, persistence. Never trust client damage/rolls.
2. **Data model starter tables:** `accounts → characters {6 stats, karma,
   hometown, vitals{hp,base_max,max,mana,max_mana,vigor,threshold,exertion,
   stomach}, spells[{id,ability}], skills[{id,ability}], learn{points,adv_pts,
   adv_timer,school_casts,gain_chance}, flags{murder,outlaw,safety,pk}, guild,
   faction, bank, vault, corpse_id}`, `rooms {id,flags,terrain,region,exits,
   enchants{walls,songs,auras}}`, `mobs {proto{level,diff,karma,atk_type,
   resists,spells,loot_tid}, state{hp,pos,target,timers}}`, `items {proto{slot,
   type,quality,hit,dmg,def,reduce,spellmod,resists}, hits, enchants}`,
   `guilds`, `timers`, `mail/news`, `audit (kills, deaths, trades)`.
3. **MVP slice (playable loop):** creation (200+45 budgets) → Tos-like hub +
   3–5 wilderness rooms + 1 dungeon + Underworld → 4–6 mobs (rat/spider/orc/
   zombie/skeleton + 1 caster) with HP/Off/Def formulas → 1 swing/s combat +
   hit/damage caps + armor/shield/resist tables → 2 schools × 4 spells each +
   4 weapon skills + dodge/block/parry → use-based +1% + HP-gain + learn-points
   → vendors/bank/basic trade → death corpse + Underworld + penalties → PvP
   flags + NO_PK/safe rooms. Defer: guilds/war/rent, factions/territory,
   Justice election, Assassin Game, Jala hinder matrix, housing/rentables,
   atrophy, weather/hour.
4. **Voxel mapping:** rooms are small BSP sectors — map 1 room → 1 Godot scene
   chunk with grid exits; walls (fire/bramble/web/lightning/fog/illusion) =
   temp blocking volumes with tick damage; actors = voxel rigs with
   palette-swapped face/hair/skin + gear slots (weapon/shield/armor/helm/
   robe). Reuse M59’s `STAT {value,min,max,curmax}` packet shape for HUD.
5. **Numbers to port first (all in §§3–7):** Off/Def/hit, weapon type/quality,
   30/hit + ⅓-HP caps, spell base × `(50+power/2)/99`, resist clip ±100,
   regen tick formulas, 75-swing check, 10 pts/15–22 min, 16 learn points,
   HP highmark `((i+1)*i)` roll, death cost 100 → /3 or −1 HP/−1–2 skills.
   Tune *after* with bots; keep formulas server-side constants.
6. **Anti-cheat/economy lessons:** buffed-Max halving, uptime-gated starter
   gifts, PK loot tags, logoff ghosts, server-loss amnesty, lawful-refusal,
   reagent costs + ring charges. Port these, not just damage.
7. **Legal:** reimplement, don’t paste GPL KOD/C. New name, new voxel assets,
   new room layouts “inspired by” (Tos-like starter, not Tos). Credit M59.

### Open questions for next session

- Exact starting spell/skill offer list for Voxilian MVP (pick 8+4?).
- Voxel room scale: keep M59’s ~50×50-block sectors or larger?
- Faction/guild phase 2 scope: minimal guild (chat+hall+war flag) vs full rent?
- Tick rates on Go: keep 1 s attack / 150 ms chase / 1.5 s move, or faster?
- Persistence: full `save-all` snapshots vs event-sourced audit?

---

## Appendix A — Quick-reference constants

```text
Stats: 1..50 creation, sum<=200, effective 1..70, display max 50
Ability: start 1..25, cap 1..99, softcap >2*req-1 (/4 spells, /5 skills)
Success: ((100-req)*ability/100)+req, spells 5..95
Advance: 10 pts / 15-22 min timer; 75 swings/check; learn max 16 (+Int)
HP: start 20, cap 100+Stam hard 150; PK-enable 30; hit cap 30, victim cap HP/3
Attack: 1/s; PostCast 2s; hit (Off*55)/Def 10..95%
Offense: Stroke*3+Prof*2+Aim*4+HP*1.5 | Defense: Parry*2+Block+Dodge*3+Agi*4+HP*1.5
Mana start 15+Myst/5; node ((5+Myst)/10)+3 (Fey x2); vigor 1..200 rest to 80
Regen base 150s HP / mana per §4.4; rest 1-2.5s/tick; stomach 100->0 ~14min
Death cost 100 (events 90/60); newbie /3; logoff ghost 600s, equiv-death 10
Rooms 265; guild max 400 min 3; karma -100..100 (x100 stored)
```

## Appendix B — Primary sources checked

`manual/chap*.tex app*.tex` (BlakSton engine, not rules); `doc/protocol.txt`,
`doc/overview.tex`, `doc/general.txt`; `doc/design/Mechanics/*.htm`
(stats/combat/weapons/monsters/treasure/karma/factions/logoff/rooms);
`kod/include/blakston.khd`; `kod/util/{system,settings,questengine,parlia}.kod`;
`kod/object/active/holder/nomoveon/battler/{player,user,monster}.kod`;
`kod/object/active/holder/nomoveon/battler/monster/*.kod` (66);
`kod/object/passive/{spell,skill}*.kod` (~150+20);
`kod/object/item/passitem/{weapon,armor,defmod}*.kod`;
`kod/object/passive/{trestype,guild,quest}.kod`;
`kod/object/active/holder/room*/**` (120 wrappers);
`resource/rooms/*.roo` (265); `blakserv/*.c`; `Meridian59/README`, `LICENSE`.

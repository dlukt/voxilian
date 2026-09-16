package sim

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dlukt/voxilian/internal/world"
)

// Complete immutable immediate-death capture (spec §9.5.1g,
// M5-T5c3c2): the store-independent sim-domain boundary between the
// live player entity and Store-domain persistence. `internal/sim`
// only: no store, persist, pgx, sqlc output, gateway, session, or
// proto import; no blocking work, no goroutines, no persistence,
// no wire behavior, no T5a plan recomputation beyond validation,
// no Saver interaction.
//
// The live player entity owns CharacterID, position, PlayerVitals,
// ephemeral runtime, and lifecycle state, but NOT the rest of the
// complete mutable character/item content required by
// store.DeathEntryRequest. This file freezes the missing complete
// in-memory state (PlayerDurableState), the atomic owner-local
// begin+capture composing c3c1, and the pure real-death builder
// over one immutable base capture plus already-resolved T5a
// outputs. The sim-domain -> Store-domain mapping lives in
// internal/persist (which may import sim + store).

// ErrPlayerDurableStateMissing marks a complete death capture (or
// full-state validation path) against a player with no complete
// durable shadow: legacy minimal AddPlayerEntity players predate
// c3c2 and legitimately carry none. Zero mutation. Matching MUST
// use errors.Is, never string parsing as control flow.
var ErrPlayerDurableStateMissing = errors.New("sim: player durable state missing")

// Catalog identity domain frozen for durable-state validation
// (spec §9.5.1g): spell, skill, and item-proto IDs live in the
// stable 1..65535 namespace guaranteed by the catalog PK rows
// (migration 0002, spec §8.2). Sim performs no PG lookup: domain
// membership is the validation, not row existence.
const (
	minCatalogID = 1
	maxCatalogID = 65535
)

// deathGainFlagResetMask is the durable part of ResetGainFlags
// (spec §9.5.6): PFLAG_DID_DAMAGE 0x000010, PFLAG_TOOK_DAMAGE
// 0x000020, PFLAG_DODGED 0x000040. poKill_target is ephemeral live
// combat state with no durable representation and is never encoded
// here.
const deathGainFlagResetMask int32 = 0x000010 | 0x000020 | 0x000040

// Advancement JSON keys frozen by the §9.5.1g advancement contract.
const (
	advPointsKey   = "adv_points"
	advGainKey     = "gain_chance"
	advTimerDueKey = "adv_timer_due"
)

// PlayerAbilityState is one durable spell or skill row of the
// store-independent shadow (spec §9.5.1g): stable catalog ID,
// current ability percentage, and the atrophy marker. Spell and
// skill IDs are separate namespaces: the same numeric ID may
// legally exist in both.
type PlayerAbilityState struct {
	ID          int32
	Ability     int16
	AtrophyFlag bool
}

// PlayerInventoryItemState is one durable inventory item of the
// store-independent shadow (spec §9.5.1g): durable item identity,
// immutable proto identity metadata, complete mutable root
// content (Qty/Hits/Enchants), and the inventory slot label.
// ProtoID is immutable identity/content metadata for later
// resolved death-policy composition: it is NOT part of
// store.ItemSnapshot and the persist mapper MUST NOT invent a
// mutable proto update from it.
type PlayerInventoryItemState struct {
	ID      int64
	ProtoID int32

	Qty      int32
	Hits     int32
	Enchants []byte

	Slot string
}

// PlayerDurableState is the complete store-independent player
// durable shadow (spec §9.5.1g): the mutable gameplay content not
// already authoritatively owned by the live entity. Position and
// vitals are deliberately NOT duplicated here: entity.position
// and entity.vitals remain authoritative, and death capture
// combines those current live values with this shadow in ONE
// owner turn. The shadow deliberately does NOT contain revision,
// Saver, pending-death, PK-protection, corpse, session,
// NetEntityID, PG-handle, or Store state. Treat values as
// immutable: installation deep-freezes and inspection/capture
// return independent copies.
type PlayerDurableState struct {
	Karma       int32
	Advancement []byte
	Flags       int32

	Spells []PlayerAbilityState
	Skills []PlayerAbilityState

	Items []PlayerInventoryItemState
}

// validatePlayerDurableState rejects an incomplete/inconsistent
// durable shadow before it can become live (spec §9.5.1g):
// Advancement must be valid JSON with a top-level object; spell
// IDs must be catalog IDs unique in the spell namespace; skill
// IDs must be catalog IDs unique in the skill namespace; Ability
// 1..99; inventory ItemID > 0 and unique; ProtoID in 1..65535;
// Enchants valid JSON; Qty/Hits use their existing int32 domain
// with no new positivity rule. Caller-owned slice order is
// authoritative and is never sorted here.
func validatePlayerDurableState(s PlayerDurableState) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("sim: durable state "+format+": %w",
			append(args, ErrInvalidDeathInput)...)
	}
	trimmed := bytes.TrimSpace(s.Advancement)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(s.Advancement) {
		return invalid("advancement is not a JSON object")
	}
	for _, id := range s.Spells {
		if id.ID < minCatalogID || id.ID > maxCatalogID {
			return invalid("spell id=%d outside catalog domain", id.ID)
		}
		if id.Ability < deathAbilityFloor || id.Ability > deathAbilityCeil {
			return invalid("spell id=%d ability=%d", id.ID, id.Ability)
		}
	}
	if err := checkDurableAbilityKeys("spell", s.Spells); err != nil {
		return err
	}
	for _, id := range s.Skills {
		if id.ID < minCatalogID || id.ID > maxCatalogID {
			return invalid("skill id=%d outside catalog domain", id.ID)
		}
		if id.Ability < deathAbilityFloor || id.Ability > deathAbilityCeil {
			return invalid("skill id=%d ability=%d", id.ID, id.Ability)
		}
	}
	if err := checkDurableAbilityKeys("skill", s.Skills); err != nil {
		return err
	}
	seen := make(map[int64]struct{}, len(s.Items))
	for _, it := range s.Items {
		if it.ID <= 0 {
			return invalid("item id=%d", it.ID)
		}
		if _, dup := seen[it.ID]; dup {
			return invalid("duplicate item id=%d", it.ID)
		}
		seen[it.ID] = struct{}{}
		if it.ProtoID < minCatalogID || it.ProtoID > maxCatalogID {
			return invalid("item id=%d proto id=%d outside catalog domain", it.ID, it.ProtoID)
		}
		if !json.Valid(it.Enchants) {
			return invalid("item id=%d enchants is not valid JSON", it.ID)
		}
	}
	return nil
}

// checkDurableAbilityKeys rejects duplicate catalog IDs within one
// durable ability namespace (spec §9.5.1g). Spell and skill
// namespaces are independent: the same numeric ID may exist in
// both.
func checkDurableAbilityKeys(kind string, list []PlayerAbilityState) error {
	seen := make(map[int32]struct{}, len(list))
	for _, a := range list {
		if _, dup := seen[a.ID]; dup {
			return fmt.Errorf("sim: durable state duplicate %s id=%d: %w", kind, a.ID, ErrInvalidDeathInput)
		}
		seen[a.ID] = struct{}{}
	}
	return nil
}

// freezePlayerDurableState deep-copies a validated durable shadow
// into immutable private ownership (spec §9.5.1g): Advancement
// bytes, Spells/Skills/Items slices (preserving nil-vs-empty so
// the captured shape never aliases the caller's slice header),
// and every item Enchants byte slice. The caller retains full
// ownership of its value: later caller mutation cannot reach the
// frozen copy.
func freezePlayerDurableState(s PlayerDurableState) PlayerDurableState {
	frozen := s
	frozen.Advancement = append([]byte(nil), s.Advancement...)
	frozen.Spells = append([]PlayerAbilityState(nil), s.Spells...)
	frozen.Skills = append([]PlayerAbilityState(nil), s.Skills...)
	frozen.Items = append([]PlayerInventoryItemState(nil), s.Items...)
	if s.Spells == nil {
		frozen.Spells = nil
	}
	if s.Skills == nil {
		frozen.Skills = nil
	}
	if s.Items == nil {
		frozen.Items = nil
	}
	for i := range s.Items {
		frozen.Items[i].Enchants = append([]byte(nil), s.Items[i].Enchants...)
	}
	return frozen
}

// AddPlayerEntityWithDurableState inserts a player entity carrying
// the durable CharacterID identity plus authoritative vitals plus
// the complete durable shadow (spec §9.5.1g): the additive
// full-state installation path. The complete durable state MUST
// validate and freeze BEFORE ordinary player creation mutates the
// registry or consumes an EntityID: an invalid durable state
// creates no player and consumes no EntityID. Past that gate the
// ordinary AddPlayerEntity semantics apply unchanged (invalid
// CharacterID, invalid vitals/inputs, duplicate live binding,
// and all-or-nothing position/ID-exhaustion rules with no
// identity binding left behind on failure), and the frozen shadow
// installs atomically with the vitals, classification, and
// CharacterID binding. No Store/gateway type appears. A
// concurrent/gateway full-state add ingress is NOT provided;
// T5c4 owns real world-entry/reconnect hydration.
//
// Owner-local: call only from the sim owner goroutine (Run/Step)
// or in Step-driven tests.
func (e *Engine) AddPlayerEntityWithDurableState(characterID CharacterID, pos world.Vec3, vitals PlayerVitals, runtimeInputs PlayerVitalsRuntimeInputs, durable PlayerDurableState) (EntitySnapshot, error) {
	if err := validatePlayerDurableState(durable); err != nil {
		return EntitySnapshot{}, err
	}
	frozen := freezePlayerDurableState(durable)
	if characterID <= InvalidCharacterID {
		return EntitySnapshot{}, fmt.Errorf("%w: %d", ErrInvalidCharacterID, int64(characterID))
	}
	if err := vitals.Validate(); err != nil {
		return EntitySnapshot{}, err
	}
	if err := runtimeInputs.Validate(); err != nil {
		return EntitySnapshot{}, err
	}
	if live, ok := e.registry.liveEntityForCharacter(characterID); ok {
		return EntitySnapshot{}, fmt.Errorf("%w: character %d on entity %d", ErrCharacterAlreadyActive, int64(characterID), uint64(live))
	}
	snap, err := e.registry.AddEntity(pos)
	if err != nil {
		return EntitySnapshot{}, err
	}
	ent, err := e.registry.lookup(snap.ID)
	if err != nil {
		return EntitySnapshot{}, fmt.Errorf("sim: entity %d vanished after add: %w", uint64(snap.ID), err)
	}
	ent.isPlayer = true
	ent.characterID = characterID
	ent.vitals = vitals
	ent.volumeFlags = e.collision.VolumeFlagsAt(pos)
	ent.durable = &frozen
	e.initPlayerRuntime(ent, runtimeInputs)
	e.registry.characters[characterID] = ent.id
	return ent.snapshot(), nil
}

// PlayerDurableStateOf inspects a live entity's complete durable
// shadow immutably (spec §9.5.1g): unknown ID ->
// ErrEntityNotFound; a known generic entity -> (zero, false,
// nil); a player without a complete shadow (legacy minimal
// AddPlayerEntity) -> (zero, false, nil); a player with a shadow
// -> (independent deep COPY, true, nil). The boolean reports
// shadow presence, not player classification. A migrating entity
// still inspects read-only under its quiesced source ownership.
// Inspection never mutates and stays allowed while the player is
// locked.
func (e *Engine) PlayerDurableStateOf(id EntityID) (PlayerDurableState, bool, error) {
	var ent *entity
	if rec, ok := e.registry.migrations[id]; ok {
		ent = rec.entity
	} else {
		var err error
		ent, err = e.registry.lookup(id)
		if err != nil {
			return PlayerDurableState{}, false, err
		}
	}
	if !ent.isPlayer || ent.durable == nil {
		return PlayerDurableState{}, false, nil
	}
	return freezePlayerDurableState(*ent.durable), true, nil
}

// ImmediateDeathBaseCapture is the immutable owner-local death
// capture from one atomic begin+capture turn (spec §9.5.1g): the
// exact c3c1 DeathAttemptToken correlated with the deep-frozen
// complete durable shadow plus the current authoritative
// pre-remap death position and current PlayerVitals. ItemKeys are
// the opaque T5a keys aligned 1:1 with Durable.Items in exact
// authoritative inventory order: each key equals its capture
// index and carries NO durable-ID semantics (tests MUST use
// ItemIDs that do not equal these keys). Treat the value as
// immutable: the begin helper builds it from independent copies
// and the real-death builder deep-copies what it consumes.
type ImmediateDeathBaseCapture struct {
	Token         DeathAttemptToken
	DeathPosition world.Vec3
	Vitals        PlayerVitals
	Durable       PlayerDurableState
	ItemKeys      []int
}

// PlayerBeginImmediateDeathCapture is the narrow owner-local
// helper composing c3c1 rather than replacing it (spec §9.5.1g).
// Binding behavior: first verify the complete durable shadow
// exists (missing -> ErrPlayerDurableStateMissing before quiesce,
// deathEpoch increment, and life-state transition); snapshot and
// deep-freeze the current complete durable shadow plus the
// current authoritative pre-remap death position and current
// PlayerVitals; then invoke the existing
// PlayerBeginDeathPersistence semantics in the SAME owner turn;
// return the exact c3c1 DeathAttemptToken correlated with that
// immutable base capture. The player must already have HP==0:
// the existing begin transition owns that rule (ErrPlayerNotDead
// surfaces from it with the snapshot discarded). Resolution and
// life-state failures likewise surface from the composed
// transition with zero mutation. The helper performs NO zero-HP
// automatic dispatch, NO disposition choice, NO double-death
// calculation, NO Store call, NO goroutine. T5c3c3 later decides
// when to call it. PlayerBeginDeathPersistence itself stays
// available and behavior-compatible.
//
// Owner-local: call only from the sim owner goroutine (Run/Step)
// or in Step-driven tests.
func (e *Engine) PlayerBeginImmediateDeathCapture(id EntityID) (DeathAttemptToken, ImmediateDeathBaseCapture, error) {
	ent, err := e.resolvePlayerAnyLife(id)
	if err != nil {
		return DeathAttemptToken{}, ImmediateDeathBaseCapture{}, err
	}
	if ent.durable == nil {
		return DeathAttemptToken{}, ImmediateDeathBaseCapture{}, fmt.Errorf("%w: id %d", ErrPlayerDurableStateMissing, uint64(id))
	}
	base := ImmediateDeathBaseCapture{
		DeathPosition: ent.position,
		Vitals:        ent.vitals,
		Durable:       freezePlayerDurableState(*ent.durable),
		ItemKeys:      make([]int, len(ent.durable.Items)),
	}
	for i := range ent.durable.Items {
		base.ItemKeys[i] = i
	}
	tok, err := e.PlayerBeginDeathPersistence(id)
	if err != nil {
		return DeathAttemptToken{}, ImmediateDeathBaseCapture{}, err
	}
	base.Token = tok
	return tok, base, nil
}

// DecodeDeathAdvancementInputs is the ONE sim-owned pure decoding
// helper for the frozen advancement JSON contract (spec §9.5.1g),
// usable by T5c3c3 to obtain the two current inputs for
// PlanDeathAdvancement: missing adv_points => semantic 0, missing
// gain_chance => semantic 0 (the pinned Meridian source
// initializes piAdvancement_points = 0, piGain_chance = 0).
// Malformed/non-object advancement JSON and malformed selected
// numeric fields are rejected; unknown/unowned fields remain
// opaque and are never inspected.
func DecodeDeathAdvancementInputs(advancement []byte) (points, gainChance int, err error) {
	trimmed := bytes.TrimSpace(advancement)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(advancement) {
		return 0, 0, fmt.Errorf("sim: death advancement is not a JSON object: %w", ErrInvalidDeathInput)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(advancement, &obj); err != nil {
		return 0, 0, fmt.Errorf("sim: death advancement is not a JSON object: %w", ErrInvalidDeathInput)
	}
	decodeField := func(key string) (int, error) {
		raw, ok := obj[key]
		if !ok {
			return 0, nil
		}
		var v any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return 0, fmt.Errorf("sim: death advancement %s malformed: %w", key, ErrInvalidDeathInput)
		}
		num, ok := v.(json.Number)
		if !ok {
			return 0, fmt.Errorf("sim: death advancement %s malformed: %w", key, ErrInvalidDeathInput)
		}
		i64, err := num.Int64()
		if err != nil {
			return 0, fmt.Errorf("sim: death advancement %s malformed: %w", key, ErrInvalidDeathInput)
		}
		out, ok := toInt(i64)
		if !ok {
			return 0, fmt.Errorf("sim: death advancement %s malformed: %w", key, ErrInvalidDeathInput)
		}
		return out, nil
	}
	if points, err = decodeField(advPointsKey); err != nil {
		return 0, 0, err
	}
	if gainChance, err = decodeField(advGainKey); err != nil {
		return 0, 0, err
	}
	return points, gainChance, nil
}

// DeathKillerKind is the sim-domain killer identity domain for a
// real-death capture (spec §9.5.1g): character killer, mob
// killer, or none/environmental. Zero is unset/invalid. It maps
// mechanically to the existing Store contract in internal/persist;
// sim MUST NOT import store.DeathEntryKiller.
type DeathKillerKind uint8

const (
	// DeathKillerNone is an environmental death with no kill audit
	// identity. CharacterID must be invalid and MobID zero.
	DeathKillerNone DeathKillerKind = iota
	// DeathKillerCharacter audits a player killer by durable
	// CharacterID (> 0, MobID zero).
	DeathKillerCharacter
	// DeathKillerMob audits a mob-proto killer by MobID (> 0,
	// CharacterID invalid).
	DeathKillerMob
)

// DeathKillerIdentity is the small sim-domain killer identity
// sufficient to map the existing Store contract (spec §9.5.1g).
type DeathKillerIdentity struct {
	Kind        DeathKillerKind
	CharacterID CharacterID
	MobID       int32
}

// validateDeathKiller rejects identity-domain violations: exactly
// one valid identity for the selected kind, zero values for
// DeathKillerNone, and no unknown kind.
func validateDeathKiller(k DeathKillerIdentity) error {
	switch k.Kind {
	case DeathKillerNone:
		if k.CharacterID != InvalidCharacterID || k.MobID != 0 {
			return fmt.Errorf("sim: death killer none with identity: %w", ErrInvalidDeathInput)
		}
	case DeathKillerCharacter:
		if k.CharacterID <= InvalidCharacterID || k.MobID != 0 {
			return fmt.Errorf("sim: death character killer identity character=%d mob=%d: %w",
				int64(k.CharacterID), k.MobID, ErrInvalidDeathInput)
		}
	case DeathKillerMob:
		if k.MobID <= 0 || k.CharacterID != InvalidCharacterID {
			return fmt.Errorf("sim: death mob killer identity character=%d mob=%d: %w",
				int64(k.CharacterID), k.MobID, ErrInvalidDeathInput)
		}
	default:
		return fmt.Errorf("sim: death killer kind=%d: %w", uint8(k.Kind), ErrInvalidDeathInput)
	}
	return nil
}

// DeathAffectedItem is one resolved item mutation of the complete
// sim-domain immediate-death capture (spec §9.5.1g): the COMPLETE
// existing mutable item root content (ID/Qty/Hits/Enchants) plus
// this death's PK-protection duration in milliseconds (zero for
// unprotected relocations, including the Token-death special
// relocation). The resulting semantic location is always ground
// at the captured pre-remap death position; the persist mapper
// owns that Store encoding.
type DeathAffectedItem struct {
	ID       int64
	Qty      int32
	Hits     int32
	Enchants []byte

	PKProtectionDurationMs int64
}

// ImmediateDeathBuildInput is the complete real-death build input
// (spec §9.5.1g): one immutable base capture plus already-resolved
// T5a outputs plus caller-resolved post-death state. TokenItemID
// is the already-resolved actual Token ItemID when
// Disposition.TokenDeath is true, else 0. Killer is the optional
// sim-domain killer identity (zero value = environmental).
type ImmediateDeathBuildInput struct {
	Base        ImmediateDeathBaseCapture
	Disposition DeathDispositionPlan
	Corpse      CorpsePolicy
	Drops       DeathDropPlan
	Advancement DeathAdvancementPlan
	PostVitals  PlayerVitals
	Pending     PendingDeathPlan
	Placement   world.Vec3
	TokenItemID int64
	Killer      DeathKillerIdentity
}

// ImmediateDeathCapture is the one complete immutable sim-domain
// immediate-death capture (spec §9.5.1g): sufficient for the
// persist mapper and for later c3c3 owner installation after
// persistence. Durable is the resulting post-death
// PlayerDurableState (what c3c3 can later install), NOT merely a
// Store request recipe. AffectedItems carries the complete
// affected item mutations in base inventory / T5a order. No live
// entity mutation happens while building this value; no
// Store/Saver revision appears anywhere in it.
type ImmediateDeathCapture struct {
	Token         DeathAttemptToken
	DeathPosition world.Vec3
	Placement     world.Vec3
	Vitals        PlayerVitals
	Durable       PlayerDurableState
	Disposition   DeathDispositionPlan
	Corpse        CorpsePolicy
	Pending       PendingDeathPlan
	AffectedItems []DeathAffectedItem
	Killer        DeathKillerIdentity
}

// BuildImmediateDeathCapture is the pure builder over one
// immutable base capture and already-resolved T5a outputs (spec
// §9.5.1g): it validates impossible/inconsistent composition,
// maps the T5a plans onto the captured durable content, and
// returns one complete immutable sim-domain immediate-death
// capture. It does NOT import Store, run persistence, mutate any
// live entity, or consume the caller's base value (independent
// deep copies are built throughout; later caller mutation of the
// input cannot reach the capture).
func BuildImmediateDeathCapture(in ImmediateDeathBuildInput) (ImmediateDeathCapture, error) {
	if err := validateDispositionPlan(in.Disposition); err != nil {
		return ImmediateDeathCapture{}, err
	}
	if in.Disposition.Disposition == DeathAvoided {
		return ImmediateDeathCapture{}, fmt.Errorf("sim: death capture for avoided death: %w", ErrInvalidDeathInput)
	}
	if in.Base.Token.EntityID == InvalidEntityID ||
		in.Base.Token.CharacterID <= InvalidCharacterID ||
		in.Base.Token.Epoch == 0 {
		return ImmediateDeathCapture{}, fmt.Errorf("sim: death capture with invalid attempt token: %w", ErrInvalidDeathInput)
	}
	if len(in.Base.ItemKeys) != len(in.Base.Durable.Items) {
		return ImmediateDeathCapture{}, fmt.Errorf("sim: death capture base keys/items length %d/%d: %w",
			len(in.Base.ItemKeys), len(in.Base.Durable.Items), ErrInvalidDeathInput)
	}
	seenBase := make(map[int]struct{}, len(in.Base.ItemKeys))
	for _, k := range in.Base.ItemKeys {
		if _, dup := seenBase[k]; dup {
			return ImmediateDeathCapture{}, fmt.Errorf("sim: death capture base key %d duplicated: %w", k, ErrInvalidDeathInput)
		}
		seenBase[k] = struct{}{}
	}
	if err := in.PostVitals.Validate(); err != nil {
		return ImmediateDeathCapture{}, err
	}
	if _, err := world.CellForPosition(in.Placement); err != nil {
		return ImmediateDeathCapture{}, fmt.Errorf("%w: %w", ErrInvalidPosition, err)
	}
	if in.Corpse.LifetimeMs != PlayerCorpseDecomposeMs || in.Corpse.NoStealMs != PlayerCorpseNoStealMs {
		return ImmediateDeathCapture{}, fmt.Errorf("sim: death capture corpse policy: %w", ErrInvalidDeathInput)
	}
	if in.Corpse.DeathTimeSeconds < 0 {
		return ImmediateDeathCapture{}, fmt.Errorf("sim: death capture death time %d: %w", in.Corpse.DeathTimeSeconds, ErrInvalidDeathTime)
	}
	// PendingDeathPlan MUST be consistent with the same
	// disposition/corpse: recompute through the existing T5a pure
	// function rather than duplicating its mechanics.
	wantPending, err := PlanPendingDeath(in.Disposition, in.Corpse)
	if err != nil {
		return ImmediateDeathCapture{}, err
	}
	if in.Pending.Phase != wantPending.Phase ||
		in.Pending.EffectiveDeathCost != wantPending.EffectiveDeathCost ||
		in.Pending.DeathTimeSeconds != wantPending.DeathTimeSeconds ||
		in.Pending.Corpse != wantPending.Corpse {
		return ImmediateDeathCapture{}, fmt.Errorf("sim: death capture pending plan inconsistent: %w", ErrInvalidDeathInput)
	}
	// DeathAdvancementPlan MUST correspond to the current captured
	// adv_points/gain_chance and disposition: recompute through
	// the existing T5a pure function.
	points, gain, err := DecodeDeathAdvancementInputs(in.Base.Durable.Advancement)
	if err != nil {
		return ImmediateDeathCapture{}, err
	}
	wantAdv, err := PlanDeathAdvancement(in.Disposition.Disposition, points, gain)
	if err != nil {
		return ImmediateDeathCapture{}, err
	}
	if in.Advancement != wantAdv {
		return ImmediateDeathCapture{}, fmt.Errorf("sim: death capture advancement plan inconsistent: %w", ErrInvalidDeathInput)
	}
	// DeathDropPlan MUST correspond one-for-one, in order, to the
	// base capture's opaque item keys: no missing, extra,
	// reordered, or duplicate keys.
	if len(in.Drops.Items) != len(in.Base.ItemKeys) {
		return ImmediateDeathCapture{}, fmt.Errorf("sim: death capture drop plan length %d vs base %d: %w",
			len(in.Drops.Items), len(in.Base.ItemKeys), ErrInvalidDeathInput)
	}
	for i, d := range in.Drops.Items {
		if d.Key != in.Base.ItemKeys[i] {
			return ImmediateDeathCapture{}, fmt.Errorf("sim: death capture drop key %d at index %d vs base %d: %w",
				d.Key, i, in.Base.ItemKeys[i], ErrInvalidDeathInput)
		}
	}
	if in.Disposition.Disposition == DeathCheap {
		for _, d := range in.Drops.Items {
			if d.Drop {
				return ImmediateDeathCapture{}, fmt.Errorf("sim: death capture cheap drop entry %d dropping: %w", d.Key, ErrInvalidDeathInput)
			}
		}
	}
	// Token-death special relocation: the caller-supplied
	// already-resolved actual Token ItemID must occur exactly
	// once in the captured inventory; without TokenDeath a
	// nonzero Token ItemID is invalid.
	tokenIdx := -1
	if in.Disposition.TokenDeath {
		if in.TokenItemID <= 0 {
			return ImmediateDeathCapture{}, fmt.Errorf("sim: death capture token death without token id: %w", ErrInvalidDeathInput)
		}
		for i, it := range in.Base.Durable.Items {
			if it.ID == in.TokenItemID {
				if tokenIdx >= 0 {
					return ImmediateDeathCapture{}, fmt.Errorf("sim: death capture token id=%d duplicated: %w", in.TokenItemID, ErrInvalidDeathInput)
				}
				tokenIdx = i
			}
		}
		if tokenIdx < 0 {
			return ImmediateDeathCapture{}, fmt.Errorf("sim: death capture token id=%d absent: %w", in.TokenItemID, ErrInvalidDeathInput)
		}
	} else if in.TokenItemID != 0 {
		return ImmediateDeathCapture{}, fmt.Errorf("sim: death capture token id=%d without token death: %w", in.TokenItemID, ErrInvalidDeathInput)
	}
	if err := validateDeathKiller(in.Killer); err != nil {
		return ImmediateDeathCapture{}, err
	}

	// Build the resulting post-death durable shadow from an
	// independent deep copy: caller mutation of the input base
	// after this call cannot reach the capture.
	result := freezePlayerDurableState(in.Base.Durable)
	if in.Disposition.Disposition == DeathNormal {
		if err := applyNormalDeathAdvancement(&result, in.Advancement); err != nil {
			return ImmediateDeathCapture{}, err
		}
		result.Flags &^= deathGainFlagResetMask
		for i := range result.Spells {
			result.Spells[i].AtrophyFlag = true
		}
		for i := range result.Skills {
			result.Skills[i].AtrophyFlag = true
		}
	}
	// Cheap death preserves advancement bytes, flags, and atrophy
	// state exactly: no re-marshalling, no flag touch.

	keyIndex := make(map[int]int, len(in.Base.ItemKeys))
	for i, k := range in.Base.ItemKeys {
		keyIndex[k] = i
	}
	removed := make(map[int]bool, len(in.Drops.Items))
	affected := make([]DeathAffectedItem, 0, len(in.Drops.Items)+1)
	for _, d := range in.Drops.Items {
		if !d.Drop {
			continue
		}
		if d.PKProtectionDurationMs < 0 {
			return ImmediateDeathCapture{}, fmt.Errorf("sim: death capture drop entry %d negative PK duration: %w", d.Key, ErrInvalidDeathInput)
		}
		idx := keyIndex[d.Key]
		src := in.Base.Durable.Items[idx]
		affected = append(affected, DeathAffectedItem{
			ID:                     src.ID,
			Qty:                    src.Qty,
			Hits:                   src.Hits,
			Enchants:               append([]byte(nil), src.Enchants...),
			PKProtectionDurationMs: d.PKProtectionDurationMs,
		})
		removed[idx] = true
	}
	if tokenIdx >= 0 {
		src := in.Base.Durable.Items[tokenIdx]
		affected = append(affected, DeathAffectedItem{
			ID:                     src.ID,
			Qty:                    src.Qty,
			Hits:                   src.Hits,
			Enchants:               append([]byte(nil), src.Enchants...),
			PKProtectionDurationMs: 0,
		})
		removed[tokenIdx] = true
	}
	kept := make([]PlayerInventoryItemState, 0, len(result.Items))
	for i, it := range result.Items {
		if !removed[i] {
			kept = append(kept, it)
		}
	}
	if kept == nil {
		kept = []PlayerInventoryItemState{}
	}
	// Preserve nil-vs-empty inventory shape of the frozen base:
	// a base with no items at all keeps a nil slice.
	if len(result.Items) == 0 {
		kept = result.Items
	}
	result.Items = kept

	out := ImmediateDeathCapture{
		Token:         in.Base.Token,
		DeathPosition: in.Base.DeathPosition,
		Placement:     in.Placement,
		Vitals:        in.PostVitals,
		Durable:       result,
		Disposition:   in.Disposition,
		Corpse:        in.Corpse,
		Pending:       in.Pending,
		AffectedItems: affected,
		Killer:        in.Killer,
	}
	if out.AffectedItems == nil {
		out.AffectedItems = []DeathAffectedItem{}
	}
	return out, nil
}

// applyNormalDeathAdvancement maps the supplied validated
// DeathAdvancementPlan onto already-copied durable advancement
// JSON (spec §9.5.1g): adv_points and gain_chance are set to the
// planned values, adv_timer_due is DELETED when
// CancelAdvancementTimer is true, and every unknown advancement
// JSON key is preserved byte-for-byte.
func applyNormalDeathAdvancement(durable *PlayerDurableState, plan DeathAdvancementPlan) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(durable.Advancement, &obj); err != nil {
		return fmt.Errorf("sim: death capture advancement remap: %w", ErrInvalidDeathInput)
	}
	if obj == nil {
		obj = make(map[string]json.RawMessage)
	}
	setInt := func(key string, v int) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("sim: death capture advancement remap: %w", ErrInvalidDeathInput)
		}
		obj[key] = raw
		return nil
	}
	if err := setInt(advPointsKey, plan.PointsAfter); err != nil {
		return err
	}
	if err := setInt(advGainKey, plan.GainChanceAfter); err != nil {
		return err
	}
	if plan.CancelAdvancementTimer {
		delete(obj, advTimerDueKey)
	}
	remapped, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("sim: death capture advancement remap: %w", ErrInvalidDeathInput)
	}
	durable.Advancement = remapped
	return nil
}

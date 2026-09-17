package sim

import (
	"fmt"
	"math"

	"github.com/dlukt/voxilian/internal/world"
)

// Zero-HP resolved T5a orchestration + double-death runtime gate
// (spec §9.5.1j, M5-T5c3c3c2): the ONE canonical owner-local
// `Killed` orchestration that future M5-T7 combat and every other
// lethal-damage owner call in the SAME owner turn after lethal
// health application. `internal/sim` only: no store, persist,
// pgx, sqlc output, gateway, session, or proto import; no
// blocking work, no goroutines, no persistence, no wire
// behavior, no automatic world/content resolution, no
// PG/catalog lookup.
//
// Source order (pinned `Meridian59/Meridian59@095c07b`,
// `player.kod::Killed`): resolve/default cost; double-death
// early return with NO stamp; `CancelRescue`; stamp
// lastDeath; capture death location; Avoided test; Avoided
// HP=1 return; otherwise the real Cheap/Normal pipeline.
// Observable state order guard -> stamp -> branch is binding.
//
// Reservation composition (binding, §9.5.1j): the existing
// c3c3c1 seam is sufficient with no additive extension. The
// owner predicts the post-begin attempt token
// ({EntityID, CharacterID, deathEpoch+1}; exhaustion already
// rejected), builds the complete token-bearing capture,
// calls Prepare while STILL Alive, begins only after Prepare
// succeeds (verifying the real token equals the prediction —
// a single-owner invariant), and Activates in the SAME owner
// turn. Prepare (the only fallible reservation operation)
// runs strictly before begin; Activate (the only post-begin
// operation) is infallible by the c3c3c1 permit proof. The
// forbidden orders (begin-then-Prepare, begin-then-TrySubmit)
// do not exist in this path.

// ImmediateDeathItemPolicy is ONE already-resolved per-item
// death policy (spec §9.5.1j): the caller's resolved facts
// for the captured inventory item with the same durable
// ItemID at the SAME index. Binding: len(policies) ==
// len(captured durable Items), each policy ItemID equals the
// durable item at the same index, no reorder/sort/missing/
// extra/duplicate. Index i converts to the existing opaque
// T5a key base.ItemKeys[i]; Key = ItemID is never a contract.
type ImmediateDeathItemPolicy struct {
	ItemID int64

	DropOnDeath bool
	RoomAccepts bool
	SpecialItem bool
}

// ImmediateDeathResolvedInput is the one already-resolved
// orchestration input value (spec §9.5.1j): only
// already-resolved mechanics facts, no Store/session/
// gateway/PG values, no room IDs, no honor strings, no Token
// proto/class, no item classes, no catalog lookup. Derived
// facts are never duplicated: killer-is-player is
// Killer.Kind == DeathKillerCharacter, frenzy is
// Context.FrenzyActive.
type ImmediateDeathResolvedInput struct {
	// NowSeconds is the resolved whole-second death time
	// (source GetTime()): must be >= 0. Never time.Now()
	// inside the owner, never a sim uint32 tick.
	NowSeconds int64

	// DefaultDeathCost is the resolved settings default
	// (domain 1..100).
	DefaultDeathCost int
	// Context carries every RESOLVED world/game-mode fact
	// the source Killed routing consumes.
	Context DeathContext

	// Killer is the sim-domain killer identity
	// (none/environment, character, or mob).
	Killer DeathKillerIdentity

	// Items is the resolved per-item policy, 1:1 with the
	// captured durable inventory in exact durable order.
	Items []ImmediateDeathItemPolicy

	// TokenItemID is the already-resolved actual Token ItemID
	// when the disposition is a Token death, else 0.
	TokenItemID int64
	// TokenRestoredThreshold is the already-resolved
	// post-unuse rest threshold effect (10..100 domain) when
	// the disposition is a Token death, else 0.
	TokenRestoredThreshold int

	// StillNewbie is the resolved source PFLAG_TUTORIAL ==
	// FALSE fact; Murderer is the resolved PFLAG_MURDERER.
	StillNewbie bool
	Murderer    bool

	// Soldier-shield resolved inputs for the pure
	// PlanImmediateDeathHooks classification (no shield
	// mutation, no persistence).
	HasSoldierShield    bool
	SoldierShieldRank   int
	KilledByShieldEnemy bool

	// NewbieHomePlacement vs UnderworldPlacement are the
	// already-resolved respawn coordinates; the disposition's
	// NewbieHomeRespawn fact selects exactly one of them.
	NewbieHomePlacement world.Vec3
	UnderworldPlacement world.Vec3

	// RuntimeInputs is the already-resolved vitals runtime
	// snapshot for the future c3c3a authoritative post-death
	// completion; it belongs in the prepared persistence
	// work and is NOT installed on the live entity here.
	RuntimeInputs PlayerVitalsRuntimeInputs
}

// ImmediateDeathDisposition is the orchestration outcome.
// Blocked/Avoided are ordinary results, not errors; on error
// the disposition is meaningless — check err first.
type ImmediateDeathDisposition uint8

const (
	// ImmediateDeathBlocked is the source double-death guard:
	// NowSeconds < LastDeathSeconds + 2. Zero mutation
	// except the reservation Cancel; not an error.
	ImmediateDeathBlocked ImmediateDeathDisposition = iota
	// ImmediateDeathAvoided is the source non-death: HP = 1,
	// NewHealth-equivalent runtime, still Alive, lastDeath
	// stamped, reservation cancelled.
	ImmediateDeathAvoided
	// ImmediateDeathAccepted is a Cheap or Normal real death:
	// lastDeath stamped, complete capture prepared, owner in
	// DeathPersisting, prepared work activated same turn.
	ImmediateDeathAccepted
)

// ImmediateDeathOrchestrationResult is the explicit
// orchestration result (spec §9.5.1j). Token/Capture are
// meaningful only for ImmediateDeathAccepted. No Store
// request, revision, corpse DB ID, or Saver state is exposed.
type ImmediateDeathOrchestrationResult struct {
	Disposition ImmediateDeathDisposition
	Plan        DeathDispositionPlan
	Hooks       ImmediateDeathHooks
	Token       DeathAttemptToken
	Capture     ImmediateDeathCapture
}

// PlayerLastDeathSecondsOf inspects a live entity's ephemeral
// last-death timestamp immutably (spec §9.5.1j): unknown ID ->
// ErrEntityNotFound; a known generic entity -> (0, false,
// nil); a player -> (current whole-second stamp, true, nil).
// A migrating entity still inspects read-only under its
// quiesced source ownership. Inspection never mutates and
// stays allowed while the player is locked.
func (e *Engine) PlayerLastDeathSecondsOf(id EntityID) (int64, bool, error) {
	if rec, ok := e.registry.migrations[id]; ok {
		if !rec.entity.isPlayer {
			return 0, false, nil
		}
		return rec.entity.lastDeathSeconds, true, nil
	}
	ent, err := e.registry.lookup(id)
	if err != nil {
		return 0, false, err
	}
	if !ent.isPlayer {
		return 0, false, nil
	}
	return ent.lastDeathSeconds, true, nil
}

// snapshotDeathBase freezes the pre-begin immutable death base
// content for one resident player: the deep-frozen complete
// durable shadow, the current authoritative pre-remap death
// position, and the current PlayerVitals, correlated with the
// supplied (predicted) attempt token. No lifecycle mutation,
// no persistence. The caller owns when (and whether) to begin.
func snapshotDeathBase(ent *entity, token DeathAttemptToken) ImmediateDeathBaseCapture {
	base := ImmediateDeathBaseCapture{
		Token:         token,
		DeathPosition: ent.position,
		Vitals:        ent.vitals,
		Durable:       freezePlayerDurableState(*ent.durable),
		ItemKeys:      make([]int, len(ent.durable.Items)),
	}
	for i := range ent.durable.Items {
		base.ItemKeys[i] = i
	}
	return base
}

// PlayerOrchestrateImmediateDeath is the canonical owner-local
// zero-HP death orchestration (spec §9.5.1j). The caller (a
// future M5-T7 combat or other lethal-damage owner, same owner
// turn after lethal health application) supplies the resident
// zero-HP player, an already-created reservation holding one
// bounded executor permit, and the complete resolved input.
//
// Reservation ownership transfers to this call: every return
// path leaves it in exactly one terminal/useful state —
// Cancel on Blocked, Avoided, and every validation/error path
// before an accepted real death; exactly one Activate on an
// accepted real death. A nil reservation is rejected before
// mutation (no ownership to discharge).
//
// Zero-HP entry contract: resident player, PlayerLifeAlive,
// HP == 0, complete durable shadow present. Entry failures
// (ErrEntityNotPlayer, ErrEntityNotFound,
// ErrCellHandoffRequired, ErrPlayerNotAlive, ErrPlayerNotDead,
// ErrPlayerDurableStateMissing) are zero mutation.
//
// Failure atomicity: ALL host-language structural validation
// (step 1, including the pure probe disposition and every
// cross-field contract check) runs BEFORE the source-semantic
// lastDeath stamp, so hostile malformed inputs never consume
// the timestamp. After the stamp only tripwire-impossible
// pure-planner invariants remain plus the two narrow
// infrastructure operations (Prepare, begin), each handled
// with Cancel and no stranded DeathPersisting.
//
// Owner-local: call only from the sim owner goroutine
// (Run/Step) or in Step-driven tests.
func (e *Engine) PlayerOrchestrateImmediateDeath(id EntityID, res ImmediateDeathWorkReservation, in ImmediateDeathResolvedInput) (ImmediateDeathOrchestrationResult, error) {
	if res == nil {
		return ImmediateDeathOrchestrationResult{}, fmt.Errorf("sim: death orchestration without reservation: %w", ErrInvalidDeathInput)
	}
	fail := func(err error) (ImmediateDeathOrchestrationResult, error) {
		res.CancelImmediateDeathWork()
		return ImmediateDeathOrchestrationResult{}, err
	}

	// Step 1: structural validation with zero mutation.
	ent, err := e.resolvePlayerAnyLife(id)
	if err != nil {
		return fail(err)
	}
	if ent.lifeState != PlayerLifeAlive {
		return fail(fmt.Errorf("%w: id %d life %d", ErrPlayerNotAlive, uint64(id), uint8(ent.lifeState)))
	}
	if ent.vitals.HP != 0 {
		return fail(fmt.Errorf("%w: id %d hp %d", ErrPlayerNotDead, uint64(id), ent.vitals.HP))
	}
	if ent.durable == nil {
		return fail(fmt.Errorf("%w: id %d", ErrPlayerDurableStateMissing, uint64(id)))
	}
	if ent.deathEpoch == math.MaxUint64 {
		return fail(fmt.Errorf("%w: id %d", ErrDeathAttemptExhausted, uint64(id)))
	}
	if in.NowSeconds < 0 {
		return fail(fmt.Errorf("sim: death time %d: %w", in.NowSeconds, ErrInvalidDeathTime))
	}
	if err := ValidateDefaultDeathCost(in.DefaultDeathCost); err != nil {
		return fail(err)
	}
	if err := in.RuntimeInputs.Validate(); err != nil {
		return fail(err)
	}
	if _, err := world.CellForPosition(in.NewbieHomePlacement); err != nil {
		return fail(fmt.Errorf("%w: %w", ErrInvalidPosition, err))
	}
	if _, err := world.CellForPosition(in.UnderworldPlacement); err != nil {
		return fail(fmt.Errorf("%w: %w", ErrInvalidPosition, err))
	}
	if err := validateDeathKiller(in.Killer); err != nil {
		return fail(err)
	}
	if in.HasSoldierShield && (in.SoldierShieldRank < 1 || in.SoldierShieldRank > 10) {
		return fail(fmt.Errorf("sim: soldier shield rank %d: %w", in.SoldierShieldRank, ErrInvalidDeathInput))
	}
	if in.TokenItemID < 0 {
		return fail(fmt.Errorf("sim: death token id=%d: %w", in.TokenItemID, ErrInvalidDeathInput))
	}
	if in.TokenRestoredThreshold != 0 && (in.TokenRestoredThreshold < minRestThreshold || in.TokenRestoredThreshold > maxRestThreshold) {
		return fail(fmt.Errorf("sim: death token threshold %d: %w", in.TokenRestoredThreshold, ErrInvalidDeathInput))
	}
	if len(in.Items) != len(ent.durable.Items) {
		return fail(fmt.Errorf("sim: death item policies %d vs durable %d: %w", len(in.Items), len(ent.durable.Items), ErrInvalidDeathInput))
	}
	for i := range in.Items {
		if in.Items[i].ItemID != ent.durable.Items[i].ID {
			return fail(fmt.Errorf("sim: death item policy id=%d at index %d vs durable id=%d: %w",
				in.Items[i].ItemID, i, ent.durable.Items[i].ID, ErrInvalidDeathInput))
		}
	}
	// Probe disposition: the pure routing decision used here
	// only for cross-field contract validation. The branch
	// disposition below reuses this exact value; the probe is
	// host-language contract validation while the branch is
	// the source-semantic routing, so the observable state
	// order (guard -> stamp -> branch) still matches source.
	killerIsPlayer := in.Killer.Kind == DeathKillerCharacter
	probe, err := PlanDeathDisposition(in.DefaultDeathCost, in.Context, killerIsPlayer)
	if err != nil {
		return fail(err)
	}
	if probe.TokenDeath {
		if in.TokenItemID <= 0 {
			return fail(fmt.Errorf("sim: death token death without token id: %w", ErrInvalidDeathInput))
		}
		found := 0
		for _, it := range ent.durable.Items {
			if it.ID == in.TokenItemID {
				found++
			}
		}
		if found != 1 {
			return fail(fmt.Errorf("sim: death token id=%d found %d times: %w", in.TokenItemID, found, ErrInvalidDeathInput))
		}
		if in.TokenRestoredThreshold < minRestThreshold || in.TokenRestoredThreshold > maxRestThreshold {
			return fail(fmt.Errorf("sim: death token threshold %d: %w", in.TokenRestoredThreshold, ErrInvalidDeathInput))
		}
	} else {
		if in.TokenItemID != 0 {
			return fail(fmt.Errorf("sim: death token id=%d without token death: %w", in.TokenItemID, ErrInvalidDeathInput))
		}
		if in.TokenRestoredThreshold != 0 {
			return fail(fmt.Errorf("sim: death token threshold %d without token death: %w", in.TokenRestoredThreshold, ErrInvalidDeathInput))
		}
	}
	// Real-death advancement-field prevalidation (still step 1
	// structural validation): for the selected REAL route the
	// selected advancement fields are host-language contract
	// input, so a malformed value fails before the guard/stamp
	// and never consumes lastDeath. Avoided deaths never consume
	// these fields, so they are not inspected on that route. The
	// player stays Alive with no interleaving live mutation, so
	// the bytes decoded here equal the bytes later frozen into
	// the base capture; the precomputed plan is reused verbatim
	// below (post-stamp path tripwire-only).
	var preAdv DeathAdvancementPlan
	if probe.Disposition == DeathCheap || probe.Disposition == DeathNormal {
		points, gain, err := DecodeDeathAdvancementInputs(ent.durable.Advancement)
		if err != nil {
			return fail(err)
		}
		preAdv, err = PlanDeathAdvancement(probe.Disposition, points, gain)
		if err != nil {
			return fail(err)
		}
	}

	// Step 2: source double-death guard over resolved whole
	// seconds (strict <; exactly +2 proceeds). Inputs are
	// already proven non-negative, so this cannot fail.
	blocked, err := DeathBlockedByDoubleDeath(ent.lastDeathSeconds, in.NowSeconds)
	if err != nil {
		return fail(err)
	}
	if blocked {
		res.CancelImmediateDeathWork()
		return ImmediateDeathOrchestrationResult{Disposition: ImmediateDeathBlocked}, nil
	}

	// Step 4: source stamp before the Avoided/real branch.
	ent.lastDeathSeconds = in.NowSeconds

	// Step 5: branch disposition (identical to the probe).
	plan := probe

	// Step 6: Avoided — not a death. HP = 1 with exactly one
	// commitVitals event plus NewHealth-equivalent reconcile;
	// life stays Alive; reservation cancelled.
	if plan.Disposition == DeathAvoided {
		after := ent.vitals
		after.HP = 1
		if err := e.commitVitals(ent, after); err != nil {
			res.CancelImmediateDeathWork()
			return ImmediateDeathOrchestrationResult{}, err
		}
		e.reconcileHealth(ent, e.tick.Load())
		res.CancelImmediateDeathWork()
		return ImmediateDeathOrchestrationResult{Disposition: ImmediateDeathAvoided, Plan: plan}, nil
	}

	// Step 7: real death — freeze the pre-begin base content
	// and predict the post-begin attempt token. Exhaustion
	// was already rejected, so Epoch = deathEpoch+1 is valid
	// and the begin below must produce exactly this token
	// (single-owner invariant: no mutation can interleave).
	predicted := DeathAttemptToken{
		EntityID:    ent.id,
		CharacterID: ent.characterID,
		Epoch:       ent.deathEpoch + 1,
	}
	base := snapshotDeathBase(ent, predicted)

	// Source order: the token unuse restores the rest
	// threshold BEFORE the post-death vitals are computed.
	working := base.Vitals
	if plan.TokenDeath {
		working.RestThreshold = in.TokenRestoredThreshold
	}

	corpse, err := PlanCorpse(in.NowSeconds)
	if err != nil {
		res.CancelImmediateDeathWork()
		return ImmediateDeathOrchestrationResult{}, err
	}
	dropInputs := make([]DeathItemInput, len(in.Items))
	for i, p := range in.Items {
		dropInputs[i] = DeathItemInput{
			Key:         base.ItemKeys[i],
			DropOnDeath: p.DropOnDeath,
			RoomAccepts: p.RoomAccepts,
			SpecialItem: p.SpecialItem,
		}
	}
	drops, err := PlanDeathDrops(plan, dropInputs)
	if err != nil {
		res.CancelImmediateDeathWork()
		return ImmediateDeathOrchestrationResult{}, err
	}
	// Reuse the pre-stamp advancement plan validated against the
	// same immutable owner state (no interleaving live mutation
	// between predecode and this capture). Build below still
	// defensively recomputes from the frozen base and is
	// guaranteed to agree.
	adv := preAdv
	angel := GuardianAngelMailEligible(plan.DeathCost, in.StillNewbie, in.Murderer)
	postVitals, err := PlanPostDeathVitals(PostDeathVitalsInput{
		Vitals:            working,
		Disposition:       plan.Disposition,
		FrenzyActive:      in.Context.FrenzyActive,
		AngelMailEligible: angel,
	})
	if err != nil {
		res.CancelImmediateDeathWork()
		return ImmediateDeathOrchestrationResult{}, err
	}
	pending, err := PlanPendingDeath(plan, corpse)
	if err != nil {
		res.CancelImmediateDeathWork()
		return ImmediateDeathOrchestrationResult{}, err
	}
	hooks, err := PlanImmediateDeathHooks(plan, ImmediateDeathHooksInput{
		FrenzyActive:        in.Context.FrenzyActive,
		StillNewbie:         in.StillNewbie,
		Murderer:            in.Murderer,
		HasSoldierShield:    in.HasSoldierShield,
		SoldierShieldRank:   in.SoldierShieldRank,
		KilledByShieldEnemy: in.KilledByShieldEnemy,
	})
	if err != nil {
		res.CancelImmediateDeathWork()
		return ImmediateDeathOrchestrationResult{}, err
	}
	placement := in.UnderworldPlacement
	if plan.NewbieHomeRespawn {
		placement = in.NewbieHomePlacement
	}
	capture, err := BuildImmediateDeathCapture(ImmediateDeathBuildInput{
		Base:        base,
		Disposition: plan,
		Corpse:      corpse,
		Drops:       drops,
		Advancement: adv,
		PostVitals:  postVitals,
		Pending:     pending,
		Placement:   placement,
		TokenItemID: in.TokenItemID,
		Killer:      in.Killer,
	})
	if err != nil {
		res.CancelImmediateDeathWork()
		return ImmediateDeathOrchestrationResult{}, err
	}

	// Step 8: Prepare while STILL Alive. Failure Cancels
	// with no DeathPersisting transition (the frozen c3c3c1
	// invariant); lastDeath stays stamped per source order.
	// A correct pre-validation makes this tripwire-impossible.
	if err := res.PrepareImmediateDeathWork(capture, in.RuntimeInputs); err != nil {
		res.CancelImmediateDeathWork()
		return ImmediateDeathOrchestrationResult{}, err
	}

	// Step 9: irreversible begin only after successful
	// Prepare. Every precondition was proven in step 1 of
	// this same owner turn, so failure here is unreachable;
	// its handler still Cancels with no persistence job.
	tok, err := e.PlayerBeginDeathPersistence(id)
	if err != nil {
		res.CancelImmediateDeathWork()
		return ImmediateDeathOrchestrationResult{}, err
	}
	if tok != predicted {
		res.CancelImmediateDeathWork()
		return ImmediateDeathOrchestrationResult{}, fmt.Errorf("sim: death orchestration token %+v vs predicted %+v: %w",
			tok, predicted, ErrDeathAttemptMismatch)
	}

	// Step 10: activate the ALREADY-prepared work in the SAME
	// owner turn (incapable of queue-full by the c3c3c1
	// permit proof).
	res.ActivateImmediateDeathWork()

	return ImmediateDeathOrchestrationResult{
		Disposition: ImmediateDeathAccepted,
		Plan:        plan,
		Hooks:       hooks,
		Token:       tok,
		Capture:     capture,
	}, nil
}

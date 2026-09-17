package persist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
)

// Bounded off-owner CommitDeathEntry + proven materialized
// recovery executor (spec §9.5.1h, M5-T5c3c3b). A failed /
// ambiguous persist.CommitDeathEntry is NEVER blindly replayed:
// recovery reconciles every Saver participant through the
// existing T5c2a adapters and delivers the authoritative c3c3a
// owner completion ONLY when the materialized PG state proves
// the original transaction committed (every recovered root at
// exactly expected+1 with exact intended content and the exact
// pending-death shape). Otherwise the executor fails closed:
// no replay, no completion, the player remains
// DeathPersisting. A persistence worker NEVER mutates a live
// sim entity; live replacement happens ONLY through the typed
// c3c3a owner completion.

// ErrDeathCommitUnproven is the stable persist-domain sentinel
// returned when recovery succeeded enough to reconcile Saver
// metadata but the materialized state does NOT prove the
// attempted death transaction committed (stale revision,
// newer PG state, content mismatch, pending-row mismatch).
// Matched with errors.Is. Critical behavior: NO replay, NO
// owner completion, the player remains DeathPersisting.
var ErrDeathCommitUnproven = errors.New("persist: death commit unproven")

// Stable executor admission/shutdown errors (exact names follow
// repository conventions; matched with errors.Is).
var (
	// ErrDeathExecutorNotRunning reports work submitted while no
	// Run owns the executor.
	ErrDeathExecutorNotRunning = errors.New("persist: death executor not running")
	// ErrDeathExecutorQueueFull reports a non-blocking submit
	// against a saturated bounded queue. No job is published.
	ErrDeathExecutorQueueFull = errors.New("persist: death executor queue full")
	// ErrDeathExecutorShutdown reports a queued-but-not-started
	// job failed by executor shutdown. No completion follows.
	ErrDeathExecutorShutdown = errors.New("persist: death executor shutdown")
	// ErrDeathExecutorAlreadyRunning reports a second concurrent
	// Run against an already-running executor.
	ErrDeathExecutorAlreadyRunning = errors.New("persist: death executor already running")
	// ErrDeathExecutorInvalid reports invalid executor
	// configuration or invalid submitted work. Matched with
	// errors.Is.
	ErrDeathExecutorInvalid = errors.New("persist: death executor invalid")
)

// DeathExecutionStore is the stable process-level Store seam
// the executor owns rather than putting it in every job:
// the death write transaction plus both T5c2a read-only
// recovery loaders. Proven satisfied by *store.PGStore.
type DeathExecutionStore interface {
	DeathPersistenceStore
	DeathCharacterRecoveryLoader
	DeathItemRecoveryLoader
}

// Compile-time proof that production PGStore satisfies the
// executor seam (no wrapper, no replacement method).
var _ DeathExecutionStore = (*store.PGStore)(nil)

// ImmediateDeathCompletionSink is the narrow typed completion
// sink: the c3c3a owner ingress that installs authoritative
// post-death state on the single sim owner. Satisfied by
// *sim.Engine. No gateway dependency.
type ImmediateDeathCompletionSink interface {
	EnqueueImmediateDeathCompletion(
		context.Context,
		sim.ImmediateDeathCompletion,
	) (
		sim.EntitySnapshot,
		sim.DeathCompletionDisposition,
		error,
	)
}

// Compile-time proof that the sim Engine is the completion
// sink (the owner remains the ONLY live-state mutator).
var _ ImmediateDeathCompletionSink = (*sim.Engine)(nil)

// ImmediateDeathPersistenceWork is one submitted work item:
// ONLY immutable/resolved state. Capture is the complete
// sim-domain immediate-death capture; RuntimeInputs are
// supplied by future c3c3c — the executor validates and
// freezes them but does NOT resolve or recalculate them.
type ImmediateDeathPersistenceWork struct {
	Capture       sim.ImmediateDeathCapture
	RuntimeInputs sim.PlayerVitalsRuntimeInputs
}

// ImmediateDeathPersistenceResult is the one definitive bounded
// result per accepted work item, observable without owner
// mutation. Recovered=false means normal CommitDeathEntry
// acknowledgement; Recovered=true means completion followed
// proven materialized recovery. Err != nil means no successful
// owner completion delivery. No mutable Store snapshots are
// exposed through the result.
type ImmediateDeathPersistenceResult struct {
	Recovered bool
	Delivery  sim.DeathCompletionDisposition
	Err       error
}

// defaultDeathExecutorRetryDelay is the deterministic pause
// between ErrSimIngressFull redeliveries of the SAME frozen
// completion on the same fixed worker: bounded resources, no
// goroutine per retry, no busy-spin.
const defaultDeathExecutorRetryDelay = 5 * time.Millisecond

// DeathExecutorConfig validates worker count, queue capacity,
// and required dependencies. RetryDelay controls the
// ingress-full redelivery pause; non-positive selects the
// default. Exact field names follow repository conventions.
type DeathExecutorConfig struct {
	Workers       int
	QueueCapacity int
	Store         DeathExecutionStore
	Saver         *sim.Saver
	Sink          ImmediateDeathCompletionSink
	RetryDelay    time.Duration
}

// deathExecutorJob is the frozen per-job state: the Store
// request and the future owner completion, both fully owned by
// the executor (caller mutation after successful submission
// cannot reach them), plus the buffered result channel (cap 1
// so workers never wait for the caller to receive).
type deathExecutorJob struct {
	req        store.DeathEntryRequest
	completion sim.ImmediateDeathCompletion
	res        chan ImmediateDeathPersistenceResult
}

// DeathExecutor is the fixed bounded death-persistence
// executor: fixed worker count, bounded job queue, NO
// goroutine per death, NO unbounded queue, NO unbounded
// completion queue, explicit Run lifecycle. Construction
// itself leaks no goroutines.
type DeathExecutor struct {
	store      DeathExecutionStore
	saver      *sim.Saver
	sink       ImmediateDeathCompletionSink
	retryDelay time.Duration
	queue      chan deathExecutorJob

	mu         sync.Mutex
	running    bool
	started    bool
	ready      chan struct{}
	numWorkers int
}

// NewDeathExecutor validates configuration and builds a
// non-running executor. Workers start only in Run.
func NewDeathExecutor(cfg DeathExecutorConfig) (*DeathExecutor, error) {
	if cfg.Workers <= 0 {
		return nil, fmt.Errorf("persist: death executor workers=%d: %w", cfg.Workers, ErrDeathExecutorInvalid)
	}
	if cfg.QueueCapacity <= 0 {
		return nil, fmt.Errorf("persist: death executor queue capacity=%d: %w", cfg.QueueCapacity, ErrDeathExecutorInvalid)
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("persist: death executor nil store: %w", ErrDeathExecutorInvalid)
	}
	if cfg.Saver == nil {
		return nil, fmt.Errorf("persist: death executor nil saver: %w", ErrDeathExecutorInvalid)
	}
	if cfg.Sink == nil {
		return nil, fmt.Errorf("persist: death executor nil sink: %w", ErrDeathExecutorInvalid)
	}
	delay := cfg.RetryDelay
	if delay <= 0 {
		delay = defaultDeathExecutorRetryDelay
	}
	return &DeathExecutor{
		store:      cfg.Store,
		saver:      cfg.Saver,
		sink:       cfg.Sink,
		retryDelay: delay,
		queue:      make(chan deathExecutorJob, cfg.QueueCapacity),
		ready:      make(chan struct{}),
		numWorkers: cfg.Workers,
	}, nil
}

// Run owns the fixed workers until ctx is cancelled: it stops
// accepting new jobs, fails every queued-but-not-started job
// with a definitive shutdown error, lets running
// Store/recovery work observe the cancelled context, waits for
// workers, and returns. No waiter is stranded; no worker
// leaks. A second concurrent Run fails. The executor is
// one-shot: any Run after the first Run has terminated fails
// with ErrDeathExecutorShutdown without starting workers and
// without closing ready again.
func (x *DeathExecutor) Run(ctx context.Context) error {
	x.mu.Lock()
	if x.running {
		x.mu.Unlock()
		return ErrDeathExecutorAlreadyRunning
	}
	if x.started {
		x.mu.Unlock()
		return ErrDeathExecutorShutdown
	}
	x.running = true
	x.started = true
	close(x.ready)
	x.mu.Unlock()

	var wg sync.WaitGroup
	for range x.numWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x.worker(ctx)
		}()
	}
	<-ctx.Done()
	x.mu.Lock()
	x.running = false
	x.mu.Unlock()
	for {
		select {
		case job := <-x.queue:
			job.res <- ImmediateDeathPersistenceResult{Err: ErrDeathExecutorShutdown}
		default:
			wg.Wait()
			return ctx.Err()
		}
	}
}

// TrySubmit validates, maps, and deep-freezes work, then
// publishes it without blocking: at submit time (before queue
// publication) it runs MapImmediateDeathCapture, freezes the
// resulting Store request, and freezes the future
// ImmediateDeathCompletion payload. Caller mutation after
// successful submission MUST NOT affect the job. A full queue
// fails immediately with the stable full error; a non-running
// executor fails with the stable not-running error. The exact
// c3c3c reservation-before-DeathPersisting API is out of scope:
// T5c3c3c MUST NOT use naive TrySubmit-after-begin semantics.
func (x *DeathExecutor) TrySubmit(
	work ImmediateDeathPersistenceWork,
) (<-chan ImmediateDeathPersistenceResult, error) {
	if err := work.RuntimeInputs.Validate(); err != nil {
		return nil, fmt.Errorf("persist: death executor work runtime inputs: %w: %w",
			err, ErrDeathExecutorInvalid)
	}
	req, err := MapImmediateDeathCapture(work.Capture)
	if err != nil {
		return nil, fmt.Errorf("persist: death executor work capture: %w: %w",
			err, ErrDeathExecutorInvalid)
	}
	// Defensive second freeze: the mapper already owns its
	// output, and the T5c2b adapter freezes again before
	// callback execution. Freeze here too so the queued job
	// can never alias caller memory even if the mapper drifts.
	req = freezeDeathEntryRequest(req)
	completion := sim.CloneImmediateDeathCompletion(sim.ImmediateDeathCompletion{
		Token:         work.Capture.Token,
		Placement:     work.Capture.Placement,
		Vitals:        work.Capture.Vitals,
		RuntimeInputs: work.RuntimeInputs,
		Durable:       work.Capture.Durable,
	})
	res := make(chan ImmediateDeathPersistenceResult, 1)
	job := deathExecutorJob{req: req, completion: completion, res: res}
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.running {
		return nil, ErrDeathExecutorNotRunning
	}
	select {
	case x.queue <- job:
		return res, nil
	default:
		return nil, ErrDeathExecutorQueueFull
	}
}

func (x *DeathExecutor) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-x.queue:
			if ctx.Err() != nil {
				job.res <- ImmediateDeathPersistenceResult{Err: ErrDeathExecutorShutdown}
				return
			}
			job.res <- x.execute(ctx, job)
		}
	}
}

// execute runs one frozen job to its definitive bounded
// result: normal CommitDeathEntry acknowledgement, proven
// materialized recovery, or a fail-closed error. It NEVER
// replays CommitDeathEntry and NEVER mutates live sim state.
func (x *DeathExecutor) execute(
	ctx context.Context,
	job deathExecutorJob,
) ImmediateDeathPersistenceResult {
	exec, err := commitDeathEntryObserved(ctx, x.saver, x.store, job.req)
	if err == nil {
		// Normal success: no PG reload, no ReconcileSaver,
		// no Store retry. The frozen planned completion is
		// authoritative.
		return x.deliver(ctx, job.completion, false)
	}
	if !errors.Is(err, sim.ErrSaverReconcileRequired) {
		// Failed entirely before the critical callback /
		// without a reconcile block: return the error, no
		// PG reload, no completion delivery.
		return ImmediateDeathPersistenceResult{Err: err}
	}
	rec, rerr := x.recover(ctx, job.req, exec.Expected, err)
	if rerr != nil {
		return ImmediateDeathPersistenceResult{Err: rerr}
	}
	if perr := proveDeathCommit(job.req, exec.Expected, rec, err); perr != nil {
		return ImmediateDeathPersistenceResult{Err: perr}
	}
	return x.deliver(ctx, job.completion, true)
}

// deathRecovery is the worker-local materialized state for one
// job: the character snapshot plus every affected item
// snapshot in ascending durable ItemID order. Staged values
// only — never live entity memory.
type deathRecovery struct {
	char  store.DeathCharacterRecoverySnapshot
	items []store.DeathItemRecoverySnapshot
}

// recover reconciles EVERY participant (character root AND
// every affected item root, deterministic ascending ItemID
// order) through worker-local ReconcileState values built from
// the ACTUAL callback expected revisions plus the existing
// T5c2a DeathCharacterReload / DeathItemReload / ReconcileSaver
// adapters. Apply closures write only into worker-local staged
// values. Every participant is attempted even if one fails
// where practical; a deterministic joined error results. A
// partially recovered set MUST NOT produce completion (the
// caller proves before delivering).
func (x *DeathExecutor) recover(
	ctx context.Context,
	req store.DeathEntryRequest,
	expected []sim.AggregateRevision,
	commitErr error,
) (deathRecovery, error) {
	var out deathRecovery
	var errs []error
	charKey := deathCharacterKey(req.Character.ID)
	charExp, ok := revisionByKey(expected, charKey)
	if !ok {
		errs = append(errs, fmt.Errorf(
			"persist: death recovery character %d without callback expected revision: %w",
			req.Character.ID, commitErr))
	} else {
		var staged store.DeathCharacterRecoverySnapshot
		stagedOK := false
		state, serr := sim.NewReconcileState(charExp)
		if serr != nil {
			errs = append(errs, fmt.Errorf("persist: death recovery character %d state: %w",
				req.Character.ID, serr))
		} else if rerr := ReconcileSaver(ctx, state, x.saver, charKey,
			DeathCharacterReload(x.store, req.Character.ID,
				func(s store.DeathCharacterRecoverySnapshot) error {
					staged = s
					stagedOK = true
					return nil
				})); rerr != nil {
			errs = append(errs, fmt.Errorf("persist: death recovery character %d: %w",
				req.Character.ID, rerr))
		} else if !stagedOK {
			errs = append(errs, fmt.Errorf("persist: death recovery character %d staged nothing: %w",
				req.Character.ID, commitErr))
		} else {
			out.char = staged
		}
	}
	ordered := append([]store.DeathEntryItem(nil), req.Items...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].Snapshot.ID < ordered[j].Snapshot.ID
	})
	for _, it := range ordered {
		itemID := it.Snapshot.ID
		itemKey := deathItemKey(itemID)
		itemExp, ok := revisionByKey(expected, itemKey)
		if !ok {
			errs = append(errs, fmt.Errorf(
				"persist: death recovery item %d without callback expected revision: %w",
				itemID, commitErr))
			continue
		}
		var staged store.DeathItemRecoverySnapshot
		stagedOK := false
		state, serr := sim.NewReconcileState(itemExp)
		if serr != nil {
			errs = append(errs, fmt.Errorf("persist: death recovery item %d state: %w",
				itemID, serr))
			continue
		}
		if rerr := ReconcileSaver(ctx, state, x.saver, itemKey,
			DeathItemReload(x.store, itemID,
				func(s store.DeathItemRecoverySnapshot) error {
					staged = s
					stagedOK = true
					return nil
				})); rerr != nil {
			errs = append(errs, fmt.Errorf("persist: death recovery item %d: %w",
				itemID, rerr))
			continue
		}
		if !stagedOK {
			errs = append(errs, fmt.Errorf("persist: death recovery item %d staged nothing: %w",
				itemID, commitErr))
			continue
		}
		out.items = append(out.items, staged)
	}
	if len(errs) > 0 {
		return deathRecovery{}, errors.Join(errs...)
	}
	return out, nil
}

// proveDeathCommit conservatively classifies whether the
// original transaction is PROVEN committed: for EVERY
// participant the recovered root revision MUST equal exactly
// callbackExpectedRevision + 1 (not >=), the recovered content
// MUST semantically equal the frozen intended snapshot, and
// the pending-death shape MUST match the frozen route. Any
// deviation returns ErrDeathCommitUnproven (wrapping the
// original commit error as context): NO replay, NO owner
// completion, the player remains DeathPersisting.
func proveDeathCommit(
	req store.DeathEntryRequest,
	expected []sim.AggregateRevision,
	rec deathRecovery,
	commitErr error,
) error {
	unproven := func(format string, args ...any) error {
		msg := fmt.Sprintf("persist: death commit unproven: "+format, args...)
		return fmt.Errorf("%s: %w", msg, errors.Join(ErrDeathCommitUnproven, commitErr))
	}
	charKey := deathCharacterKey(req.Character.ID)
	charExp, ok := revisionByKey(expected, charKey)
	if !ok {
		return unproven("character %d without callback expected revision", req.Character.ID)
	}
	if rec.char.Character.ExpectedRevision != charExp+1 {
		return unproven("character %d revision %d, want expected+1 %d",
			req.Character.ID, rec.char.Character.ExpectedRevision, charExp+1)
	}
	if err := equalDeathCharacterContent(req.Character, rec.char.Character); err != nil {
		var mismatch *deathProofMismatch
		if errors.As(err, &mismatch) {
			return unproven("character %d content: %s", req.Character.ID, mismatch.msg)
		}
		return fmt.Errorf("persist: death proof character %d: %w", req.Character.ID, err)
	}
	recoveredByID := make(map[int64]store.DeathItemRecoverySnapshot, len(rec.items))
	for _, it := range rec.items {
		recoveredByID[it.Item.ID] = it
	}
	if len(recoveredByID) != len(rec.items) {
		return unproven("duplicate recovered item roots")
	}
	if len(recoveredByID) != len(req.Items) {
		return unproven("recovered %d items, want %d", len(recoveredByID), len(req.Items))
	}
	ordered := append([]store.DeathEntryItem(nil), req.Items...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].Snapshot.ID < ordered[j].Snapshot.ID
	})
	for _, want := range ordered {
		itemID := want.Snapshot.ID
		got, ok := recoveredByID[itemID]
		if !ok {
			return unproven("affected item %d not recovered", itemID)
		}
		itemExp, ok := revisionByKey(expected, deathItemKey(itemID))
		if !ok {
			return unproven("item %d without callback expected revision", itemID)
		}
		if got.Item.ExpectedRevision != itemExp+1 {
			return unproven("item %d revision %d, want expected+1 %d",
				itemID, got.Item.ExpectedRevision, itemExp+1)
		}
		if err := equalDeathItemContent(req.Character.ID, want, got); err != nil {
			var mismatch *deathProofMismatch
			if errors.As(err, &mismatch) {
				return unproven("item %d content: %s", itemID, mismatch.msg)
			}
			return fmt.Errorf("persist: death proof item %d: %w", itemID, err)
		}
	}
	if err := equalDeathPendingShape(req, rec.char.Pending); err != nil {
		var mismatch *deathProofMismatch
		if errors.As(err, &mismatch) {
			return unproven("pending: %s", mismatch.msg)
		}
		return fmt.Errorf("persist: death proof pending: %w", err)
	}
	return nil
}

// deathProofMismatch marks semantic inequality between
// intended and recovered materialized state (maps to
// ErrDeathCommitUnproven). Preparation failures (unparseable
// JSON) are plain errors instead: the proof cannot run, so
// recovery fails rather than merely staying unproven.
type deathProofMismatch struct{ msg string }

func (e *deathProofMismatch) Error() string { return e.msg }

// semanticJSONEqual compares JSON SEMANTICS (decoded values
// with json.Number), never raw bytes: PostgreSQL JSONB may
// normalize whitespace and key order. Unknown keys remain
// significant (the complete decoded object compares).
// Malformed input is a preparation failure (error), not
// inequality.
func semanticJSONEqual(a, b []byte) (bool, error) {
	decode := func(data []byte) (any, error) {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if dec.More() {
			return nil, fmt.Errorf("trailing JSON data")
		}
		return v, nil
	}
	va, err := decode(a)
	if err != nil {
		return false, fmt.Errorf("intended JSON: %w", err)
	}
	vb, err := decode(b)
	if err != nil {
		return false, fmt.Errorf("recovered JSON: %w", err)
	}
	return reflect.DeepEqual(va, vb), nil
}

// equalDeathCharacterContent proves the recovered character
// root semantically equals the frozen intended snapshot,
// excluding only ExpectedRevision: ID, Karma, PosX/Y/Z,
// Flags, Spells, Skills, Vitals JSON, Advancement JSON.
// Spells/skills compare semantically by durable catalog ID
// (order-insensitive; duplicates rejected, never hidden).
func equalDeathCharacterContent(
	intended, recovered store.CharacterSnapshot,
) error {
	mismatch := func(format string, args ...any) error {
		return &deathProofMismatch{msg: fmt.Sprintf(format, args...)}
	}
	if recovered.ID != intended.ID {
		return mismatch("id %d, want %d", recovered.ID, intended.ID)
	}
	if recovered.Karma != intended.Karma {
		return mismatch("karma %d, want %d", recovered.Karma, intended.Karma)
	}
	if recovered.PosX != intended.PosX || recovered.PosY != intended.PosY || recovered.PosZ != intended.PosZ {
		return mismatch("pos %d/%d/%d, want %d/%d/%d",
			recovered.PosX, recovered.PosY, recovered.PosZ,
			intended.PosX, intended.PosY, intended.PosZ)
	}
	if recovered.Flags != intended.Flags {
		return mismatch("flags %#x, want %#x", recovered.Flags, intended.Flags)
	}
	type ability struct {
		ability int16
		atrophy bool
	}
	collectSpells := func(snaps []store.CharacterSpellSnapshot) (map[int32]ability, error) {
		out := make(map[int32]ability, len(snaps))
		for _, s := range snaps {
			if _, dup := out[s.SpellID]; dup {
				return nil, mismatch("duplicate spell id %d", s.SpellID)
			}
			out[s.SpellID] = ability{ability: s.Ability, atrophy: s.AtrophyFlag}
		}
		return out, nil
	}
	collectSkills := func(snaps []store.CharacterSkillSnapshot) (map[int32]ability, error) {
		out := make(map[int32]ability, len(snaps))
		for _, s := range snaps {
			if _, dup := out[s.SkillID]; dup {
				return nil, mismatch("duplicate skill id %d", s.SkillID)
			}
			out[s.SkillID] = ability{ability: s.Ability, atrophy: s.AtrophyFlag}
		}
		return out, nil
	}
	wantSpells, err := collectSpells(intended.Spells)
	if err != nil {
		return err
	}
	gotSpells, err := collectSpells(recovered.Spells)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(gotSpells, wantSpells) {
		return mismatch("spells %+v, want %+v", recovered.Spells, intended.Spells)
	}
	wantSkills, err := collectSkills(intended.Skills)
	if err != nil {
		return err
	}
	gotSkills, err := collectSkills(recovered.Skills)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(gotSkills, wantSkills) {
		return mismatch("skills %+v, want %+v", recovered.Skills, intended.Skills)
	}
	eq, err := semanticJSONEqual(intended.Vitals, recovered.Vitals)
	if err != nil {
		return err
	}
	if !eq {
		return mismatch("vitals %s, want %s", string(recovered.Vitals), string(intended.Vitals))
	}
	eq, err = semanticJSONEqual(intended.Advancement, recovered.Advancement)
	if err != nil {
		return err
	}
	if !eq {
		return mismatch("advancement %s, want %s", string(recovered.Advancement), string(intended.Advancement))
	}
	return nil
}

// equalDeathItemContent proves one recovered item root equals
// the intended death-entry item snapshot, excluding only
// ExpectedRevision: ID, Qty, Hits, Enchants JSON semantics,
// and the complete ItemLocationSnapshot against the resolved
// ground position. Pointer/value presence is significant (nil
// is never equal to a zero pointer, an empty string pointer,
// or any ownership/container/vault/slot difference). PK
// protection follows the frozen treatment: zero duration means
// "no new write" (a pre-existing row MUST NOT fail the proof),
// while a positive duration requires a recovered row naming
// the affected item and the victim. ExpiresAt is never
// compared for equality.
func equalDeathItemContent(
	victimCharacterID int64,
	intended store.DeathEntryItem,
	recovered store.DeathItemRecoverySnapshot,
) error {
	mismatch := func(format string, args ...any) error {
		return &deathProofMismatch{msg: fmt.Sprintf(format, args...)}
	}
	if recovered.Item.ID != intended.Snapshot.ID {
		return mismatch("id %d, want %d", recovered.Item.ID, intended.Snapshot.ID)
	}
	if recovered.Item.Qty != intended.Snapshot.Qty {
		return mismatch("qty %d, want %d", recovered.Item.Qty, intended.Snapshot.Qty)
	}
	if recovered.Item.Hits != intended.Snapshot.Hits {
		return mismatch("hits %d, want %d", recovered.Item.Hits, intended.Snapshot.Hits)
	}
	eq, err := semanticJSONEqual(intended.Snapshot.Enchants, recovered.Item.Enchants)
	if err != nil {
		return err
	}
	if !eq {
		return mismatch("enchants %s, want %s",
			string(recovered.Item.Enchants), string(intended.Snapshot.Enchants))
	}
	if !reflect.DeepEqual(recovered.Item.Location, intended.Snapshot.Location) {
		return mismatch("location %+v, want %+v",
			recovered.Item.Location, intended.Snapshot.Location)
	}
	if intended.PKProtectionDuration == 0 {
		return nil
	}
	prot := recovered.PKProtection
	if prot == nil {
		return mismatch("missing PK protection for duration %s", intended.PKProtectionDuration)
	}
	if prot.ItemID != intended.Snapshot.ID {
		return mismatch("protection item %d, want %d", prot.ItemID, intended.Snapshot.ID)
	}
	if prot.VictimCharacterID != victimCharacterID {
		return mismatch("protection victim %d, want %d", prot.VictimCharacterID, victimCharacterID)
	}
	return nil
}

// equalDeathPendingShape validates the recovered pending-death
// child against the planned death route. Newbie-home direct
// respawn requires Pending == nil. Every Underworld-bound real
// death requires a non-nil row with exactly the frozen cost,
// death time, and PortalUsed == false. CorpseID is NEVER a
// strict commit-proof identity: the pending row may outlive
// corpse expiry via ON DELETE SET NULL, so a nil recovered
// CorpseID is allowed and no corpse lookup is invented.
func equalDeathPendingShape(
	req store.DeathEntryRequest,
	pending *store.PendingDeathSnapshot,
) error {
	mismatch := func(format string, args ...any) error {
		return &deathProofMismatch{msg: fmt.Sprintf(format, args...)}
	}
	if req.NewbieHomeRespawn {
		if pending != nil {
			return mismatch("newbie-home pending %+v, want nil", pending)
		}
		return nil
	}
	if pending == nil {
		return mismatch("missing pending row for Underworld-bound death")
	}
	if pending.CharacterID != req.Character.ID {
		return mismatch("pending character %d, want %d", pending.CharacterID, req.Character.ID)
	}
	if pending.EffectiveCost != req.EffectiveDeathCost {
		return mismatch("pending cost %d, want %d", pending.EffectiveCost, req.EffectiveDeathCost)
	}
	if pending.DeathTimeSeconds != req.DeathTimeSeconds {
		return mismatch("pending death time %d, want %d",
			pending.DeathTimeSeconds, req.DeathTimeSeconds)
	}
	if pending.PortalUsed {
		return mismatch("pending portal used, want false")
	}
	return nil
}

// deliver sends the frozen completion through typed owner
// ingress. A nil-error Applied is success; a nil-error
// Duplicate is ALSO success (the expected idempotent
// redelivery result — no second live-state apply occurs). If
// delivery returns ErrSimIngressFull the persistence
// transaction is already authoritative, so the SAME frozen
// completion is retried/redelivered on the same fixed worker
// with context-aware bounded retry (no goroutine per retry, no
// unbounded retry queue, no busy-spin). Engine-stop, unknown
// entity, attempt-mismatch, handoff, and payload/install
// validation errors are terminal: the delivery error returns
// with no CharacterID fallback lookup and no live-state
// mutation from the worker. The committed PG state remains
// authoritative for reconnect/restart recovery.
func (x *DeathExecutor) deliver(
	ctx context.Context,
	completion sim.ImmediateDeathCompletion,
	recovered bool,
) ImmediateDeathPersistenceResult {
	for {
		_, disp, err := x.sink.EnqueueImmediateDeathCompletion(ctx, completion)
		if err == nil {
			return ImmediateDeathPersistenceResult{Recovered: recovered, Delivery: disp}
		}
		if !errors.Is(err, sim.ErrSimIngressFull) {
			return ImmediateDeathPersistenceResult{Err: err}
		}
		timer := time.NewTimer(x.retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ImmediateDeathPersistenceResult{Err: ctx.Err()}
		case <-timer.C:
		}
	}
}

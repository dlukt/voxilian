package persist

import (
	"context"
	"errors"
	"fmt"
	"testing"

	randv2 "math/rand/v2"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
)

var (
	errModelTransient = errors.New("persist property transient")
	errModelLostAck   = errors.New("persist property lost acknowledgement")
	errModelLoad      = errors.New("persist property load failure")
)

type modelExec struct {
	key       sim.AggregateKey
	exp       int64
	ok        bool // durable write committed
	newRev    int64
	lostAck   bool // committed but acknowledgement lost
	stale     bool
	transient bool
}

// modelStore is a scriptable in-memory SnapshotStore + BankLoader:
// exact-revision CAS, one-shot transient failures, one-shot lost
// acknowledgements, and one-shot loader failures.
type modelStore struct {
	rev         map[sim.AggregateKey]int64
	val         map[sim.AggregateKey]int64
	failNext    error
	loseAckNext bool
	failLoad    bool
	lostAckHits int
	execs       []modelExec
	execCount   map[sim.AggregateKey]int
}

func newModelStore() *modelStore {
	return &modelStore{
		rev:       map[sim.AggregateKey]int64{},
		val:       map[sim.AggregateKey]int64{},
		execCount: map[sim.AggregateKey]int{},
	}
}

func (m *modelStore) save(key sim.AggregateKey, exp, val int64) (int64, error) {
	m.execCount[key]++
	rec := modelExec{key: key, exp: exp}
	defer func() { m.execs = append(m.execs, rec) }()
	if m.failNext != nil {
		err := m.failNext
		m.failNext = nil
		rec.transient = true
		return 0, err
	}
	if exp != m.rev[key] {
		rec.stale = true
		return 0, fmt.Errorf("model save %v: %w", key, store.ErrStaleRevision)
	}
	m.rev[key]++
	m.val[key] = val
	rec.ok = true
	rec.newRev = m.rev[key]
	if m.loseAckNext {
		m.loseAckNext = false
		m.lostAckHits++
		rec.lostAck = true
		return 0, errModelLostAck
	}
	return m.rev[key], nil
}

func (m *modelStore) SaveCharacterSnapshot(_ context.Context, snap store.CharacterSnapshot) (int64, error) {
	return m.save(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: snap.ID}, snap.ExpectedRevision, int64(snap.Karma))
}

func (m *modelStore) SaveItemSnapshot(_ context.Context, snap store.ItemSnapshot) (int64, error) {
	return m.save(sim.AggregateKey{Kind: sim.AggregateItem, ID: snap.ID}, snap.ExpectedRevision, int64(snap.Qty))
}

func (m *modelStore) SaveBankBalance(_ context.Context, snap store.BankSnapshot) (int64, error) {
	return m.save(sim.AggregateKey{Kind: sim.AggregateBank, ID: snap.CharacterID, Scope: snap.System}, snap.ExpectedRevision, snap.Balance)
}

func (m *modelStore) LoadBankBalance(_ context.Context, charID int64, system string) (store.BankSnapshot, error) {
	if m.failLoad {
		m.failLoad = false
		return store.BankSnapshot{}, errModelLoad
	}
	k := sim.AggregateKey{Kind: sim.AggregateBank, ID: charID, Scope: system}
	return store.BankSnapshot{CharacterID: charID, System: system, ExpectedRevision: m.rev[k], Balance: m.val[k]}, nil
}

// buildJob constructs a production adapter job carrying memVal.
func buildModelJob(t *testing.T, st SnapshotStore, key sim.AggregateKey, memVal int64) SnapshotJob {
	t.Helper()
	var job SnapshotJob
	var err error
	switch key.Kind {
	case sim.AggregateCharacter:
		job, err = NewCharacterSnapshotJob(st, store.CharacterSnapshot{
			ID: key.ID, Karma: int32(memVal),
			Vitals: []byte(`{}`), Advancement: []byte(`{}`),
		})
	case sim.AggregateItem:
		slot := "s"
		job, err = NewItemSnapshotJob(st, store.ItemSnapshot{
			ID: key.ID, Qty: int32(memVal), Enchants: []byte(`{}`),
			Location: store.ItemLocationSnapshot{Kind: 0, CharacterID: &key.ID, Slot: &slot},
		})
	case sim.AggregateBank:
		job, err = NewBankSnapshotJob(st, store.BankSnapshot{
			CharacterID: key.ID, System: key.Scope, Balance: memVal,
		})
	}
	if err != nil {
		t.Fatalf("buildJob(%v): %v", key, err)
	}
	if job.Key != key {
		t.Fatalf("job key = %v, want %v", job.Key, key)
	}
	return job
}

func TestPersistBridgeProperty(t *testing.T) {
	const seeds = 64
	const actions = 64
	var coverAdvance, coverStale, coverReload, coverPostRecon, coverLostAck int
	for seed := 0; seed < seeds; seed++ {
		rng := randv2.New(randv2.NewPCG(uint64(seed)+11, uint64(seed)*97+3))
		ms := newModelStore()
		s := mustSaverForPersist(t)
		keys := []sim.AggregateKey{
			{Kind: sim.AggregateCharacter, ID: 1},
			{Kind: sim.AggregateItem, ID: 2},
			{Kind: sim.AggregateBank, ID: 3, Scope: "tos"},
		}
		type km struct {
			durableRev, durableVal int64
			memRev, memVal         int64
			saverKnown             int64
			saverPending           bool
			saverBlocked           bool
			t3c                    *sim.ReconcileState
			callsAtBlock           int
			blockedSeen            bool
		}
		model := map[sim.AggregateKey]*km{}
		for _, k := range keys {
			if err := s.Track(k, 0); err != nil {
				t.Fatal(err)
			}
			model[k] = &km{t3c: mustReconcileState(t, 0)}
		}
		ctx := context.Background()
		drain := func() []modelExec {
			out := ms.execs
			ms.execs = nil
			return out
		}
		// applyExec replays one logged saver execution into the
		// saver-side model. viaWT selects write-through rules.
		applyExec := func(r modelExec, viaWT bool) {
			m := model[r.key]
			if r.exp != m.saverKnown {
				t.Fatalf("seed %d: key %v executed exp %d, saver known %d (guessed revision)",
					seed, r.key, r.exp, m.saverKnown)
			}
			switch {
			case r.ok && !r.lostAck:
				m.saverKnown = r.newRev
				m.durableRev, m.durableVal = r.newRev, ms.val[r.key]
				m.saverPending = false
			case r.ok && r.lostAck:
				m.durableRev, m.durableVal = r.newRev, ms.val[r.key]
				m.saverPending = true
			case r.transient:
				m.saverPending = true
			case r.stale:
				m.saverBlocked = true
				m.blockedSeen = true
				m.callsAtBlock = ms.execCount[r.key]
				if !viaWT {
					m.saverPending = false
				}
			}
		}
		check := func(op string) {
			t.Helper()
			dirty := 0
			for _, k := range keys {
				m := model[k]
				snap, err := s.Inspect(k)
				if err != nil {
					t.Fatalf("seed %d op %s: inspect: %v", seed, op, err)
				}
				if snap.KnownRevision != m.saverKnown || snap.Dirty != m.saverPending || snap.Blocked != m.saverBlocked {
					t.Fatalf("seed %d op %s key %v: saver %+v model known%d pending%v blocked%v",
						seed, op, k, snap, m.saverKnown, m.saverPending, m.saverBlocked)
				}
				if m.saverPending {
					dirty++
				}
				// Invariants (spec A70).
				if m.saverKnown > ms.rev[k] {
					t.Fatalf("seed %d: saver known %d exceeds durable %d",
						seed, m.saverKnown, ms.rev[k])
				}
				if m.blockedSeen && m.saverBlocked && ms.execCount[k] != m.callsAtBlock {
					t.Fatalf("seed %d: blocked key %v executed again", seed, k)
				}
				rs := m.t3c.Snapshot()
				if rs.KnownRevision > ms.rev[k] {
					t.Fatalf("seed %d: t3c known exceeds durable", seed)
				}
			}
			if got := s.DirtyCount(); got != dirty {
				t.Fatalf("seed %d op %s: DirtyCount %d, model %d", seed, op, got, dirty)
			}
		}
		bridge := func(k sim.AggregateKey) error {
			m := model[k]
			var reload sim.ReloadFunc
			if k.Kind == sim.AggregateBank {
				reload = BankReload(ms, k.ID, k.Scope, func(snap store.BankSnapshot) error {
					m.memRev, m.memVal = snap.ExpectedRevision, snap.Balance
					return nil
				})
			} else {
				reload = func(context.Context) (sim.ReloadCandidate, error) {
					if ms.failLoad {
						ms.failLoad = false
						return sim.ReloadCandidate{}, errModelLoad
					}
					rev, val := ms.rev[k], ms.val[k]
					return sim.ReloadCandidate{Revision: rev, Apply: func() error {
						m.memRev, m.memVal = rev, val
						return nil
					}}, nil
				}
			}
			return ReconcileSaver(ctx, m.t3c, s, k, reload)
		}
		for step := 0; step < actions; step++ {
			k := keys[rng.IntN(len(keys))]
			m := model[k]
			switch rng.IntN(14) {
			case 0, 1, 2, 3, 4: // mutate + mark (owner discipline: reconcile first)
				if m.t3c.Snapshot().Pending {
					coverReload++
					if err := bridge(k); err != nil {
						break // loader failed: mutation stays blocked.
					}
					m.saverKnown = m.t3c.Snapshot().KnownRevision
					m.saverPending, m.saverBlocked = false, false
					coverPostRecon++
				}
				if m.t3c.Snapshot().Pending {
					break
				}
				m.memVal += int64(rng.IntN(10) + 1)
				m.memRev = m.saverKnown
				job := buildModelJob(t, ms, k, m.memVal)
				if m.saverBlocked {
					if err := s.MarkDirty(k, job.Write); !errors.Is(err, sim.ErrSaverReconcileRequired) {
						t.Fatalf("seed %d: blocked mark err = %v", seed, err)
					}
					break
				}
				if err := s.MarkDirty(k, job.Write); err != nil {
					t.Fatalf("seed %d: mark: %v", seed, err)
				}
				m.saverPending = true
			case 5, 6, 7: // flush
				_ = s.FlushDirty(ctx)
				for _, r := range drain() {
					applyExec(r, false)
					if r.stale {
						coverStale++
					}
				}
			case 8: // write-through current memory
				job := buildModelJob(t, ms, k, m.memVal)
				_, wtErr := s.WriteThrough(ctx, k, job.Write)
				execs := drain()
				if m.saverBlocked {
					if !errors.Is(wtErr, sim.ErrSaverReconcileRequired) {
						t.Fatalf("seed %d: blocked wt err = %v", seed, wtErr)
					}
					if len(execs) != 0 {
						t.Fatalf("seed %d: blocked wt executed", seed)
					}
					break
				}
				if len(execs) != 1 {
					t.Fatalf("seed %d: wt execs = %d", seed, len(execs))
				}
				applyExec(execs[0], true)
				if execs[0].stale {
					coverStale++
				}
			case 9: // external durable advance + T3c fence
				ms.rev[k]++
				ms.val[k] += int64(rng.IntN(50) + 1)
				m.durableRev, m.durableVal = ms.rev[k], ms.val[k]
				if err := m.t3c.MarkCommitted(m.durableRev); err != nil {
					t.Fatalf("seed %d: markcommitted: %v", seed, err)
				}
				coverAdvance++
			case 10: // arm lost acknowledgement
				ms.loseAckNext = true
			case 11: // bridge reconcile
				coverReload++
				if err := bridge(k); err != nil {
					if !m.t3c.Snapshot().Pending {
						t.Fatalf("seed %d: failed bridge cleared t3c", seed)
					}
					break
				}
				m.saverKnown = m.t3c.Snapshot().KnownRevision
				m.saverPending, m.saverBlocked = false, false
				m.memRev = m.saverKnown
				// Reconcile success aligns memory + T3c + saver.
				if m.memRev != m.durableRev || m.memVal != m.durableVal {
					t.Fatalf("seed %d: post-reconcile mem %d/rev%d, durable %d/rev%d",
						seed, m.memVal, m.memRev, m.durableVal, m.durableRev)
				}
				coverPostRecon++
			default: // arm store/loader failures
				if rng.IntN(2) == 0 {
					ms.failNext = errModelTransient
				} else {
					ms.failLoad = true
				}
			}
			if ms.lostAckHits > 0 {
				coverLostAck += ms.lostAckHits
				ms.lostAckHits = 0
			}
			check("step")
		}
		// End-of-seed convergence: reconcile + flush until aligned.
		for round := 0; round < 10; round++ {
			aligned := true
			for _, k := range keys {
				m := model[k]
				if m.saverBlocked || m.t3c.Snapshot().Pending || m.saverPending {
					if err := bridge(k); err == nil {
						m.saverKnown = m.t3c.Snapshot().KnownRevision
						m.saverPending, m.saverBlocked = false, false
						m.memRev = m.saverKnown
					}
				}
			}
			_ = s.FlushDirty(ctx)
			for _, r := range drain() {
				applyExec(r, false)
			}
			for _, k := range keys {
				m := model[k]
				snap, _ := s.Inspect(k)
				rs := m.t3c.Snapshot()
				// Saver may still be dirty only if a transient hit;
				// loop again. Stale must not appear: memory is current.
				if snap.Blocked || rs.Pending {
					aligned = false
				}
				if m.memVal != m.durableVal {
					// Memory behind: re-mark current memory and flush.
					m.memVal = m.durableVal
					m.memRev = m.durableRev
					job := buildModelJob(t, ms, k, m.memVal)
					if err := s.MarkDirty(k, job.Write); err == nil {
						m.saverPending = true
					}
					aligned = false
				}
			}
			check("converge")
			if aligned && s.DirtyCount() == 0 {
				break
			}
		}
		for _, k := range keys {
			m := model[k]
			snap, _ := s.Inspect(k)
			if snap.Blocked || m.t3c.Snapshot().Pending {
				t.Fatalf("seed %d: key %v did not converge: %+v", seed, k, snap)
			}
			if m.saverKnown != m.durableRev {
				t.Fatalf("seed %d: key %v saver %d durable %d", seed, k, m.saverKnown, m.durableRev)
			}
		}
	}
	if coverAdvance == 0 || coverStale == 0 || coverReload == 0 || coverPostRecon == 0 || coverLostAck == 0 {
		t.Fatalf("coverage missing: advance=%d stale=%d reload=%d postrecon=%d lostack=%d",
			coverAdvance, coverStale, coverReload, coverPostRecon, coverLostAck)
	}
	t.Logf("coverage: advance=%d stale=%d reload=%d postrecon=%d lostack=%d",
		coverAdvance, coverStale, coverReload, coverPostRecon, coverLostAck)
}

// TestPersistAdapterImmutabilityProperty mutates randomized
// character/item inputs after job construction; the fake Store must
// always receive the frozen captured values.
func TestPersistAdapterImmutabilityProperty(t *testing.T) {
	for iter := 0; iter < 64; iter++ {
		rng := randv2.New(randv2.NewPCG(uint64(iter)+101, 7))
		fs := newFakeSnapshotStore()
		// Randomized character snapshot.
		vitals := []byte(fmt.Sprintf(`{"hp":%d}`, rng.IntN(100)))
		adv := []byte(fmt.Sprintf(`{"pts":%d}`, rng.IntN(10)))
		var spells []store.CharacterSpellSnapshot
		var skills []store.CharacterSkillSnapshot
		for i := 0; i < rng.IntN(4); i++ {
			spells = append(spells, store.CharacterSpellSnapshot{SpellID: int32(i + 1), Ability: int16(rng.IntN(99) + 1)})
		}
		for i := 0; i < rng.IntN(4); i++ {
			skills = append(skills, store.CharacterSkillSnapshot{SkillID: int32(i + 1), Ability: int16(rng.IntN(99) + 1)})
		}
		csnap := store.CharacterSnapshot{ID: 1, Vitals: vitals, Advancement: adv, Spells: spells, Skills: skills}
		cjob, err := NewCharacterSnapshotJob(fs, csnap)
		if err != nil {
			t.Fatal(err)
		}
		wantVitals, wantAdv := string(vitals), string(adv)
		wantSpells := append([]store.CharacterSpellSnapshot(nil), spells...)
		wantSkills := append([]store.CharacterSkillSnapshot(nil), skills...)
		for i := range vitals {
			vitals[i] = 'X'
		}
		for i := range adv {
			adv[i] = 'X'
		}
		for i := range spells {
			spells[i].Ability = -1
		}
		for i := range skills {
			skills[i].Ability = -1
		}
		if _, err := cjob.Write(context.Background(), 0); err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		gotC := fs.gotChar[len(fs.gotChar)-1]
		if string(gotC.Vitals) != wantVitals || string(gotC.Advancement) != wantAdv {
			t.Fatalf("iter %d: bytes leaked", iter)
		}
		if fmt.Sprint(gotC.Spells) != fmt.Sprint(wantSpells) || fmt.Sprint(gotC.Skills) != fmt.Sprint(wantSkills) {
			t.Fatalf("iter %d: slices leaked", iter)
		}

		// Randomized item snapshot with all pointer fields set.
		e := []byte(fmt.Sprintf(`{"e":%d}`, rng.IntN(9)))
		mkI := func() *int64 { v := rng.IntN(1000); v64 := int64(v + 1); return &v64 }
		mkS := func() *string { v := fmt.Sprintf("s%d", rng.IntN(100)); return &v }
		isnap := store.ItemSnapshot{ID: 2, Enchants: e, Location: store.ItemLocationSnapshot{
			Kind: int16(rng.IntN(5)), CharacterID: mkI(), CorpseID: mkI(),
			ContainerItemID: mkI(), VaultRegion: mkS(),
			PosX: mkI(), PosY: mkI(), PosZ: mkI(), Slot: mkS(),
		}}
		ijob, err := NewItemSnapshotJob(fs, isnap)
		if err != nil {
			t.Fatal(err)
		}
		wantE := string(e)
		wantLoc := isnap.Location
		wantVals := map[string]string{
			"c": fmt.Sprint(*wantLoc.CharacterID), "co": fmt.Sprint(*wantLoc.CorpseID),
			"ci": fmt.Sprint(*wantLoc.ContainerItemID), "v": *wantLoc.VaultRegion,
			"x": fmt.Sprint(*wantLoc.PosX), "y": fmt.Sprint(*wantLoc.PosY),
			"z": fmt.Sprint(*wantLoc.PosZ), "s": *wantLoc.Slot,
		}
		for i := range e {
			e[i] = 'X'
		}
		*isnap.Location.CharacterID = -1
		*isnap.Location.CorpseID = -1
		*isnap.Location.ContainerItemID = -1
		*isnap.Location.VaultRegion = "ZZZ"
		*isnap.Location.PosX = -1
		*isnap.Location.PosY = -1
		*isnap.Location.PosZ = -1
		*isnap.Location.Slot = "ZZZ"
		if _, err := ijob.Write(context.Background(), 0); err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		gotI := fs.gotItem[len(fs.gotItem)-1]
		if string(gotI.Enchants) != wantE {
			t.Fatalf("iter %d: enchants leaked", iter)
		}
		gl := gotI.Location
		gotVals := map[string]string{
			"c": fmt.Sprint(*gl.CharacterID), "co": fmt.Sprint(*gl.CorpseID),
			"ci": fmt.Sprint(*gl.ContainerItemID), "v": *gl.VaultRegion,
			"x": fmt.Sprint(*gl.PosX), "y": fmt.Sprint(*gl.PosY),
			"z": fmt.Sprint(*gl.PosZ), "s": *gl.Slot,
		}
		for f, want := range wantVals {
			if gotVals[f] != want {
				t.Fatalf("iter %d: location field %s leaked (%s != %s)", iter, f, gotVals[f], want)
			}
		}
	}
}

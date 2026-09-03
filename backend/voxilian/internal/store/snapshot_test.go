package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5/pgxpool"
)

func snapFixture(t *testing.T, pool *pgxpool.Pool, q *gen.Queries, sub, name string, slot int16) (int64, CharacterSnapshot) {
	t.Helper()
	ctx := context.Background()
	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: sub})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := createCharacter(ctx, q, validCharParams(acct.ID, slot, name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO spell_protos (id,school,level,mana,exertion,cast_ms,min_hp,outlaw,harmful,reagents,params,version) VALUES (1,1,1,1,1,0,1,false,false,'{}','{}',1),(2,2,1,1,1,0,1,false,false,'{}','{}',1),(3,3,1,1,1,0,1,false,false,'{}','{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO skill_protos (id,division,level,exertion,params,version) VALUES (1,1,1,1,'{}',1),(2,2,1,1,'{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	base := CharacterSnapshot{
		ID: ch.ID, ExpectedRevision: 0, Karma: 5,
		PosX: 5_000_000_000, PosY: -5_000_000_000, PosZ: 3,
		Vitals: json.RawMessage(`{"hp":40}`), Advancement: json.RawMessage(`{"pts":2}`), Flags: 7,
		Spells: []CharacterSpellSnapshot{{SpellID: 1, Ability: 10}, {SpellID: 2, Ability: 20, AtrophyFlag: true}},
		Skills: []CharacterSkillSnapshot{{SkillID: 1, Ability: 30}, {SkillID: 2, Ability: 40}},
	}
	return ch.ID, base
}

func readRoot(t *testing.T, q *gen.Queries, id int64) gen.Character {
	t.Helper()
	ch, err := q.GetCharacterByID(context.Background(), id)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	return ch
}

func TestSaveSnapshotSuccess(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	id, snap := snapFixture(t, pool, q, "sub-snap", "Snapper", 0)

	rev, err := SaveCharacterSnapshot(ctx, pool, snap)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if rev != 1 {
		t.Fatalf("rev = %d, want 1", rev)
	}
	got := readRoot(t, q, id)
	if got.Revision != 1 || got.Karma != 5 || got.PosX != 5_000_000_000 || got.PosY != -5_000_000_000 || got.Flags != 7 {
		t.Fatalf("root = %+v", got)
	}
	if string(got.Vitals) != `{"hp": 40}` && string(got.Vitals) != `{"hp":40}` {
		t.Fatalf("vitals = %s", got.Vitals)
	}

	// Identity/profile fields untouched by the gameplay save.
	if got.AccountID == 0 || got.Slot != 0 || got.Name != "Snapper" || got.Gender != 0 ||
		string(got.Face) == "" || got.Might != 10 || got.Intellect != 10 || got.Stamina != 10 ||
		got.Agility != 10 || got.Mysticism != 10 || got.Aim != 10 || got.Hometown != "tos" ||
		!got.CreatedAt.Valid || got.DeletedAt.Valid {
		t.Fatalf("identity drifted: %+v", got)
	}

	// updated_at moves (backdated first; no sleeps).
	if _, err := pool.Exec(ctx, `UPDATE characters SET updated_at = '2020-01-01' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	snap.ExpectedRevision = 1
	if _, err := SaveCharacterSnapshot(ctx, pool, snap); err != nil {
		t.Fatal(err)
	}
	got = readRoot(t, q, id)
	if !got.UpdatedAt.Time.After(time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("updated_at not advanced: %v", got.UpdatedAt)
	}
}

func TestSaveSnapshotChildReplacement(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	id, snap := snapFixture(t, pool, q, "sub-repl", "Replacer", 0)
	if _, err := SaveCharacterSnapshot(ctx, pool, snap); err != nil {
		t.Fatal(err)
	}
	snap.ExpectedRevision = 1
	snap.Spells = []CharacterSpellSnapshot{{SpellID: 2, Ability: 50}, {SpellID: 3, Ability: 70, AtrophyFlag: true}}
	snap.Skills = []CharacterSkillSnapshot{{SkillID: 2, Ability: 44}}
	if _, err := SaveCharacterSnapshot(ctx, pool, snap); err != nil {
		t.Fatal(err)
	}
	spells, err := q.ListCharacterSpells(ctx, id)
	if err != nil || len(spells) != 2 || spells[0].SpellID != 2 || spells[0].Ability != 50 || spells[1].SpellID != 3 || !spells[1].AtrophyFlag {
		t.Fatalf("spells = %+v, %v", spells, err)
	}
	skills, err := q.ListCharacterSkills(ctx, id)
	if err != nil || len(skills) != 1 || skills[0].SkillID != 2 || skills[0].Ability != 44 {
		t.Fatalf("skills = %+v, %v", skills, err)
	}
}

func TestSaveSnapshotEmptyClears(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	id, snap := snapFixture(t, pool, q, "sub-empty", "Emptier", 0)
	if _, err := SaveCharacterSnapshot(ctx, pool, snap); err != nil {
		t.Fatal(err)
	}
	snap.ExpectedRevision = 1
	snap.Spells = nil
	snap.Skills = []CharacterSkillSnapshot{}
	if _, err := SaveCharacterSnapshot(ctx, pool, snap); err != nil {
		t.Fatal(err)
	}
	if spells, _ := q.ListCharacterSpells(ctx, id); len(spells) != 0 {
		t.Fatalf("spells remain: %+v", spells)
	}
	if skills, _ := q.ListCharacterSkills(ctx, id); len(skills) != 0 {
		t.Fatalf("skills remain: %+v", skills)
	}
}

func TestSaveSnapshotStale(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	id, snap := snapFixture(t, pool, q, "sub-stale", "Staler", 0)
	if _, err := SaveCharacterSnapshot(ctx, pool, snap); err != nil {
		t.Fatal(err)
	}
	stale := snap // ExpectedRevision still 0, radically different content
	stale.Karma = 999
	stale.PosX, stale.PosY = 1, 2
	stale.Vitals = json.RawMessage(`{"hp":1}`)
	stale.Spells = []CharacterSpellSnapshot{{SpellID: 3, Ability: 99}}
	stale.Skills = nil
	if _, err := SaveCharacterSnapshot(ctx, pool, stale); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("err = %v, want ErrStaleRevision", err)
	}
	got := readRoot(t, q, id)
	if got.Revision != 1 || got.Karma != 5 || got.PosX != 5_000_000_000 {
		t.Fatalf("root touched by stale save: %+v", got)
	}
	spells, _ := q.ListCharacterSpells(ctx, id)
	if len(spells) != 2 || spells[0].SpellID != 1 {
		t.Fatalf("spells touched: %+v", spells)
	}
	skills, _ := q.ListCharacterSkills(ctx, id)
	if len(skills) != 2 {
		t.Fatalf("skills touched: %+v", skills)
	}

	// Future revision is equally stale.
	stale.ExpectedRevision = 99
	if _, err := SaveCharacterSnapshot(ctx, pool, stale); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("future err = %v, want ErrStaleRevision", err)
	}
}

func TestSaveSnapshotChildRollback(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	id, snap := snapFixture(t, pool, q, "sub-rb", "Rollbacker", 0)
	if _, err := SaveCharacterSnapshot(ctx, pool, snap); err != nil {
		t.Fatal(err)
	}
	bad := snap
	bad.ExpectedRevision = 1
	bad.Karma = 777
	bad.Spells = []CharacterSpellSnapshot{{SpellID: 9999, Ability: 10}}
	if _, err := SaveCharacterSnapshot(ctx, pool, bad); err == nil {
		t.Fatal("bad spell FK accepted")
	}
	got := readRoot(t, q, id)
	if got.Revision != 1 || got.Karma != 5 {
		t.Fatalf("root not rolled back: %+v", got)
	}
	spells, _ := q.ListCharacterSpells(ctx, id)
	if len(spells) != 2 || spells[0].SpellID != 1 {
		t.Fatalf("children not rolled back: %+v", spells)
	}

	bad2 := snap
	bad2.ExpectedRevision = 1
	bad2.Skills = []CharacterSkillSnapshot{{SkillID: 1, Ability: 0}}
	if _, err := SaveCharacterSnapshot(ctx, pool, bad2); err == nil {
		t.Fatal("ability 0 accepted")
	}
	got = readRoot(t, q, id)
	if got.Revision != 1 {
		t.Fatalf("root moved on skill failure: %+v", got)
	}
}

func TestSaveSnapshotRace(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	id, snap := snapFixture(t, pool, q, "sub-race", "Racer", 0)
	a, b := snap, snap
	a.Karma, b.Karma = 111, 222
	a.Spells = []CharacterSpellSnapshot{{SpellID: 1, Ability: 11}}
	b.Spells = []CharacterSpellSnapshot{{SpellID: 3, Ability: 33}}

	type res struct {
		rev int64
		err error
	}
	ch := make(chan res, 2)
	var wg sync.WaitGroup
	for _, s := range []CharacterSnapshot{a, b} {
		wg.Add(1)
		go func(s CharacterSnapshot) {
			defer wg.Done()
			rev, err := SaveCharacterSnapshot(ctx, pool, s)
			ch <- res{rev, err}
		}(s)
	}
	wg.Wait()
	close(ch)
	var wins, stalem int
	for r := range ch {
		if r.err == nil {
			wins++
			if r.rev != 1 {
				t.Fatalf("winner rev = %d", r.rev)
			}
		} else if errors.Is(r.err, ErrStaleRevision) {
			stalem++
		} else {
			t.Fatalf("unexpected err: %v", r.err)
		}
	}
	if wins != 1 || stalem != 1 {
		t.Fatalf("wins=%d stale=%d, want 1/1", wins, stalem)
	}
	got := readRoot(t, q, id)
	if got.Revision != 1 || (got.Karma != 111 && got.Karma != 222) {
		t.Fatalf("root = %+v", got)
	}
	spells, _ := q.ListCharacterSpells(ctx, id)
	if len(spells) != 1 || (spells[0].Ability != 11 && spells[0].Ability != 33) {
		t.Fatalf("mixed children: %+v (karma %d)", spells, got.Karma)
	}
	if (got.Karma == 111) != (spells[0].Ability == 11) {
		t.Fatalf("root/children from different winners: %+v vs %d", spells, got.Karma)
	}
}

func TestSoftDeleteCAS(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	id, snap := snapFixture(t, pool, q, "sub-del", "Deleter", 0)
	if _, err := SaveCharacterSnapshot(ctx, pool, snap); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE characters SET updated_at = '2020-01-01' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	rev, err := SoftDeleteCharacter(ctx, pool, id, 1)
	if err != nil || rev != 2 {
		t.Fatalf("delete rev = %d, %v", rev, err)
	}
	got := readRoot(t, q, id)
	if !got.DeletedAt.Valid {
		t.Fatalf("not deleted: %+v", got)
	}
	if !got.UpdatedAt.Time.After(time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("updated_at not advanced: %v", got.UpdatedAt)
	}
	if list, _ := q.ListLiveCharactersByAccount(ctx, got.AccountID); len(list) != 0 {
		t.Fatalf("deleted listed: %+v", list)
	}
	// Children retained.
	if spells, _ := q.ListCharacterSpells(ctx, id); len(spells) != 2 {
		t.Fatalf("children deleted: %+v", spells)
	}
	// Saving a deleted character is stale.
	snap.ExpectedRevision = 2
	if _, err := SaveCharacterSnapshot(ctx, pool, snap); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("post-delete save err = %v, want stale", err)
	}
	// Slot+name released: recreate succeeds.
	if _, err := createCharacter(ctx, q, validCharParams(got.AccountID, 0, "Deleter")); err != nil {
		t.Fatalf("reuse after delete: %v", err)
	}
	// Second delete on old revision is stale, not idempotent success.
	if _, err := SoftDeleteCharacter(ctx, pool, id, 1); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("second delete err = %v, want stale", err)
	}
}

func TestSaveVsDeleteRace(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	id, snap := snapFixture(t, pool, q, "sub-sd", "Raced", 0)

	var wg sync.WaitGroup
	var saveErr, delErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		s := snap
		s.Karma = 555
		_, saveErr = SaveCharacterSnapshot(ctx, pool, s)
	}()
	go func() {
		defer wg.Done()
		_, delErr = SoftDeleteCharacter(ctx, pool, id, 0)
	}()
	wg.Wait()

	saveOK, delOK := saveErr == nil, delErr == nil
	if saveOK == delOK {
		t.Fatalf("save=%v delete=%v, want exactly one winner", saveErr, delErr)
	}
	if !saveOK && !errors.Is(saveErr, ErrStaleRevision) {
		t.Fatalf("save loser err = %v", saveErr)
	}
	if !delOK && !errors.Is(delErr, ErrStaleRevision) {
		t.Fatalf("delete loser err = %v", delErr)
	}
	got := readRoot(t, q, id)
	if got.Revision != 1 {
		t.Fatalf("rev = %d, want exactly 1", got.Revision)
	}
	if saveOK && (got.DeletedAt.Valid || got.Karma != 555) {
		t.Fatalf("save-won state wrong: %+v", got)
	}
	if delOK && !got.DeletedAt.Valid {
		t.Fatalf("delete-won state wrong: %+v", got)
	}
}

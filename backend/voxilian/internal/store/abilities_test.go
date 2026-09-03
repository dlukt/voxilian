package store

import (
	"context"
	"testing"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5/pgxpool"
)

func seedAbilityCatalogs(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, id := range []int32{1, 2, 3} {
		_, err := pool.Exec(ctx, `INSERT INTO spell_protos (id,school,level,mana,exertion,cast_ms,min_hp,outlaw,harmful,reagents,params,version) VALUES ($1,1,1,1,1,0,1,false,false,'{}','{}',1)`, id)
		if err != nil {
			t.Fatalf("spell proto %d: %v", id, err)
		}
	}
	for _, id := range []int32{1, 2} {
		_, err := pool.Exec(ctx, `INSERT INTO skill_protos (id,division,level,exertion,params,version) VALUES ($1,1,1,1,'{}',1)`, id)
		if err != nil {
			t.Fatalf("skill proto %d: %v", id, err)
		}
	}
}

func TestAbilityReads(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	seedAbilityCatalogs(t, pool)
	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-ab"})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := createCharacter(ctx, q, validCharParams(acct.ID, 0, "Ab"))
	if err != nil {
		t.Fatal(err)
	}

	// Empty lists: no error, empty result.
	spells, err := q.ListCharacterSpells(ctx, ch.ID)
	if err != nil || len(spells) != 0 {
		t.Fatalf("empty spells = %+v, %v", spells, err)
	}
	skills, err := q.ListCharacterSkills(ctx, ch.ID)
	if err != nil || len(skills) != 0 {
		t.Fatalf("empty skills = %+v, %v", skills, err)
	}

	// Primitives run in an explicit test transaction; production
	// orchestration (root CAS first) belongs to M1-T7a.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tq := q.WithTx(tx)
	for _, s := range []struct {
		id      int32
		ability int16
		atrophy bool
	}{{3, 90, true}, {1, 10, false}, {2, 55, false}} {
		if err := tq.InsertCharacterSpell(ctx, gen.InsertCharacterSpellParams{
			CharacterID: ch.ID, SpellID: s.id, Ability: s.ability, AtrophyFlag: s.atrophy,
		}); err != nil {
			t.Fatalf("insert spell: %v", err)
		}
	}
	for _, s := range []struct {
		id      int32
		ability int16
		atrophy bool
	}{{2, 77, true}, {1, 5, false}} {
		if err := tq.InsertCharacterSkill(ctx, gen.InsertCharacterSkillParams{
			CharacterID: ch.ID, SkillID: s.id, Ability: s.ability, AtrophyFlag: s.atrophy,
		}); err != nil {
			t.Fatalf("insert skill: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Ascending order + round-trip.
	spells, err = q.ListCharacterSpells(ctx, ch.ID)
	if err != nil || len(spells) != 3 ||
		spells[0].SpellID != 1 || spells[1].SpellID != 2 || spells[2].SpellID != 3 {
		t.Fatalf("spell order = %+v, %v", spells, err)
	}
	if spells[2].Ability != 90 || !spells[2].AtrophyFlag || spells[0].AtrophyFlag {
		t.Fatalf("spell fields = %+v", spells)
	}
	skills, err = q.ListCharacterSkills(ctx, ch.ID)
	if err != nil || len(skills) != 2 || skills[0].SkillID != 1 || skills[1].Ability != 77 {
		t.Fatalf("skills = %+v, %v", skills, err)
	}

	// Delete removes only this character's rows.
	ch2, err := createCharacter(ctx, q, validCharParams(acct.ID, 1, "Ab2"))
	if err != nil {
		t.Fatal(err)
	}
	tx2, _ := pool.Begin(ctx)
	tq2 := q.WithTx(tx2)
	_ = tq2.InsertCharacterSpell(ctx, gen.InsertCharacterSpellParams{CharacterID: ch2.ID, SpellID: 1, Ability: 1})
	_ = tq2.InsertCharacterSkill(ctx, gen.InsertCharacterSkillParams{CharacterID: ch2.ID, SkillID: 1, Ability: 1})
	_ = tx2.Commit(ctx)
	if err := q.DeleteCharacterSpells(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	if err := q.DeleteCharacterSkills(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	if spells, _ := q.ListCharacterSpells(ctx, ch.ID); len(spells) != 0 {
		t.Fatalf("spells remain: %+v", spells)
	}
	if spells, _ := q.ListCharacterSpells(ctx, ch2.ID); len(spells) != 1 {
		t.Fatalf("other char spells touched: %+v", spells)
	}
	if skills, _ := q.ListCharacterSkills(ctx, ch2.ID); len(skills) != 1 {
		t.Fatalf("other char skills touched: %+v", skills)
	}

	// Primitives surface real FK/CHECK errors.
	if err := q.InsertCharacterSpell(ctx, gen.InsertCharacterSpellParams{CharacterID: ch.ID, SpellID: 9999, Ability: 1}); err == nil {
		t.Fatal("bad spell FK accepted")
	}
	if err := q.InsertCharacterSkill(ctx, gen.InsertCharacterSkillParams{CharacterID: ch.ID, SkillID: 1, Ability: 0}); err == nil {
		t.Fatal("ability 0 accepted")
	}
}

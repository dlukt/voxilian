package character

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/dlukt/voxilian/internal/simtest"
	"github.com/dlukt/voxilian/internal/store"
)

// Test-only stable IDs. These are explicitly NOT canonical game IDs
// (M9 supplies real content later); production code invents none.
const (
	testFreeSpell  = 100
	testSpellL1    = 101
	testSpellL2    = 102
	testSpellShut  = 103 // Offered=false
	testSpellL3    = 104 // Level 3: unselectable
	testSpellBadAb = 105 // InitialAbility 0: broken trusted data
	testSkillL1    = 201
	testSkillL2    = 202
	testSkillDupNo = 102 // numeric overlap with a spell ID: allowed
	testItemMace   = 9001
	testItemCoins  = 9002
)

func testContent() StaticContent {
	return StaticContent{
		Spells: map[uint16]AbilitySpec{
			testFreeSpell:  {ID: testFreeSpell, Level: 1, Offered: true, InitialAbility: 50, School: SchoolShalille},
			testSpellL1:    {ID: testSpellL1, Level: 1, Offered: true, InitialAbility: 30, School: SchoolShalille},
			testSpellL2:    {ID: testSpellL2, Level: 2, Offered: true, InitialAbility: 40, School: SchoolQor},
			testSpellShut:  {ID: testSpellShut, Level: 1, Offered: false, InitialAbility: 30, School: SchoolShalille},
			testSpellL3:    {ID: testSpellL3, Level: 3, Offered: true, InitialAbility: 30, School: SchoolShalille},
			testSpellBadAb: {ID: testSpellBadAb, Level: 1, Offered: true, InitialAbility: 0, School: SchoolShalille},
		},
		Skills: map[uint16]AbilitySpec{
			testSkillL1:    {ID: testSkillL1, Level: 1, Offered: true, InitialAbility: 25},
			testSkillL2:    {ID: testSkillL2, Level: 2, Offered: true, InitialAbility: 35},
			testSkillDupNo: {ID: testSkillDupNo, Level: 1, Offered: true, InitialAbility: 20},
		},
		Profile: StarterProfile{
			Spells: []AbilitySpec{
				{ID: testFreeSpell, Level: 1, Offered: true, InitialAbility: 50, School: SchoolShalille},
			},
			Items: []StarterItem{
				{ProtoID: testItemMace, Qty: 1, Hits: 100, Slot: "hand"},
				{ProtoID: testItemCoins, Qty: 500, Hits: 0, Slot: "coins"},
			},
			Hometown: "tos",
			PosX:     1000,
			PosY:     2000,
			PosZ:     3000,
		},
	}
}

func testPolicy() NamePolicy {
	return NewNamePolicy([]string{"goblin"}, []string{"admin"})
}

func validRequest() CreateRequest {
	return CreateRequest{
		Slot:     0,
		Name:     "Aria",
		Gender:   1,
		Face:     Face{HairStyle: 3, HairColor: 4, SkinTone: 5, Parts: [5]uint8{6, 7, 8, 9, 10}},
		Stats:    [6]uint8{10, 10, 10, 10, 25, 10},
		SpellIDs: []uint16{testSpellL1},
		SkillIDs: []uint16{testSkillL1},
	}
}

type fakeRepo struct {
	mu        sync.Mutex
	created   []store.NewCharacter
	createID  int64
	createErr error
	live      []store.LiveCharacter
	listErr   error
	deleted   [][2]int64
	deleteRev int64
	deleteErr error
}

func (f *fakeRepo) CreateCharacter(_ context.Context, c store.NewCharacter) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, c)
	if f.createErr != nil {
		return 0, f.createErr
	}
	return f.createID, nil
}

func (f *fakeRepo) ListLiveCharacters(_ context.Context, _ int64) ([]store.LiveCharacter, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live, f.listErr
}

func (f *fakeRepo) SoftDeleteCharacter(_ context.Context, id, rev int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, [2]int64{id, rev})
	return f.deleteRev, f.deleteErr
}

func newUnitService(t *testing.T, repo *fakeRepo) *Service {
	t.Helper()
	svc, err := NewService(testContent(), testPolicy(), repo)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestNameNormalization(t *testing.T) {
	p := testPolicy()
	// NFC composed/decomposed equivalence; the normalized form returns.
	got, err := p.Normalize("élan")
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got != "élan" {
		t.Errorf("Normalize = %q, want NFC %q", got, "élan")
	}
	if c := utf8.RuneCountInString(got); c != 4 {
		t.Errorf("code points = %d, want 4", c)
	}
	// Combining marks are allowed (and compose under NFC).
	got, err = p.Normalize("ábc")
	if err != nil {
		t.Fatalf("combining mark: %v", err)
	}
	if got != "ábc" {
		t.Errorf("Normalize = %q, want %q", got, "ábc")
	}
}

func TestNameLengthBoundaries(t *testing.T) {
	p := testPolicy()
	for name, ok := range map[string]bool{
		"Ab":                    false, // 2 points
		"Abc":                   true,  // 3
		"Abcdefghijklmnop":      true,  // 16
		"Abcdefghijklmnopq":     false, // 17
		strings.Repeat("é", 16): true,  // multibyte: 16 points
		strings.Repeat("é", 17): false, // multibyte: 17 points
	} {
		got, err := p.Normalize(name)
		if ok && err != nil {
			t.Errorf("Normalize(%q) = %v, want ok", name, err)
		}
		if !ok && !errors.Is(err, ErrInvalidName) {
			t.Errorf("Normalize(%q) = %q, %v; want ErrInvalidName", name, got, err)
		}
	}
}

func TestNameAllowedCategories(t *testing.T) {
	p := testPolicy()
	for _, name := range []string{
		"Bob", "O'Brien", "Jean-Luc", "A B",
		"ßigurd", "Ωmega", "Жанна", // non-ASCII letters
		"ab42", "R2D2", // numbers
	} {
		if _, err := p.Normalize(name); err != nil {
			t.Errorf("Normalize(%q) = %v, want ok", name, err)
		}
	}
}

func TestNameRejectedCategories(t *testing.T) {
	p := testPolicy()
	for _, name := range []string{
		"a_b",   // underscore
		"a/b",   // slash
		"ab😀",   // emoji
		"ab\n",  // control/newline
		"ab’",   // smart quote U+2019 (only U+0027 allowed)
		"ab.",   // period
		"ab\tc", // tab
	} {
		if got, err := p.Normalize(name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Normalize(%q) = %q, %v; want ErrInvalidName", name, got, err)
		}
	}
}

func TestNameNoTrimming(t *testing.T) {
	p := testPolicy()
	got, err := p.Normalize(" Bob ")
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got != " Bob " {
		t.Errorf("Normalize rewrote %q to %q; only NFC may apply", " Bob ", got)
	}
}

func TestNamePolicyExactFoldedMatch(t *testing.T) {
	p := testPolicy()
	for _, name := range []string{"goblin", "GOBLIN", "Goblin", "GoBlIn"} {
		if _, err := p.Normalize(name); !errors.Is(err, ErrNameUnavailable) {
			t.Errorf("blocked %q: err = %v, want ErrNameUnavailable", name, err)
		}
	}
	for _, name := range []string{"admin", "ADMIN"} {
		if _, err := p.Normalize(name); !errors.Is(err, ErrNameUnavailable) {
			t.Errorf("reserved %q: err = %v, want ErrNameUnavailable", name, err)
		}
	}
	// Substrings and superstrings are not matches.
	for _, name := range []string{"gob", "goblinx", "xadmin", "myadminx"} {
		if _, err := p.Normalize(name); err != nil {
			t.Errorf("non-matching %q rejected: %v", name, err)
		}
	}
}

func TestValidateStats(t *testing.T) {
	if err := ValidateStats([6]uint8{1, 1, 1, 1, 1, 1}); err != nil {
		t.Errorf("all 1: %v", err)
	}
	if err := ValidateStats([6]uint8{50, 50, 50, 25, 15, 10}); err != nil { // sum 200
		t.Errorf("sum 200: %v", err)
	}
	for name, stats := range map[string][6]uint8{
		"all 50":   {50, 50, 50, 50, 50, 50},
		"zero":     {0, 10, 10, 10, 10, 10},
		"fiftyone": {51, 10, 10, 10, 10, 10},
		"sum 201":  {50, 50, 50, 25, 16, 10},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateStats(stats); !errors.Is(err, ErrBadStats) {
				t.Errorf("err = %v, want ErrBadStats", err)
			}
		})
	}
}

func TestAbilityBudgetBoundaries(t *testing.T) {
	repo := &fakeRepo{createID: 7}
	svc := newUnitService(t, repo)
	ctx := context.Background()
	budget := func(spells, skills []uint16) error {
		req := validRequest()
		req.SpellIDs, req.SkillIDs = spells, skills
		_, err := svc.Create(ctx, 1, req)
		return err
	}
	// Costs: L1=10, L2=25.
	for name, tc := range map[string]struct {
		spells, skills []uint16
		ok             bool
	}{
		"zero":                     {nil, nil, true},
		"ten":                      {[]uint16{testSpellL1}, nil, true},
		"twenty":                   {[]uint16{testSpellL1}, []uint16{testSkillL1}, true},
		"twentyfive":               {[]uint16{testSpellL2}, nil, true},
		"thirtyfive":               {[]uint16{testSpellL2, testSpellL1}, nil, true},
		"fortyfive":                {[]uint16{testSpellL2, testSpellL1}, []uint16{testSkillL1}, true},
		"fifty":                    {[]uint16{testSpellL2}, []uint16{testSkillL2}, false},
		"same numeric spell+skill": {[]uint16{testSpellL2}, []uint16{testSkillDupNo}, true},
	} {
		t.Run("cost/"+name, func(t *testing.T) {
			err := budget(tc.spells, tc.skills)
			if tc.ok && err != nil {
				t.Errorf("err = %v, want ok", err)
			}
			if !tc.ok && !errors.Is(err, ErrBadBudget) {
				t.Errorf("err = %v, want ErrBadBudget", err)
			}
		})
	}
	for name, tc := range map[string]struct {
		spells, skills []uint16
	}{
		"unknown spell":        {[]uint16{999}, nil},
		"unknown skill":        {nil, []uint16{999}},
		"unoffered spell":      {[]uint16{testSpellShut}, nil},
		"level3 spell":         {[]uint16{testSpellL3}, nil},
		"duplicate spell":      {[]uint16{testSpellL1, testSpellL1}, nil},
		"duplicate skill":      {nil, []uint16{testSkillL1, testSkillL1}},
		"explicit free spell":  {[]uint16{testFreeSpell}, nil},
		"free plus paid mixed": {[]uint16{testFreeSpell, testSpellL1}, nil},
	} {
		t.Run("reject/"+name, func(t *testing.T) {
			err := budget(tc.spells, tc.skills)
			if !errors.Is(err, ErrBadBudget) {
				t.Errorf("err = %v, want ErrBadBudget", err)
			}
		})
	}
	// Broken trusted data (ability 0) is internal, not bad_budget.
	req := validRequest()
	req.SpellIDs = []uint16{testSpellBadAb}
	if _, err := svc.Create(ctx, 1, req); !errors.Is(err, ErrInvalidContent) {
		t.Errorf("bad trusted ability err = %v, want ErrInvalidContent", err)
	}
}

func TestCreateValidationErrors(t *testing.T) {
	repo := &fakeRepo{createID: 7}
	svc := newUnitService(t, repo)
	ctx := context.Background()

	req := validRequest()
	req.Slot = 2
	if _, err := svc.Create(ctx, 1, req); !errors.Is(err, ErrInvalidSlot) {
		t.Errorf("slot err = %v, want ErrInvalidSlot", err)
	}
	req = validRequest()
	req.Name = "ab"
	if _, err := svc.Create(ctx, 1, req); !errors.Is(err, ErrInvalidName) {
		t.Errorf("name err = %v, want ErrInvalidName", err)
	}
	req = validRequest()
	req.Name = "goblin"
	if _, err := svc.Create(ctx, 1, req); !errors.Is(err, ErrNameUnavailable) {
		t.Errorf("blocked err = %v, want ErrNameUnavailable", err)
	}
	req = validRequest()
	req.Stats = [6]uint8{50, 50, 50, 50, 50, 50}
	if _, err := svc.Create(ctx, 1, req); !errors.Is(err, ErrBadStats) {
		t.Errorf("stats err = %v, want ErrBadStats", err)
	}
	if len(repo.created) != 0 {
		t.Errorf("failed validations reached repo %d times", len(repo.created))
	}
}

func TestCreateOrchestration(t *testing.T) {
	repo := &fakeRepo{createID: 7}
	svc := newUnitService(t, repo)
	id, err := svc.Create(context.Background(), 42, validRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != 7 {
		t.Errorf("id = %d, want repo 7", id)
	}
	if len(repo.created) != 1 {
		t.Fatalf("repo calls = %d", len(repo.created))
	}
	nc := repo.created[0]
	if nc.AccountID != 42 || nc.Slot != 0 || nc.Name != "Aria" || nc.Gender != 1 {
		t.Errorf("identity = %+v", nc)
	}
	if nc.Might != 10 || nc.Mysticism != 25 || nc.Aim != 10 {
		t.Errorf("stats = %+v", nc)
	}
	// Free spell first, then selected, with resolved initial abilities.
	if len(nc.Spells) != 2 || nc.Spells[0].ID != testFreeSpell || nc.Spells[0].Ability != 50 {
		t.Errorf("spells = %+v, want free first", nc.Spells)
	}
	if nc.Spells[1].ID != testSpellL1 || nc.Spells[1].Ability != 30 {
		t.Errorf("spells = %+v", nc.Spells)
	}
	if len(nc.Skills) != 1 || nc.Skills[0].ID != testSkillL1 || nc.Skills[0].Ability != 25 {
		t.Errorf("skills = %+v", nc.Skills)
	}
	// Karma: free Shalille + selected Shalille, no Qor → +20.
	if nc.Karma != 20 {
		t.Errorf("karma = %d, want 20", nc.Karma)
	}
	if nc.Hometown != "tos" || nc.PosX != 1000 || nc.PosY != 2000 || nc.PosZ != 3000 {
		t.Errorf("spawn = %+v", nc)
	}
	var v Vitals
	if err := json.Unmarshal(nc.Vitals, &v); err != nil {
		t.Fatalf("vitals json: %v", err)
	}
	if v.HP != 20 || v.BaseMax != 20 || v.Max != 20 || v.Vigor != 100 || v.Threshold != 80 || v.Stomach != 0 {
		t.Errorf("vitals = %+v", v)
	}
	if v.Mana != 20 || v.MaxMana != 20 { // 15 + 25/5
		t.Errorf("mana = %d/%d, want 20/20", v.Mana, v.MaxMana)
	}
	var f struct {
		HairStyle uint8    `json:"hair_style"`
		HairColor uint8    `json:"hair_color"`
		SkinTone  uint8    `json:"skin_tone"`
		Parts     [5]uint8 `json:"parts"`
	}
	if err := json.Unmarshal(nc.Face, &f); err != nil {
		t.Fatalf("face json: %v", err)
	}
	if f.HairStyle != 3 || f.HairColor != 4 || f.SkinTone != 5 || f.Parts != [5]uint8{6, 7, 8, 9, 10} {
		t.Errorf("face = %+v", f)
	}
	if string(nc.Advancement) != "{}" || nc.Flags != 0 {
		t.Errorf("advancement/flags = %s/%d", nc.Advancement, nc.Flags)
	}
	if len(nc.Items) != 2 || nc.Items[0].ProtoID != testItemMace || nc.Items[0].Qty != 1 ||
		nc.Items[1].ProtoID != testItemCoins || nc.Items[1].Qty != 500 {
		t.Errorf("items = %+v", nc.Items)
	}
	// Store race sentinels pass through untouched.
	repo.createErr = store.ErrNameTaken
	if _, err := svc.Create(context.Background(), 42, validRequest()); !errors.Is(err, store.ErrNameTaken) {
		t.Errorf("sentinel err = %v, want ErrNameTaken", err)
	}
}

func TestKarmaTable(t *testing.T) {
	ctx := context.Background()
	qorFree := testContent()
	qorFree.Profile.Spells = []AbilitySpec{
		{ID: testFreeSpell, Level: 1, Offered: true, InitialAbility: 50, School: SchoolQor},
	}
	noneFree := testContent()
	noneFree.Profile.Spells = []AbilitySpec{
		{ID: testFreeSpell, Level: 1, Offered: true, InitialAbility: 50, School: 0},
	}
	cases := []struct {
		name    string
		content StaticContent
		spells  []uint16
		want    int32
	}{
		{"shalille only", testContent(), []uint16{testSpellL1}, 20},
		{"qor only", qorFree, nil, -20},
		{"both", testContent(), []uint16{testSpellL2}, 0},
		{"neither", noneFree, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepo{createID: 1}
			svc, err := NewService(tc.content, testPolicy(), repo)
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			req := validRequest()
			req.SpellIDs = tc.spells
			if _, err := svc.Create(ctx, 1, req); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if got := repo.created[0].Karma; got != tc.want {
				t.Errorf("karma = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestContentValidation(t *testing.T) {
	repo := &fakeRepo{}
	mutate := func(f func(*StaticContent)) StaticContent {
		c := testContent()
		f(&c)
		return c
	}
	for name, content := range map[string]StaticContent{
		"empty hometown": mutate(func(c *StaticContent) { c.Profile.Hometown = "" }),
		"dup free": mutate(func(c *StaticContent) {
			c.Profile.Spells = append(c.Profile.Spells, c.Profile.Spells[0])
		}),
		"free ability 0": mutate(func(c *StaticContent) {
			s := c.Profile.Spells[0]
			s.InitialAbility = 0
			c.Profile.Spells[0] = s
		}),
		"free ability 100": mutate(func(c *StaticContent) {
			s := c.Profile.Spells[0]
			s.InitialAbility = 100
			c.Profile.Spells[0] = s
		}),
		"item proto 0":    mutate(func(c *StaticContent) { c.Profile.Items[0].ProtoID = 0 }),
		"item qty 0":      mutate(func(c *StaticContent) { c.Profile.Items[0].Qty = 0 }),
		"item empty slot": mutate(func(c *StaticContent) { c.Profile.Items[0].Slot = "" }),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewService(content, testPolicy(), repo); !errors.Is(err, ErrInvalidContent) {
				t.Errorf("err = %v, want ErrInvalidContent", err)
			}
		})
	}
	if _, err := NewService(nil, testPolicy(), repo); !errors.Is(err, ErrInvalidContent) {
		t.Errorf("nil content err = %v, want ErrInvalidContent", err)
	}
}

func TestServiceListFindDelete(t *testing.T) {
	repo := &fakeRepo{
		createID: 1,
		live: []store.LiveCharacter{
			{ID: 11, Slot: 0, Name: "Aria", Revision: 3, Vitals: []byte(`{"hp":20,"base_max":20}`)},
			{ID: 12, Slot: 1, Name: "Bram", Revision: 0, Vitals: []byte(`{"hp":20,"base_max":20}`)},
		},
		deleteRev: 4,
	}
	svc := newUnitService(t, repo)
	ctx := context.Background()

	list, err := svc.List(ctx, 42)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0] != (ListEntry{Slot: 0, Name: "Aria", Level: 20}) {
		t.Errorf("list = %+v", list)
	}
	d, err := svc.FindBySlot(ctx, 42, 1)
	if err != nil || d.ID != 12 || d.Revision != 0 || d.Name != "Bram" {
		t.Errorf("find = %+v, %v", d, err)
	}
	if _, err := svc.FindBySlot(ctx, 42, 5); !errors.Is(err, ErrInvalidSlot) {
		t.Errorf("bad slot err = %v, want ErrInvalidSlot", err)
	}
	// Slot 0 is taken here; use an empty repo for the miss path.
	repo.live = repo.live[:1]
	if _, err := svc.FindBySlot(ctx, 42, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("miss err = %v, want ErrNotFound", err)
	}
	rev, err := svc.Delete(ctx, 11, 3)
	if err != nil || rev != 4 {
		t.Errorf("delete = %d, %v", rev, err)
	}
	if len(repo.deleted) != 1 || repo.deleted[0] != [2]int64{11, 3} {
		t.Errorf("deleted = %v", repo.deleted)
	}
}

func TestServiceListRejectsCorruptVitals(t *testing.T) {
	ctx := context.Background()
	for name, vitals := range map[string]string{
		"malformed":  `{oops`,
		"missing":    `{"hp":20}`,
		"zero":       `{"base_max":0}`,
		"over u16":   `{"base_max":70000}`,
		"wrong type": `{"base_max":"20"}`,
	} {
		t.Run(name, func(t *testing.T) {
			repo := &fakeRepo{live: []store.LiveCharacter{
				{ID: 1, Slot: 0, Name: "Aria", Vitals: []byte(vitals)},
			}}
			svc := newUnitService(t, repo)
			if _, err := svc.List(ctx, 42); !errors.Is(err, ErrInvalidContent) {
				t.Errorf("err = %v, want ErrInvalidContent (never silent 0)", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PG-backed integration (real *store.PGStore through the service)
// ---------------------------------------------------------------------------

func openServicePG(t *testing.T, content StaticContent) (*Service, *store.PGStore, *pgxpool.Pool) {
	t.Helper()
	pg := simtest.StartPostgres18(t)
	sqldb, err := sql.Open("pgx", pg.DSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sqldb.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(simtest.RepoRoot(t), "backend", "voxilian", "migrations")
	if err := goose.UpTo(sqldb, dir, 5); err != nil {
		t.Fatalf("migrate to 5: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), pg.DSN)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	st, err := store.New(pool, nil)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	seedCreationCatalog(t, st)
	svc, err := NewService(content, testPolicy(), st)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, st, pool
}

func seedCreationCatalog(t *testing.T, st *store.PGStore) {
	t.Helper()
	ctx := context.Background()
	spell := func(id int32, school int16) store.SpellProtoRecord {
		return store.SpellProtoRecord{ID: id, School: school, Level: 1, Mana: 5,
			Exertion: 1, CastMs: 100, MinHP: 1,
			Reagents: json.RawMessage(`{}`), Params: json.RawMessage(`{}`), Version: 1}
	}
	err := st.UpsertCatalogBatch(ctx, store.CatalogBatch{
		Spells: []store.SpellProtoRecord{
			spell(testFreeSpell, 1), spell(testSpellL1, 1), spell(testSpellL2, 2),
		},
		Skills: []store.SkillProtoRecord{
			{ID: testSkillL1, Division: 1, Level: 1, Exertion: 1, Params: json.RawMessage(`{}`), Version: 1},
			{ID: testSkillL2, Division: 1, Level: 1, Exertion: 1, Params: json.RawMessage(`{}`), Version: 1},
			{ID: testSkillDupNo, Division: 1, Level: 1, Exertion: 1, Params: json.RawMessage(`{}`), Version: 1},
		},
		Items: []store.ItemProtoRecord{
			{ID: testItemMace, Kind: 0, Slot: strp("hand"), Base: json.RawMessage(`{}`), Version: 1},
			{ID: testItemCoins, Kind: 0, Slot: strp("coins"), Base: json.RawMessage(`{}`), Version: 1},
		},
	}, false)
	if err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
}

func strp(s string) *string { return &s }

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func TestServiceCreatePersistsFullAggregate(t *testing.T) {
	svc, st, pool := openServicePG(t, testContent())
	ctx := context.Background()
	acct, err := st.EnsureAccount(ctx, "creator-sub", nil)
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	id, err := svc.Create(ctx, acct, validRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id <= 0 {
		t.Fatalf("id = %d", id)
	}
	live, err := st.ListLiveCharacters(ctx, acct)
	if err != nil || len(live) != 1 {
		t.Fatalf("live = %+v, %v", live, err)
	}
	row := live[0]
	if row.Name != "Aria" || row.Slot != 0 || row.Revision != 0 {
		t.Errorf("row = %+v, want Aria/slot 0/revision 0", row)
	}
	if countRows(t, pool, `SELECT COUNT(*) FROM character_spells WHERE character_id = $1`, id) != 2 {
		t.Error("want free + selected spell rows")
	}
	if countRows(t, pool, `SELECT COUNT(*) FROM character_skills WHERE character_id = $1`, id) != 1 {
		t.Error("want 1 skill row")
	}
	if n := countRows(t, pool,
		`SELECT COUNT(*) FROM character_spells WHERE character_id = $1 AND atrophy_flag = false`, id); n != 2 {
		t.Errorf("atrophy-clear spells = %d, want 2", n)
	}
	var qty int
	if err := pool.QueryRow(ctx,
		`SELECT qty FROM item_instances ii JOIN item_locations il ON il.item_id = ii.id
		 WHERE il.character_id = $1 AND ii.proto = $2`, id, testItemCoins).Scan(&qty); err != nil {
		t.Fatalf("coins: %v", err)
	}
	if qty != 500 {
		t.Errorf("coins qty = %d, want 500", qty)
	}
	if n := countRows(t, pool,
		`SELECT COUNT(*) FROM item_locations WHERE character_id = $1 AND kind = 0`, id); n != 2 {
		t.Errorf("inventory locations = %d, want 2", n)
	}
	// Service list sources level 20 from durable vitals.
	list, err := svc.List(ctx, acct)
	if err != nil || len(list) != 1 || list[0].Level != 20 {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

func TestServiceCreateStoresNFCName(t *testing.T) {
	svc, st, pool := openServicePG(t, testContent())
	ctx := context.Background()
	acct, err := st.EnsureAccount(ctx, "nfc-sub", nil)
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	req := validRequest()
	req.Name = "Ária" // decomposed A + U+0301.
	if _, err := svc.Create(ctx, acct, req); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var name string
	if err := pool.QueryRow(ctx, `SELECT name FROM characters WHERE account_id = $1`, acct).Scan(&name); err != nil {
		t.Fatalf("read name: %v", err)
	}
	if name != "Ária" {
		t.Errorf("durable name = %q, want NFC %q", name, "Ária")
	}
}

func TestServiceDoubleCreateRace(t *testing.T) {
	svc, st, pool := openServicePG(t, testContent())
	ctx := context.Background()
	acct, err := st.EnsureAccount(ctx, "race-sub", nil)
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	const n = 8
	ids := make([]int64, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ids[i], errs[i] = svc.Create(ctx, acct, validRequest())
		}(i)
	}
	close(start)
	wg.Wait()
	var winner int64
	for i := 0; i < n; i++ {
		if errs[i] == nil {
			if winner == 0 {
				winner = ids[i]
			} else if ids[i] != winner {
				t.Fatalf("two winners: %v", ids)
			}
		} else if !errors.Is(errs[i], store.ErrSlotOccupied) && !errors.Is(errs[i], store.ErrNameTaken) {
			t.Fatalf("loser %d err = %v, want slot/name conflict", i, errs[i])
		}
	}
	if winner <= 0 {
		t.Fatal("no winner")
	}
	if c := countRows(t, pool, `SELECT COUNT(*) FROM characters WHERE account_id = $1 AND deleted_at IS NULL`, acct); c != 1 {
		t.Errorf("live rows = %d, want 1", c)
	}
	if c := countRows(t, pool, `SELECT COUNT(*) FROM character_spells WHERE character_id = $1`, winner); c != 2 {
		t.Errorf("winner spells = %d, want 2", c)
	}
	if c := countRows(t, pool,
		`SELECT COUNT(*) FROM item_locations WHERE character_id = $1 AND kind = 0`, winner); c != 2 {
		t.Errorf("winner inventory = %d, want 2", c)
	}
	if c := countRows(t, pool, `SELECT COUNT(*) FROM item_instances`); c != 2 {
		t.Errorf("total item instances = %d, want exactly the winner's 2", c)
	}
}

func TestServiceCaseInsensitiveNameRace(t *testing.T) {
	svc, st, pool := openServicePG(t, testContent())
	ctx := context.Background()
	acct, err := st.EnsureAccount(ctx, "case-sub", nil)
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i, name := range []string{"Aria", "ARIA"} {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			<-start
			req := validRequest()
			req.Name = name
			req.Slot = uint8(i) // different slots: only the name index arbitrates.
			_, errs[i] = svc.Create(ctx, acct, req)
		}(i, name)
	}
	close(start)
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else if !errors.Is(err, store.ErrNameTaken) {
			t.Fatalf("err = %v, want nil or ErrNameTaken", err)
		}
	}
	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1: %v", wins, errs)
	}
	if c := countRows(t, pool, `SELECT COUNT(*) FROM characters WHERE account_id = $1 AND deleted_at IS NULL`, acct); c != 1 {
		t.Errorf("live rows = %d, want 1", c)
	}
}

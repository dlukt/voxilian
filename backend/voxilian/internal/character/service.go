// Package character owns character-creation domain rules and persistence
// orchestration (spec §6.1, §8, §9): display-name normalization/policy,
// stat and 45-point ability validation, initial karma/vitals/face
// generation, and the transactional create/list/delete flow.
//
// Staging: creation content (ability metadata, starter profile, name
// policy) arrives through injected immutable seams. Tests use
// deterministic fakes; M9-T1 supplies the production content. This
// package never touches the network, sessions, gateway, or SQL
// directly: persistence goes through the narrow Repository interface
// satisfied by *store.PGStore, and only internal/store imports pgx.
package character

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/dlukt/voxilian/internal/store"
)

// Domain errors. T3b maps these to the wire later; the split is frozen
// here so mapping stays mechanical.
var (
	// ErrInvalidName reports display-name syntax/length failure
	// (→ 217 create/rejected).
	ErrInvalidName = errors.New("invalid_name")
	// ErrNameUnavailable reports a blocked/reserved name
	// (→ 202 name_taken; deliberately not which list matched).
	ErrNameUnavailable = errors.New("name_unavailable")
	// ErrBadStats reports stat range/sum failure (→ 202 bad_stats).
	ErrBadStats = errors.New("bad_stats")
	// ErrBadBudget reports ability selection/budget failure
	// (→ 202 bad_budget).
	ErrBadBudget = errors.New("bad_budget")
	// ErrInvalidSlot reports a slot outside 0/1
	// (→ 217 rejected for the requesting op).
	ErrInvalidSlot = errors.New("invalid_slot")
	// ErrSlotOccupied reports a lost slot-claim race at the store
	// (→ 202 slot_occupied). Translated from the store sentinel so the
	// gateway never imports store errors.
	ErrSlotOccupied = errors.New("slot_occupied")
	// ErrPersistence reports repository/persistence unavailability:
	// connection loss, query failure, stale CAS on delete (→ 202 retry
	// at the WS layer). Corrupt durable content stays ErrInvalidContent
	// and is never mapped here.
	ErrPersistence = errors.New("persistence_unavailable")
	// ErrNotFound reports no live character in the requested slot.
	ErrNotFound = errors.New("character_not_found")
	// ErrInvalidContent reports broken trusted creation metadata or
	// corrupt durable state. This is an internal configuration/server
	// invariant failure, never a client budget/name error.
	ErrInvalidContent = errors.New("invalid_content")
)

// Stat indexes into CreateRequest.Stats (spec §9 order).
const (
	StatMight     = 0
	StatIntellect = 1
	StatStamina   = 2
	StatAgility   = 3
	StatMysticism = 4
	StatAim       = 5
)

// Creation school IDs for karma seeding (spec §9). These live in the
// character/content domain, not in any protocol wire namespace.
const (
	SchoolShalille int16 = 1
	SchoolQor      int16 = 2
)

// Ability point costs by offered level (spec §9).
const (
	Level1Cost = 10
	Level2Cost = 25
)

// MaxAbilityBudget is the 45-point creation budget (spec §9).
const MaxAbilityBudget = 45

// Name bounds in Unicode code points after NFC (spec §9).
const (
	MinNamePoints = 3
	MaxNamePoints = 16
)

// NamePolicy is an immutable display-name policy: blocked + reserved
// exact-name sets, normalized at construction with the same NFC +
// Unicode case-fold logic used for lookup. The zero value matches
// nothing; construct with NewNamePolicy.
type NamePolicy struct {
	blocked  map[string]struct{}
	reserved map[string]struct{}
}

// NewNamePolicy builds a policy from blocked/reserved names. Entries
// are copied into fresh lookup maps (caller slices are never retained
// or mutated) and keyed by NFC + case-fold; entries that could never
// be valid client names simply never match.
func NewNamePolicy(blocked, reserved []string) NamePolicy {
	p := NamePolicy{
		blocked:  make(map[string]struct{}, len(blocked)),
		reserved: make(map[string]struct{}, len(reserved)),
	}
	for _, n := range blocked {
		p.blocked[foldKey(n)] = struct{}{}
	}
	for _, n := range reserved {
		p.reserved[foldKey(n)] = struct{}{}
	}
	return p
}

// foldKey is the blocklist/reserved comparison key: NFC normalize,
// then Unicode case-fold. It performs no syntax validation; policy
// data is trusted configuration.
func foldKey(name string) string {
	return cases.Fold().String(norm.NFC.String(name))
}

// Normalize validates a submitted display name and returns the
// persistable NFC-normalized form (spec §9): NFC first, 3–16 code
// points, only Letter/Mark/Number plus space/apostrophe/hyphen, then
// exact folded blocklist/reserved lookup. Nothing is trimmed,
// collapsed, lowercased, or otherwise rewritten.
func (p NamePolicy) Normalize(name string) (string, error) {
	n := norm.NFC.String(name)
	if c := utf8.RuneCountInString(n); c < MinNamePoints || c > MaxNamePoints {
		return "", fmt.Errorf("character: name code points = %d, want %d..%d: %w",
			c, MinNamePoints, MaxNamePoints, ErrInvalidName)
	}
	for _, r := range n {
		if unicode.Is(unicode.Letter, r) ||
			unicode.Is(unicode.Mark, r) ||
			unicode.Is(unicode.Number, r) ||
			r == ' ' || r == '\'' || r == '-' {
			continue
		}
		return "", fmt.Errorf("character: disallowed rune U+%04X: %w", r, ErrInvalidName)
	}
	key := cases.Fold().String(n)
	if _, ok := p.blocked[key]; ok {
		return "", fmt.Errorf("character: name unavailable: %w", ErrNameUnavailable)
	}
	if _, ok := p.reserved[key]; ok {
		return "", fmt.Errorf("character: name unavailable: %w", ErrNameUnavailable)
	}
	return n, nil
}

// Face mirrors proto.CharacterFace as a small character-domain value
// so the service never depends on protocol structs.
type Face struct {
	HairStyle uint8
	HairColor uint8
	SkinTone  uint8
	Parts     [5]uint8
}

// CreateRequest carries the semantic data of opcode 122, independent
// of any transport.
type CreateRequest struct {
	Slot     uint8
	Name     string
	Gender   uint8
	Face     Face
	Stats    [6]uint8 // Might..Aim in Stat* order
	SpellIDs []uint16
	SkillIDs []uint16
}

// AbilitySpec describes one ability offered (or granted) at creation.
// School selects the karma classification for spells and is ignored
// for skills.
type AbilitySpec struct {
	ID             uint16
	Level          uint8
	Offered        bool
	InitialAbility int16
	School         int16
}

// StarterItem is one trusted starter-inventory template.
type StarterItem struct {
	ProtoID  int32
	Qty      int32
	Hits     int32
	Slot     string
	Enchants []byte // nil/empty means {}
}

// StarterProfile is the trusted starter package: free spells,
// inventory templates, hometown, and spawn position in millimeters.
type StarterProfile struct {
	Spells   []AbilitySpec
	Items    []StarterItem
	Hometown string
	PosX     int64
	PosY     int64
	PosZ     int64
}

// Content resolves creation metadata without database queries in the
// validation loop (spec §8.2 architecture). Implementations are
// immutable; tests use StaticContent, M9 backs it with real data.
type Content interface {
	Spell(id uint16) (AbilitySpec, bool)
	Skill(id uint16) (AbilitySpec, bool)
	Starter() StarterProfile
}

// StaticContent is a map-backed immutable Content holder for tests
// (and any future static composition).
type StaticContent struct {
	Spells  map[uint16]AbilitySpec
	Skills  map[uint16]AbilitySpec
	Profile StarterProfile
}

// Spell implements Content.
func (c StaticContent) Spell(id uint16) (AbilitySpec, bool) {
	s, ok := c.Spells[id]
	return s, ok
}

// Skill implements Content.
func (c StaticContent) Skill(id uint16) (AbilitySpec, bool) {
	s, ok := c.Skills[id]
	return s, ok
}

// Starter implements Content.
func (c StaticContent) Starter() StarterProfile {
	return c.Profile
}

// validateContent rejects broken trusted creation metadata as an
// internal error (spec §9 / B10). Client input is never blamed for
// server content corruption.
func validateContent(c Content) error {
	if c == nil {
		return fmt.Errorf("character: nil creation content: %w", ErrInvalidContent)
	}
	st := c.Starter()
	if st.Hometown == "" {
		return fmt.Errorf("character: empty starter hometown: %w", ErrInvalidContent)
	}
	seen := make(map[uint16]struct{}, len(st.Spells))
	for _, s := range st.Spells {
		if _, dup := seen[s.ID]; dup {
			return fmt.Errorf("character: duplicate free spell %d: %w", s.ID, ErrInvalidContent)
		}
		seen[s.ID] = struct{}{}
		if s.InitialAbility < 1 || s.InitialAbility > 99 {
			return fmt.Errorf("character: free spell %d ability %d out of 1..99: %w",
				s.ID, s.InitialAbility, ErrInvalidContent)
		}
	}
	for _, it := range st.Items {
		if it.ProtoID <= 0 {
			return fmt.Errorf("character: starter item proto %d: %w", it.ProtoID, ErrInvalidContent)
		}
		if it.Qty <= 0 {
			return fmt.Errorf("character: starter item proto %d qty %d: %w",
				it.ProtoID, it.Qty, ErrInvalidContent)
		}
		if it.Slot == "" {
			return fmt.Errorf("character: starter item proto %d empty slot: %w",
				it.ProtoID, ErrInvalidContent)
		}
		if len(it.Enchants) > 0 && !json.Valid(it.Enchants) {
			return fmt.Errorf("character: starter item proto %d bad enchants: %w",
				it.ProtoID, ErrInvalidContent)
		}
	}
	return nil
}

// ValidateStats enforces the §9 stat rules: each 1..50, sum ≤ 200.
func ValidateStats(stats [6]uint8) error {
	sum := 0
	for i, v := range stats {
		if v < 1 || v > 50 {
			return fmt.Errorf("character: stat[%d] = %d, want 1..50: %w", i, v, ErrBadStats)
		}
		sum += int(v)
	}
	if sum > 200 {
		return fmt.Errorf("character: stat sum = %d, want <= 200: %w", sum, ErrBadStats)
	}
	return nil
}

// resolvedAbility is one accepted selection with its persisted value.
type resolvedAbility struct {
	ID      uint16
	Initial int16
	School  int16
}

// resolveAbilities validates client-selected IDs against the resolver:
// known, offered at level 1/2, no duplicates, no explicit free-spell
// selection; total cost ≤ 45. Spell and skill IDs are separate
// namespaces. Every resolved initial ability must be 1..99 (trusted
// data outside that is an internal error, not bad_budget).
func resolveAbilities(
	c Content,
	spellIDs, skillIDs []uint16,
	free map[uint16]struct{},
) (spells, skills []resolvedAbility, schools map[int16]bool, err error) {
	schools = make(map[int16]bool)
	cost := 0
	resolve := func(kind string, ids []uint16, lookup func(uint16) (AbilitySpec, bool)) ([]resolvedAbility, error) {
		seen := make(map[uint16]struct{}, len(ids))
		out := make([]resolvedAbility, 0, len(ids))
		for _, id := range ids {
			if _, dup := seen[id]; dup {
				return nil, fmt.Errorf("character: duplicate %s %d: %w", kind, id, ErrBadBudget)
			}
			seen[id] = struct{}{}
			spec, ok := lookup(id)
			if !ok {
				return nil, fmt.Errorf("character: unknown %s %d: %w", kind, id, ErrBadBudget)
			}
			if !spec.Offered {
				return nil, fmt.Errorf("character: %s %d not offered: %w", kind, id, ErrBadBudget)
			}
			switch spec.Level {
			case 1:
				cost += Level1Cost
			case 2:
				cost += Level2Cost
			default:
				return nil, fmt.Errorf("character: %s %d level %d: %w", kind, id, spec.Level, ErrBadBudget)
			}
			if spec.InitialAbility < 1 || spec.InitialAbility > 99 {
				return nil, fmt.Errorf("character: %s %d ability %d out of 1..99: %w",
					kind, id, spec.InitialAbility, ErrInvalidContent)
			}
			out = append(out, resolvedAbility{ID: id, Initial: spec.InitialAbility, School: spec.School})
		}
		return out, nil
	}
	var rerr error
	if spells, rerr = resolve("spell", spellIDs, c.Spell); rerr != nil {
		return nil, nil, nil, rerr
	}
	for _, s := range spells {
		if _, isFree := free[s.ID]; isFree {
			return nil, nil, nil, fmt.Errorf("character: free spell %d explicitly selected: %w",
				s.ID, ErrBadBudget)
		}
		schools[s.School] = true
	}
	if skills, rerr = resolve("skill", skillIDs, c.Skill); rerr != nil {
		return nil, nil, nil, rerr
	}
	if cost > MaxAbilityBudget {
		return nil, nil, nil, fmt.Errorf("character: ability cost = %d, want <= %d: %w",
			cost, MaxAbilityBudget, ErrBadBudget)
	}
	return spells, skills, schools, nil
}

// karmaFor seeds initial karma from the final starting spell set
// (spec §9): Qor-only −20, Shalille-only +20, otherwise 0. Free
// starter spells participate via schools.
func karmaFor(schools map[int16]bool) int32 {
	qor, shalille := schools[SchoolQor], schools[SchoolShalille]
	switch {
	case qor && !shalille:
		return -20
	case shalille && !qor:
		return 20
	default:
		return 0
	}
}

// Vitals is the typed initial-vitals representation (spec §9),
// persisted as JSON.
type Vitals struct {
	HP        int32 `json:"hp"`
	BaseMax   int32 `json:"base_max"`
	Max       int32 `json:"max"`
	Mana      int32 `json:"mana"`
	MaxMana   int32 `json:"max_mana"`
	Vigor     int32 `json:"vigor"`
	Threshold int32 `json:"threshold"`
	Stomach   int32 `json:"stomach"`
}

// NewVitals builds initial vitals: hp/base_max/max 20,
// mana = 15 + mysticism/5 (integer division), vigor 100,
// threshold 80, stomach 0.
func NewVitals(mysticism uint8) Vitals {
	mana := int32(15 + mysticism/5)
	return Vitals{
		HP: 20, BaseMax: 20, Max: 20,
		Mana: mana, MaxMana: mana,
		Vigor: 100, Threshold: 80, Stomach: 0,
	}
}

// faceJSON is the persisted face representation (spec §9 §A14).
type faceJSON struct {
	HairStyle uint8    `json:"hair_style"`
	HairColor uint8    `json:"hair_color"`
	SkinTone  uint8    `json:"skin_tone"`
	Parts     [5]uint8 `json:"parts"`
}

// Repository is the narrow persistence seam satisfied by
// *store.PGStore. Character-domain types only; T3b holds the session
// lifecycle guard around these calls.
type Repository interface {
	CreateCharacter(ctx context.Context, c store.NewCharacter) (int64, error)
	ListLiveCharacters(ctx context.Context, accountID int64) ([]store.LiveCharacter, error)
	SoftDeleteCharacter(ctx context.Context, id, expectedRevision int64) (int64, error)
}

// Service orchestrates character creation, listing, lookup, and
// delete against validated content, policy, and repository.
type Service struct {
	content Content
	policy  NamePolicy
	repo    Repository
}

// NewService validates the trusted starter content once and wires the
// service. All three dependencies are required.
func NewService(content Content, policy NamePolicy, repo Repository) (*Service, error) {
	if err := validateContent(content); err != nil {
		return nil, err
	}
	if repo == nil {
		return nil, fmt.Errorf("character: nil repository: %w", ErrInvalidContent)
	}
	return &Service{content: content, policy: policy, repo: repo}, nil
}

// Create validates a creation request and persists the full aggregate
// (root + abilities + free spell + starter inventory) in one store
// transaction. Store unique-violations translate to the character
// boundary (ErrNameUnavailable / ErrSlotOccupied); any other
// repository failure becomes ErrPersistence. Store errors never escape.
func (s *Service) Create(ctx context.Context, accountID int64, req CreateRequest) (int64, error) {
	if req.Slot > 1 {
		return 0, fmt.Errorf("character: slot = %d, want 0/1: %w", req.Slot, ErrInvalidSlot)
	}
	name, err := s.policy.Normalize(req.Name)
	if err != nil {
		return 0, err
	}
	if err := ValidateStats(req.Stats); err != nil {
		return 0, err
	}
	st := s.content.Starter()
	free := make(map[uint16]struct{}, len(st.Spells))
	for _, f := range st.Spells {
		free[f.ID] = struct{}{}
	}
	spells, skills, schools, err := resolveAbilities(s.content, req.SpellIDs, req.SkillIDs, free)
	if err != nil {
		return 0, err
	}
	for _, f := range st.Spells {
		schools[f.School] = true
	}
	vitals, err := json.Marshal(NewVitals(req.Stats[StatMysticism]))
	if err != nil {
		return 0, fmt.Errorf("character: marshal vitals: %w", err)
	}
	face, err := json.Marshal(faceJSON{
		HairStyle: req.Face.HairStyle,
		HairColor: req.Face.HairColor,
		SkinTone:  req.Face.SkinTone,
		Parts:     req.Face.Parts,
	})
	if err != nil {
		return 0, fmt.Errorf("character: marshal face: %w", err)
	}
	nc := store.NewCharacter{
		AccountID:   accountID,
		Slot:        int16(req.Slot),
		Name:        name,
		Gender:      int16(req.Gender),
		Face:        face,
		Might:       int16(req.Stats[StatMight]),
		Intellect:   int16(req.Stats[StatIntellect]),
		Stamina:     int16(req.Stats[StatStamina]),
		Agility:     int16(req.Stats[StatAgility]),
		Mysticism:   int16(req.Stats[StatMysticism]),
		Aim:         int16(req.Stats[StatAim]),
		Karma:       karmaFor(schools),
		Hometown:    st.Hometown,
		PosX:        st.PosX,
		PosY:        st.PosY,
		PosZ:        st.PosZ,
		Vitals:      vitals,
		Advancement: []byte("{}"),
		Flags:       0,
	}
	for _, f := range st.Spells {
		nc.Spells = append(nc.Spells, store.NewCharacterAbility{ID: int32(f.ID), Ability: f.InitialAbility})
	}
	for _, sp := range spells {
		nc.Spells = append(nc.Spells, store.NewCharacterAbility{ID: int32(sp.ID), Ability: sp.Initial})
	}
	for _, sk := range skills {
		nc.Skills = append(nc.Skills, store.NewCharacterAbility{ID: int32(sk.ID), Ability: sk.Initial})
	}
	for _, it := range st.Items {
		nc.Items = append(nc.Items, store.NewCharacterItem{
			ProtoID:  it.ProtoID,
			Qty:      it.Qty,
			Hits:     it.Hits,
			Enchants: it.Enchants,
			Slot:     it.Slot,
		})
	}
	id, err := s.repo.CreateCharacter(ctx, nc)
	if err != nil {
		if errors.Is(err, store.ErrNameTaken) {
			return 0, fmt.Errorf("character: name race lost: %w", ErrNameUnavailable)
		}
		if errors.Is(err, store.ErrSlotOccupied) {
			return 0, fmt.Errorf("character: slot race lost: %w", ErrSlotOccupied)
		}
		return 0, fmt.Errorf("character: persist: %w: %w", err, ErrPersistence)
	}
	return id, nil
}

// ListEntry is one domain-level character-list row (spec §6.1):
// slot, name, and level sourced from vitals.base_max.
type ListEntry struct {
	Slot  uint8
	Name  string
	Level uint16
}

// List returns live characters for one account as list rows.
// Repository failure becomes ErrPersistence; corrupt durable vitals
// stay ErrInvalidContent (a server invariant failure, never retry).
func (s *Service) List(ctx context.Context, accountID int64) ([]ListEntry, error) {
	rows, err := s.repo.ListLiveCharacters(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("character: list: %w: %w", err, ErrPersistence)
	}
	out := make([]ListEntry, 0, len(rows))
	for _, r := range rows {
		level, err := levelFromVitals(r.Vitals)
		if err != nil {
			return nil, fmt.Errorf("character %d: %w", r.ID, err)
		}
		out = append(out, ListEntry{Slot: uint8(r.Slot), Name: r.Name, Level: level})
	}
	return out, nil
}

// levelFromVitals extracts vitals.base_max as the list level. Missing,
// malformed, or out-of-u16-range values are an internal invariant
// failure — level 0 is never silently emitted.
func levelFromVitals(raw []byte) (uint16, error) {
	var v struct {
		BaseMax int64 `json:"base_max"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, fmt.Errorf("malformed vitals: %w: %w", err, ErrInvalidContent)
	}
	if v.BaseMax < 1 || v.BaseMax > 0xFFFF {
		return 0, fmt.Errorf("character: base_max = %d: %w", v.BaseMax, ErrInvalidContent)
	}
	return uint16(v.BaseMax), nil
}

// Descriptor is the live-character lookup result sufficient for
// T3b/T4: durable ID, slot, name, and revision for CAS delete.
type Descriptor struct {
	ID       int64
	Slot     uint8
	Name     string
	Revision int64
}

// FindBySlot locates the live character in one of the account's at
// most two slots (reusing the list primitive; no extra query).
func (s *Service) FindBySlot(ctx context.Context, accountID int64, slot uint8) (Descriptor, error) {
	if slot > 1 {
		return Descriptor{}, fmt.Errorf("character: slot = %d, want 0/1: %w", slot, ErrInvalidSlot)
	}
	rows, err := s.repo.ListLiveCharacters(ctx, accountID)
	if err != nil {
		return Descriptor{}, fmt.Errorf("character: lookup: %w: %w", err, ErrPersistence)
	}
	for _, r := range rows {
		if r.Slot == int16(slot) {
			return Descriptor{ID: r.ID, Slot: slot, Name: r.Name, Revision: r.Revision}, nil
		}
	}
	return Descriptor{}, fmt.Errorf("character: no live character in slot %d: %w", slot, ErrNotFound)
}

// Delete soft-deletes by durable ID + expected revision (store CAS).
// A stale CAS becomes ErrPersistence without exposing the store
// sentinel; every other repository failure becomes ErrPersistence with
// its cause preserved (→ 202 retry at the WS layer). The store's CAS
// semantics are unchanged. Session in-use checks belong to T3b under
// the account guard, not here.
func (s *Service) Delete(ctx context.Context, id, expectedRevision int64) (int64, error) {
	rev, err := s.repo.SoftDeleteCharacter(ctx, id, expectedRevision)
	if err != nil {
		if errors.Is(err, store.ErrStaleRevision) {
			return 0, fmt.Errorf("character: delete stale revision: %w", ErrPersistence)
		}
		return 0, fmt.Errorf("character: delete: %w: %w", err, ErrPersistence)
	}
	return rev, nil
}

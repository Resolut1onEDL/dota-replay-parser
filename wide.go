package main

// v4.9.0 «wide» export: raw timelines next to the pre-aggregated counters, so a
// new metric over a different window or a different pair of heroes is a
// recompute on the stored JSON, not a reparse of the .dem — Valve deletes pub
// replays after ~14 days. All four streams are additive; no existing field
// changes meaning.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dotabuff/manta"
	"github.com/dotabuff/manta/dota"
)

// Attacker codes of DamageSecond.A for damage that no hero dealt.
const (
	srcLaneCreep = -1
	srcNeutral   = -2
	srcBuilding  = -3
	srcOther     = -4 // summons, Roshan, anything without a hero name
)

// DamageSecond is the damage one source dealt to one real hero within one game
// second, by damage type. A player's illusions, summons and dominated creeps
// count as the player (same rule as heroDamage); damage to illusions and
// Sunder's HP swap are not recorded. Hits on a Spirit Bear are its owner's,
// marked U (heroDamage counts them; damage taken by the hero itself does not).
type DamageSecond struct {
	T int `json:"t"`           // floor of the game second, 0 = horn
	A int `json:"a"`           // attacker slot 0-9, or a src* code
	V int `json:"v"`           // victim slot 0-9
	K int `json:"k"`           // damage type: 1 physical, 2 magical, 4 pure
	D int `json:"d"`           // damage summed over the second
	U int `json:"u,omitempty"` // 1: the hits landed on the victim's Spirit Bear
}

// ControlEvent is a control modifier (the combat log gives it a stun or a
// slow duration) applied to or removed from a real hero.
type ControlEvent struct {
	T    float64 `json:"t"`
	P    int     `json:"p"` // slot of the hero carrying it
	A    int     `json:"a"` // caster slot, -1 when not a hero
	M    string  `json:"m"`
	Ab   string  `json:"ab,omitempty"` // ability or item that applied it, when logged
	Stun float64 `json:"stun,omitempty"`
	Slow float64 `json:"slow,omitempty"`
	On   bool    `json:"on"` // true applied, false removed
}

// Vitals are a hero's HP and mana once per game second from the horn; index =
// second. A second without an entity update carries the previous value.
type Vitals struct {
	HP      []int `json:"hp"`
	HPMax   []int `json:"hpMax"`
	Mana    []int `json:"mana"`
	ManaMax []int `json:"manaMax"`
}

// ItemGain is an item appearing on the hero (inventory, backpack, neutral
// slot): a new item entity, or more charges on one already held. Purchases
// show up here on arrival; Healing Lotuses (item_famango, item_great_famango,
// item_greater_famango) only ever arrive this way.
type ItemGain struct {
	T    float64 `json:"t"`
	Item string  `json:"item"`
	N    int     `json:"n"` // charges gained (1 for an item without charges)
}

type damageHit struct {
	T          float64
	A, V, K, D int
	U          bool // on the victim's Spirit Bear
}

type wideTrack struct {
	HP, HPMax, Mana, ManaMax []int
	ItemGains                []ItemGain
	itemSeen                 map[uint32]int // item handle → charges last seen
}

func damageSource(attackerName string, state *ParserState) int {
	if strings.HasPrefix(attackerName, "npc_dota_hero_") {
		if idx := heroNameToPlayerIndex(attackerName, state); idx >= 0 && idx < 10 {
			return idx
		}
		return srcOther
	}
	switch {
	case isLaneCreepName(attackerName):
		return srcLaneCreep
	case isNeutralCreepName(attackerName):
		return srcNeutral
	case isBuildingTarget(attackerName):
		return srcBuilding
	}
	return srcOther
}

func isSpiritBear(name string) bool { return strings.HasPrefix(name, "npc_dota_lone_druid_bear") }

// damageVictim is the player a damage entry hits: a real hero's, or the owner of a Spirit Bear — Valve's hero
// damage counts the hits on the bear, though its death is no kill (OpenDota's hero_damage_t counts them too).
// The owner is the target's source hero, else the hero whose name the bear's starts with; -1 for anything else.
func (s *ParserState) damageVictim(targetName, targetSource string) int {
	if strings.HasPrefix(targetName, "npc_dota_hero_") {
		return heroNameToPlayerIndex(targetName, s)
	}
	if isSpiritBear(targetName) {
		return s.ownerByNames(targetSource, targetName)
	}
	return -1
}

// recordDamage keeps one combat-log damage entry whose victim is a real hero or a Spirit Bear.
func (s *ParserState) recordDamage(m *dota.CMsgDOTACombatLogEntry, t float64, attackerName, targetName string, damage int, damageType uint32) {
	if damage <= 0 || m.GetIsTargetIllusion() || s.LookupName(m.GetInflictorName()) == "terrorblade_sunder" {
		return
	}
	v := s.damageVictim(targetName, s.LookupName(m.GetTargetSourceName()))
	if v < 0 || v >= 10 {
		return
	}
	// a player's summons, dominated creeps and illusions count as the player (heroDamage's rule)
	a := s.damageOwner(m, attackerName)
	if a < 0 {
		a = damageSource(attackerName, s)
	}
	s.DamageHits = append(s.DamageHits, damageHit{T: t, A: a, V: v, K: int(damageType), D: damage, U: isSpiritBear(targetName)})
}

// combatSeconds sums the hits per (second, attacker, victim, type), in time order.
func damageSeconds(hits []damageHit) []DamageSecond {
	type key struct {
		t, a, v, k int
		u          bool
	}
	sums := make(map[key]int)
	for _, h := range hits {
		t := int(h.T)
		if h.T < 0 && float64(t) != h.T {
			t-- // floor, not truncation, for pregame seconds
		}
		sums[key{t, h.A, h.V, h.K, h.U}] += h.D
	}
	out := make([]DamageSecond, 0, len(sums))
	for k, d := range sums {
		u := 0
		if k.u {
			u = 1
		}
		out = append(out, DamageSecond{T: k.t, A: k.a, V: k.v, K: k.k, D: d, U: u})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.T != b.T {
			return a.T < b.T
		}
		if a.V != b.V {
			return a.V < b.V
		}
		if a.A != b.A {
			return a.A < b.A
		}
		if a.K != b.K {
			return a.K < b.K
		}
		return a.U < b.U
	})
	return out
}

// recordModifier keeps control modifiers on real heroes: an add with a stun or
// slow duration, and the matching remove.
func (s *ParserState) recordModifier(m *dota.CMsgDOTACombatLogEntry, t float64, on bool) {
	targetName := s.LookupName(m.GetTargetName())
	if m.GetIsTargetIllusion() || !strings.HasPrefix(targetName, "npc_dota_hero_") {
		return
	}
	p := heroNameToPlayerIndex(targetName, s)
	if p < 0 || p >= 10 {
		return
	}
	mod := s.LookupName(m.GetInflictorName())
	if mod == "" {
		return
	}
	key := fmt.Sprintf("%d|%s", p, mod)
	if s.modActive == nil {
		s.modActive = make(map[string]int)
	}
	if on {
		stun, slow := float64(m.GetStunDuration()), float64(m.GetSlowDuration())
		if stun <= 0 && slow <= 0 {
			return
		}
		a := heroNameToPlayerIndex(s.LookupName(m.GetAttackerName()), s)
		if a < 0 || a >= 10 {
			a = -1
		}
		ab := s.LookupName(m.GetModifierAbility())
		if ab == "dota_unknown" || strings.HasPrefix(ab, "unknown_") {
			ab = ""
		}
		s.ModEvents = append(s.ModEvents, ControlEvent{T: t, P: p, A: a, M: mod, Ab: ab, Stun: stun, Slow: slow, On: true})
		s.modActive[key]++
		return
	}
	if s.modActive[key] > 0 {
		s.modActive[key]--
		s.ModEvents = append(s.ModEvents, ControlEvent{T: t, P: p, A: -1, M: mod, On: false})
	}
}

// sampleWide reads HP, mana and held items from a real hero's entity update.
func (s *ParserState) sampleWide(e *manta.Entity, ps *PlayerState, t float64) {
	if s.GameStartTime <= 0 || t < 0 {
		return
	}
	w := &ps.Wide
	sec := int(t)
	hp, _ := e.GetInt32("m_iHealth")
	hpMax, _ := e.GetInt32("m_iMaxHealth")
	mana, _ := e.GetFloat32("m_flMana")
	manaMax, _ := e.GetFloat32("m_flMaxMana")
	for len(w.HP) <= sec {
		last := len(w.HP) - 1
		if last < 0 {
			w.HP, w.HPMax, w.Mana, w.ManaMax = append(w.HP, int(hp)), append(w.HPMax, int(hpMax)), append(w.Mana, int(mana)), append(w.ManaMax, int(manaMax))
			continue
		}
		w.HP, w.HPMax, w.Mana, w.ManaMax = append(w.HP, w.HP[last]), append(w.HPMax, w.HPMax[last]), append(w.Mana, w.Mana[last]), append(w.ManaMax, w.ManaMax[last])
	}
	w.HP[sec], w.HPMax[sec], w.Mana[sec], w.ManaMax[sec] = int(hp), int(hpMax), int(mana), int(manaMax)

	if w.itemSeen == nil {
		w.itemSeen = make(map[uint32]int)
	}
	for _, slot := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 16} {
		handle, ok := e.GetUint32(fmt.Sprintf("m_hItems.%04d", slot))
		if !ok || handle == 0 || handle >= 16777215 {
			continue
		}
		itemEnt := s.Parser.FindEntity(int32(handle & 0x3FFF))
		if itemEnt == nil || !strings.HasPrefix(itemEnt.GetClassName(), "CDOTA_Item_") {
			continue
		}
		charges := 0
		if c, okC := itemEnt.GetInt32("m_iCurrentCharges"); okC && c > 0 {
			charges = int(c)
		}
		prev, seen := w.itemSeen[handle]
		w.itemSeen[handle] = charges
		gained := 0
		if !seen {
			gained = charges
			if gained < 1 {
				gained = 1
			}
		} else if charges > prev {
			gained = charges - prev
		}
		if gained > 0 {
			name := normalizeEntityItemName(strings.TrimPrefix(itemEnt.GetClassName(), "CDOTA_Item_"))
			w.ItemGains = append(w.ItemGains, ItemGain{T: t, Item: name, N: gained})
		}
	}
}

// vitals pads the per-second series to the game's length (carrying the last
// value) so every index up to the end is a real second.
func (w *wideTrack) vitals(duration int) *Vitals {
	if len(w.HP) == 0 {
		return nil
	}
	for len(w.HP) <= duration {
		last := len(w.HP) - 1
		w.HP, w.HPMax, w.Mana, w.ManaMax = append(w.HP, w.HP[last]), append(w.HPMax, w.HPMax[last]), append(w.Mana, w.Mana[last]), append(w.ManaMax, w.ManaMax[last])
	}
	n := duration + 1
	return &Vitals{HP: w.HP[:n], HPMax: w.HPMax[:n], Mana: w.Mana[:n], ManaMax: w.ManaMax[:n]}
}

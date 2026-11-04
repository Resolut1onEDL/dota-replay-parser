package main

import "testing"

func TestDamageSecondsSumsPerSecondAndFloorsPregame(t *testing.T) {
	got := damageSeconds([]damageHit{
		{T: 12.2, A: 3, V: 7, K: 1, D: 40},
		{T: 12.9, A: 3, V: 7, K: 1, D: 35}, // same second, same pair, same type → summed
		{T: 12.5, A: 3, V: 7, K: 2, D: 90}, // magical stays its own row
		{T: 12.5, A: srcLaneCreep, V: 7, K: 1, D: 20},
		{T: -0.4, A: 2, V: 6, K: 1, D: 15}, // pregame: second −1, not 0
	})
	want := []DamageSecond{
		{T: -1, A: 2, V: 6, K: 1, D: 15},
		{T: 12, A: srcLaneCreep, V: 7, K: 1, D: 20},
		{T: 12, A: 3, V: 7, K: 1, D: 75},
		{T: 12, A: 3, V: 7, K: 2, D: 90},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// v4.10.1: a hit on the victim's Spirit Bear keeps its own row, marked u=1.
func TestDamageSecondsKeepsBearHitsApart(t *testing.T) {
	got := damageSeconds([]damageHit{
		{T: 30.1, A: 3, V: 7, K: 1, D: 40},
		{T: 30.6, A: 3, V: 7, K: 1, D: 60, U: true},
	})
	want := []DamageSecond{{T: 30, A: 3, V: 7, K: 1, D: 40}, {T: 30, A: 3, V: 7, K: 1, D: 60, U: 1}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestDamageSourceClassifiesNonHeroAttackers(t *testing.T) {
	s := &ParserState{}
	for name, want := range map[string]int{
		"npc_dota_creep_goodguys_melee": srcLaneCreep,
		"npc_dota_neutral_kobold":       srcNeutral,
		"npc_dota_badguys_tower1_mid":   srcBuilding,
		"npc_dota_roshan":               srcOther,
	} {
		if got := damageSource(name, s); got != want {
			t.Errorf("%s: got %d, want %d", name, got, want)
		}
	}
}

func TestVitalsPadToGameEndCarryingLastValue(t *testing.T) {
	w := &wideTrack{HP: []int{600, 550}, HPMax: []int{600, 600}, Mana: []int{300, 280}, ManaMax: []int{300, 300}}
	v := w.vitals(4)
	if len(v.HP) != 5 || v.HP[4] != 550 || v.Mana[4] != 280 || v.HPMax[3] != 600 {
		t.Fatalf("padding: %+v", v)
	}
	if (&wideTrack{}).vitals(10) != nil {
		t.Fatal("a hero never sampled must have no vitals, not zeros")
	}
}

func TestObjectiveNameCountsOnlyEnemyBuildingsRoshanAndTormentor(t *testing.T) {
	cases := []struct {
		target  string
		radiant bool
		want    string
	}{
		{"npc_dota_badguys_tower1_mid", true, "badguys_tower1_mid"},
		{"npc_dota_goodguys_tower1_mid", true, ""}, // own tower: no push
		{"npc_dota_goodguys_range_rax_bot", false, "goodguys_range_rax_bot"},
		{"npc_dota_roshan", false, "roshan"},
		{"npc_dota_miniboss", true, "tormentor"},
		{"npc_dota_creep_badguys_melee", true, ""},
		{"npc_dota_hero_axe", true, ""},
	}
	for _, c := range cases {
		if got := objectiveName(c.target, c.radiant); got != c.want {
			t.Errorf("objectiveName(%q, %v) = %q, want %q", c.target, c.radiant, got, c.want)
		}
	}
}

func TestObjectiveSecondsSumsPerSecondAttackerAndObjective(t *testing.T) {
	got := objectiveSeconds([]objectiveHit{
		{T: 600.2, A: 1, O: "badguys_tower1_mid", D: 100},
		{T: 600.8, A: 1, O: "badguys_tower1_mid", D: 120}, // same second → summed
		{T: 600.5, A: 1, O: "roshan", D: 80},
		{T: 600.5, A: 0, O: "badguys_tower1_mid", D: 50},
		{T: 601.0, A: 1, O: "badguys_tower1_mid", D: 10},
	})
	want := []ObjectiveDamageSecond{
		{T: 600, A: 0, O: "badguys_tower1_mid", D: 50},
		{T: 600, A: 1, O: "badguys_tower1_mid", D: 220},
		{T: 600, A: 1, O: "roshan", D: 80},
		{T: 601, A: 1, O: "badguys_tower1_mid", D: 10},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// v4.17.0: one row per real change of a slot — a jittering cooldown read or the same item read again writes nothing.
func TestRecordSlotWritesOnlyChanges(t *testing.T) {
	w := &wideTrack{}
	w.recordSlot(100, 2, "", 0)                  // an empty slot never seen: nothing
	w.recordSlot(120, 2, "black_king_bar", 0)    // bought
	w.recordSlot(121, 2, "black_king_bar", 0)    // read again
	w.recordSlot(600, 2, "black_king_bar", 95)   // pressed: cooldown to 695
	w.recordSlot(601, 2, "black_king_bar", 94.4) // the same cooldown, a tick of jitter
	w.recordSlot(700, 2, "black_king_bar", 0)    // cooled down: no row, cdEnd already said so
	w.recordSlot(710, 2, "", 0)                  // moved out
	w.recordSlot(710, 7, "black_king_bar", 0)    // into the backpack
	want := []InvRow{
		{T: 120, S: 2, Item: "black_king_bar"},
		{T: 600, S: 2, Item: "black_king_bar", CDEnd: 695},
		{T: 710, S: 2},
		{T: 710, S: 7, Item: "black_king_bar"},
	}
	if len(w.Inventory) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(w.Inventory), len(want), w.Inventory)
	}
	for i := range want {
		if w.Inventory[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, w.Inventory[i], want[i])
		}
	}
}

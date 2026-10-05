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

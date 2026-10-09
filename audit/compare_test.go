package audit

import (
	"encoding/json"
	"strings"
	"testing"
)

func decode(t *testing.T, raw string, out interface{}) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatal(err)
	}
}

func byField(checks []Check) map[string][]Check {
	m := map[string][]Check{}
	for _, c := range checks {
		m[c.Field] = append(m[c.Field], c)
	}
	return m
}

// One radiant player against OpenDota's answer: the scoreboard exactly, items in any slot order, purchases
// paired within a second, a 64-bit Steam id read as the 32-bit account id.
const oursJSON = `{"id":1,"gameMode":22,"lobbyType":7,"didRadiantWin":true,"durationSeconds":1800,
 "players":[{"steamAccountId":76561198072603265,"heroId":6,"isRadiant":true,"isVictory":true,
  "kills":3,"deaths":1,"assists":2,"numLastHits":100,"numDenies":5,"goldPerMinute":500,"experiencePerMinute":600,
  "networth":15000,"level":20,"heroDamage":12000,"towerDamage":800,"heroHealing":0,
  "item0Id":63,"item1Id":147,"neutral0Id":1605,
  "stats":{"killEvents":[{"time":636.2,"target":5,"targetName":"npc_dota_hero_witch_doctor"},{"time":700,"target":5,"targetName":"npc_dota_hero_witch_doctor"},{"time":900,"target":5,"targetName":"npc_dota_hero_witch_doctor"}],
   "itemPurchases":[{"time":10.4,"itemName":"item_tango"},{"time":136.5,"itemName":"item_boots"}],
   "abilityCastReport":[{"abilityName":"drow_ranger_multishot","count":64},{"abilityName":"item_tpscroll","count":7}],
   "itemUsed":[{"itemName":"item_tpscroll","count":7}],"tpCount":7,
   "lastHitsPerMinute":[0,5,12],"skillBuild":[{"time":1,"abilityName":"drowranger_frostarrows"}]}}]}`

const odJSON = `{"match_id":1,"radiant_win":true,"duration":1800,"game_mode":22,"lobby_type":7,"radiant_score":3,"dire_score":1,"version":22,
 "players":[{"player_slot":0,"hero_id":6,"account_id":112337537,"kills":3,"deaths":1,"assists":2,"last_hits":100,"denies":5,
  "gold_per_min":500,"xp_per_min":600,"net_worth":15000,"level":20,"hero_damage":11000,"tower_damage":800,"hero_healing":0,
  "item_0":147,"item_1":63,"item_neutral":1605,
  "kills_log":[{"time":636,"key":"npc_dota_hero_witch_doctor"},{"time":700,"key":"npc_dota_hero_witch_doctor"},{"time":900,"key":"npc_dota_hero_witch_doctor"}],
  "purchase_log":[{"time":-89,"key":"faerie_fire"},{"time":10,"key":"tango"},{"time":136,"key":"boots"}],
  "ability_uses":{"drow_ranger_multishot":64},"item_uses":{"tpscroll":7},"lh_t":[0,5,12],"ability_upgrades_arr":[5019]}]}`

func TestCompareScoreboardItemsAndLogs(t *testing.T) {
	var ours Match
	var od ODMatch
	decode(t, oursJSON, &ours)
	decode(t, odJSON, &od)
	got := byField(Compare(&ours, &od, Consts{Abilities: map[int]string{5019: "drow_ranger_frost_arrows"}}))

	for _, f := range []string{"player.kills", "player.items", "player.neutral_item", "player.account_id", "player.kills_log",
		"player.ability_uses", "player.item_uses", "player.tp_uses", "player.lh_t", "player.ability_upgrades", "match.lobby_type"} {
		if len(got[f]) != 1 || !got[f][0].OK {
			t.Errorf("%s: want one passing check, got %+v", f, got[f])
		}
	}
	if c := got["player.hero_damage"]; len(c) != 1 || c[0].OK || c[0].Note != "Δ +1000" {
		t.Errorf("hero_damage: want a mismatch noted Δ +1000, got %+v", c)
	}
	// the starting item OpenDota saw and we did not (starting items compare as one pre-horn set: @-1)
	if c := got["player.purchase_log"]; len(c) != 1 || c[0].OK || !strings.Contains(c[0].Note, "missing 1 [faerie_fire@-1]") {
		t.Errorf("purchase_log: want the pre-horn faerie fire missing, got %+v", c)
	}
	if c := got["consistency.kill_events"]; len(c) != 1 || !c[0].OK {
		t.Errorf("kill events vs kills: %+v", c)
	}
}

// Valve's scoreboard does not count a Spirit Bear kill; OpenDota's kills_log does (match 9034607477: Viper 11 kills, 16 logged).
func TestKillsLogSkipsSpiritBear(t *testing.T) {
	var ours Match
	var od ODMatch
	decode(t, oursJSON, &ours)
	decode(t, strings.Replace(odJSON, `{"time":900,"key":"npc_dota_hero_witch_doctor"}`,
		`{"time":900,"key":"npc_dota_hero_witch_doctor"},{"time":1000,"key":"npc_dota_lone_druid_bear1"}`, 1), &od)
	if c := byField(Compare(&ours, &od, Consts{}))["player.kills_log"]; len(c) != 1 || !c[0].OK {
		t.Errorf("kills_log with a Spirit Bear death: want one passing check, got %+v", c)
	}
}

func TestCompareUnparsedMatchHasValveChecksOnly(t *testing.T) {
	var ours Match
	var od ODMatch
	decode(t, oursJSON, &ours)
	decode(t, strings.Replace(odJSON, `"version":22,`, `"version":null,`, 1), &od)
	for _, c := range Compare(&ours, &od, Consts{}) {
		if c.Group == GroupReplay || c.Field == "match.teamfights" || c.Field == "player.lane_role" {
			t.Errorf("%s compared although OpenDota has not parsed the replay", c.Field)
		}
	}
}

// OpenDota samples a minute up to a second early: our value at m:00 may run ahead of its minute m, never past m+1.
func TestTimelineBracket(t *testing.T) {
	var rows []Check
	add := func(field, group string, hero int, o, th interface{}, ok bool, note string) {
		rows = append(rows, Check{Field: field, OK: ok, Note: note})
	}
	timeline(add, "lh", 1, []int{0, 5, 13, 99}, []int{0, 4, 12, 20}, 0, 0)
	timeline(add, "lh", 1, []int{0, 3, 13}, []int{0, 4, 12}, 0, 0)
	timeline(add, "lh", 1, []int{0, 13, 20}, []int{0, 4, 12}, 0, 0)
	if !rows[0].OK || rows[1].OK || rows[2].OK {
		t.Errorf("want ahead-within-a-minute OK, behind and past-next-minute failing (the last minute is not compared): %+v", rows)
	}
	// with a minute of slack back, minute 0 may lag OpenDota's (a rune fight at 0:00 that it rounds into minute 0)
	rows = nil
	timeline(add, "dmg", 1, []int{65, 700, 1500}, []int{104, 638, 1545}, 1, 0)
	timeline(add, "dmg", 1, []int{65, 700, 1500}, []int{104, 638, 1545}, 0, 0)
	if !rows[0].OK || rows[1].OK {
		t.Errorf("minute 0 behind OpenDota's: want OK with back=1, failing with back=0: %+v", rows)
	}
}

func TestEventPairingTolerance(t *testing.T) {
	var rows []Check
	add := func(field, group string, hero int, o, th interface{}, ok bool, note string) {
		rows = append(rows, Check{Field: field, OK: ok, Note: note})
	}
	evCheck(add, "x", GroupReplay, 1, []ev{{T: 11, K: "a"}, {T: 50, K: "b"}}, []ev{{T: 10, K: "a"}, {T: 52, K: "b"}}, 1, -1)
	if rows[0].OK || !strings.Contains(rows[0].Note, "missing 1 [b@52]; extra 1 [b@50]") {
		t.Errorf("want a within 1 s and b 2 s off, got %+v", rows[0])
	}
	rows = nil
	evCheck(add, "x", GroupReplay, 1, nil, nil, 1, -1)
	if len(rows) != 0 {
		t.Errorf("two empty logs make no row, got %+v", rows)
	}
}

func TestAggregateBaselineRegressions(t *testing.T) {
	checks := []Check{
		{Field: "player.kills", Group: GroupValve, OK: true}, {Field: "player.kills", Group: GroupValve, OK: true},
		{Field: "player.stuns", Group: GroupReplay, OK: true}, {Field: "player.stuns", Group: GroupReplay, OK: false},
		{Field: "player.lane_role", Group: GroupHeuristic, OK: false},
	}
	stats := Aggregate(checks)
	if stats[0].Field != "player.kills" || stats[0].Rate != 1 || stats[1].Field != "player.stuns" || stats[1].Rate != 0.5 {
		t.Fatalf("aggregate order/rates: %+v", stats)
	}
	b := NewBaseline(stats)
	if b["player.kills"] != 1 || b["player.stuns"] != 0.5 {
		t.Errorf("baseline: %+v", b)
	}
	if _, gated := b["player.lane_role"]; gated {
		t.Errorf("a heuristic field must not be gated")
	}
	// a 100 % field fails on one miss; a 50 % field over 2 rows may swing by its sampling noise
	worse := Aggregate(append(checks, Check{Field: "player.kills", Group: GroupValve, OK: false}, Check{Field: "player.stuns", Group: GroupReplay, OK: false}))
	if r := Regressions(worse, b); len(r) != 1 || !strings.HasPrefix(r[0], "player.kills: 66.7%") {
		t.Errorf("regressions: %v", r)
	}
	many := []Check{}
	for i := 0; i < 1000; i++ {
		many = append(many, Check{Field: "player.stuns", Group: GroupReplay, OK: i < 400})
	}
	if r := Regressions(Aggregate(many), b); len(r) != 1 || !strings.HasPrefix(r[0], "player.stuns: 40.0%") {
		t.Errorf("a 10-point drop over 1000 rows is no noise: %v", r)
	}
}

func TestSteamIDTo32(t *testing.T) {
	for raw, want := range map[string]SteamID{`76561198072603265`: 112337537, `"76561198072603265"`: 112337537, `112337537`: 112337537, `null`: 0} {
		var s SteamID
		if err := json.Unmarshal([]byte(raw), &s); err != nil || s != want {
			t.Errorf("%s → %d, want %d (%v)", raw, s, want, err)
		}
	}
}

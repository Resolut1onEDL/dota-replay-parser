package audit

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Groups of checks, by who the reference is.
const (
	// GroupValve: Valve's own numbers (the post-game scoreboard, items, score) — exact truth.
	GroupValve = "valve"
	// GroupReplay: OpenDota's parse of the same replay — a second opinion; present only when OpenDota parsed it.
	GroupReplay = "replay"
	// GroupHeuristic: information only, never a gate — both sides are heuristics (lanes, teamfights), or the
	// two sides count by different rules on purpose (damage taken, Valve's own exclusions, assist lists).
	GroupHeuristic = "heuristic"
	// GroupConsistency: the parser against itself (events vs totals) — no public source needed.
	GroupConsistency = "consistency"
)

// Check is one compared value: one player's field, or one match-level field.
type Check struct {
	Field   string `json:"field"`
	Group   string `json:"group"`
	MatchID int64  `json:"matchId"`
	HeroID  int    `json:"heroId,omitempty"`
	Ours    string `json:"ours"`
	Theirs  string `json:"theirs"`
	OK      bool   `json:"ok"`
	Note    string `json:"note,omitempty"`
}

// odTimeTol: OpenDota's event times are round(timestamp) − round(game start), and its game start may come from
// the GAME_STATE combat-log entry instead of m_flGameStartTime (odota/parser Parse.java) — up to 2 s from ours.
const odTimeTol = 2

// Consts are OpenDota's constants the comparison needs to translate names: ability ids (ability_upgrades_arr)
// and hero names (damage_targets). Either may be empty — the checks that need it are skipped then.
type Consts struct {
	Abilities map[int]string // ability id → internal name
	Heroes    map[string]int // npc_dota_hero_* → hero id
}

// Compare checks every field the parser and OpenDota both have for one match.
func Compare(ours *Match, od *ODMatch, c Consts) []Check {
	var out []Check
	mid := od.MatchID
	add := func(field, group string, hero int, o, t interface{}, ok bool, note string) {
		out = append(out, Check{Field: field, Group: group, MatchID: mid, HeroID: hero, Ours: fmt.Sprint(o), Theirs: fmt.Sprint(t), OK: ok, Note: note})
	}
	eq := func(field, group string, hero int, o, t int) {
		note := ""
		if o != t {
			note = fmt.Sprintf("Δ %+d", o-t)
		}
		add(field, group, hero, o, t, o == t, note)
	}
	near := func(field, group string, hero int, o, t, absTol int, relTol float64) {
		d := abs(o - t)
		note := ""
		if d != 0 {
			note = fmt.Sprintf("Δ %+d", o-t)
		}
		ok := float64(d) <= math.Max(float64(absTol), relTol*float64(t))
		add(field, group, hero, o, t, ok, note)
	}
	parsed := od.Version != nil

	// ---- match: Valve
	add("match.radiant_win", GroupValve, 0, ours.DidRadiantWin, od.RadiantWin, ours.DidRadiantWin == od.RadiantWin, "")
	eq("match.duration", GroupValve, 0, ours.DurationSeconds, od.Duration)
	eq("match.game_mode", GroupValve, 0, ours.GameMode, od.GameMode)
	eq("match.lobby_type", GroupValve, 0, ours.LobbyType, od.LobbyType)
	if od.FirstBloodTime != nil && *od.FirstBloodTime > 0 { // 0: OpenDota has none
		fb := -1
		for _, p := range ours.Players {
			if p.Stats == nil {
				continue
			}
			for _, k := range p.Stats.KillEvents {
				if t := fl(k.Time); !k.Reincarnated && (fb < 0 || t < fb) {
					fb = t
				}
			}
		}
		add("match.first_blood_time", GroupValve, 0, fb, *od.FirstBloodTime, fb >= 0 && abs(fb-*od.FirstBloodTime) <= 1, "")
	}

	// ---- match: OpenDota's parse
	if parsed {
		var op, tp []ev
		for _, p := range ours.Pauses {
			op = append(op, ev{T: fl(p.GameTime), V: int(math.Round(p.DurationSec))})
		}
		for _, p := range od.Pauses {
			tp = append(tp, ev{T: p.Time, V: p.Duration})
		}
		evCheck(add, "match.pauses", GroupReplay, 0, op, tp, 1, 2)

		var or, tr []ev
		for _, r := range ours.RoshanKills {
			or = append(or, ev{T: fl(r.Time), K: r.Team})
		}
		var ta, tb []ev
		for _, o := range od.Objectives {
			switch o.Type {
			case "CHAT_MESSAGE_ROSHAN_KILL":
				team := ""
				if o.Team != nil {
					team = map[int]string{2: "radiant", 3: "dire"}[*o.Team]
				}
				tr = append(tr, ev{T: o.Time, K: team})
			case "CHAT_MESSAGE_AEGIS", "CHAT_MESSAGE_AEGIS_STOLEN", "CHAT_MESSAGE_DENIED_AEGIS":
				hero := 0
				if o.Slot != nil && *o.Slot >= 0 && *o.Slot < len(od.Players) {
					hero = od.Players[*o.Slot].HeroID
				} else if o.PlayerSlot != nil {
					for _, p := range od.Players {
						if p.PlayerSlot == *o.PlayerSlot {
							hero = p.HeroID
						}
					}
				}
				ta = append(ta, ev{T: o.Time, K: strconv.Itoa(hero)})
			case "building_kill":
				tb = append(tb, ev{T: o.Time, K: string(o.Key)})
			}
		}
		evCheck(add, "match.roshan_kills", GroupReplay, 0, or, tr, 2, -1)
		var oa []ev
		for _, a := range ours.AegisEvents {
			hero := 0
			if a.PlayerIdx >= 0 && a.PlayerIdx < len(ours.Players) {
				hero = ours.Players[a.PlayerIdx].HeroID
			}
			oa = append(oa, ev{T: fl(a.Time), K: strconv.Itoa(hero)})
		}
		evCheck(add, "match.aegis", GroupReplay, 0, oa, ta, 2, -1)
		var ob []ev
		for _, b := range ours.BuildingKills {
			ob = append(ob, ev{T: fl(b.Time), K: b.Building})
		}
		evCheck(add, "match.buildings", GroupReplay, 0, ob, tb, 2, -1)
		eq("match.teamfights", GroupHeuristic, 0, len(ours.Teamfights), len(od.Teamfights))
	}

	// ---- players
	odByHero := map[int]*ODPlayer{}
	for i := range od.Players {
		odByHero[od.Players[i].HeroID] = &od.Players[i]
	}
	firstKiller := -1
	firstKillT := math.MaxInt32
	for i, p := range ours.Players {
		if p.Stats == nil {
			continue
		}
		for _, k := range p.Stats.KillEvents {
			if t := fl(k.Time); !k.Reincarnated && t < firstKillT {
				firstKillT, firstKiller = t, i
			}
		}
	}
	for i := range ours.Players {
		p := &ours.Players[i]
		h := p.HeroID
		o := odByHero[h]
		if o == nil {
			add("player.present", GroupValve, h, h, "missing", false, "hero not in OpenDota's answer")
			continue
		}
		s := p.Stats
		if s == nil {
			s = &Stats{}
		}

		// Valve's scoreboard
		add("player.side", GroupValve, h, p.IsRadiant, o.Radiant(), p.IsRadiant == o.Radiant(), "")
		win := o.Radiant() == od.RadiantWin
		add("player.win", GroupValve, h, p.IsVictory, win, p.IsVictory == win, "")
		if o.AccountID != nil && *o.AccountID > 0 && *o.AccountID != 4294967295 {
			add("player.account_id", GroupValve, h, int64(p.SteamAccountID), *o.AccountID, int64(p.SteamAccountID) == *o.AccountID, "")
		}
		eq("player.kills", GroupValve, h, p.Kills, o.Kills)
		eq("player.deaths", GroupValve, h, p.Deaths, o.Deaths)
		eq("player.assists", GroupValve, h, p.Assists, o.Assists)
		eq("player.last_hits", GroupValve, h, p.NumLastHits, o.LastHits)
		eq("player.denies", GroupValve, h, p.NumDenies, o.Denies)
		// the replay's own Valve counters and Valve's API differ by the end-of-game snapshot: GPM/XPM/net worth
		// by one, hero and tower damage by up to ~1.5 % for ~1 % of players (99 matches, 2026-10-07)
		near("player.gold_per_min", GroupValve, h, p.GoldPerMinute, o.GoldPerMin, 1, 0)
		// Valve keeps counting XP past level 30; the replay caps it (64 400) — OpenDota's own replay data too
		if o.Level < 30 {
			near("player.xp_per_min", GroupValve, h, p.ExperiencePerMinute, o.XPPerMin, 1, 0)
		}
		if o.NetWorth != nil {
			near("player.net_worth", GroupValve, h, p.Networth, *o.NetWorth, 1, 0)
		}
		eq("player.level", GroupValve, h, p.Level, o.Level)
		if o.HeroDamage != nil {
			near("player.hero_damage", GroupValve, h, p.HeroDamage, *o.HeroDamage, 50, 0.02)
		}
		if o.TowerDamage != nil {
			near("player.tower_damage", GroupValve, h, p.TowerDamage, *o.TowerDamage, 50, 0.02)
		}
		if o.HeroHealing != nil {
			eq("player.hero_healing", GroupValve, h, p.HeroHealing, *o.HeroHealing)
		}
		setCheck(add, "player.items", GroupValve, h, []int{p.Item0ID, p.Item1ID, p.Item2ID, p.Item3ID, p.Item4ID, p.Item5ID}, []int{o.Item0, o.Item1, o.Item2, o.Item3, o.Item4, o.Item5})
		setCheck(add, "player.backpack", GroupValve, h, []int{p.Backpack0, p.Backpack1, p.Backpack2}, []int{o.Backpack0, o.Backpack1, o.Backpack2})
		eq("player.neutral_item", GroupValve, h, p.Neutral0ID, o.ItemNeutral)

		// consistency: the parser's events against its own totals
		// a reincarnation (Aegis, Wraith King) is an event but no kill and no death on the scoreboard
		kills, deaths := 0, 0
		for _, k := range s.KillEvents {
			if !k.Reincarnated {
				kills++
			}
		}
		for _, d := range s.DeathEvents {
			if !d.Reincarnated {
				deaths++
			}
		}
		eq("consistency.kill_events", GroupConsistency, h, kills, p.Kills)
		eq("consistency.death_events", GroupConsistency, h, deaths, p.Deaths)
		// information: the combat log lists fewer assists than Valve's scoreboard (cause unknown)
		eq("consistency.assist_events", GroupHeuristic, h, len(s.AssistEvents), p.Assists)
		if n := len(s.LastHitsPerMinute); n > 0 {
			eq("consistency.lh_timeline_end", GroupConsistency, h, s.LastHitsPerMinute[n-1], p.NumLastHits)
		}
		if len(ours.DamageSeconds) > 0 {
			sum := 0
			for _, d := range ours.DamageSeconds {
				if d.A == i && d.V >= 0 && d.V < len(ours.Players) && ours.Players[d.V].IsRadiant != p.IsRadiant {
					sum += d.D
				}
			}
			// information: heroDamage is Valve's counter, which leaves out some combat-log damage (Ember Spirit's
			// Immolation in 9032007166: 25 000; OpenDota's combat-log sum agrees with ours)
			eq("consistency.damage_seconds_total", GroupHeuristic, h, sum, p.HeroDamage)
		}

		if !parsed {
			continue
		}
		// OpenDota's parse of the replay
		// gold: OpenDota's gold_t runs ±1 off m_iTotalEarnedGold in some replays
		timeline(add, "player.gold_t", h, s.GoldPerMinute, o.GoldT, 0, 1)
		timeline(add, "player.xp_t", h, s.ExperiencePerMinute, o.XPT, 0, 0)
		timeline(add, "player.lh_t", h, s.LastHitsPerMinute, o.LHT, 0, 0)
		timeline(add, "player.dn_t", h, s.DeniesPerMinute, o.DNT, 0, 0)
		if len(ours.DamageSeconds) > 0 && len(o.HeroDamageT) > 0 {
			cum := make([]int, len(o.HeroDamageT))
			for _, d := range ours.DamageSeconds {
				if d.A != i || d.V < 0 || d.V >= len(ours.Players) || ours.Players[d.V].IsRadiant == p.IsRadiant {
					continue
				}
				for m := range cum {
					if d.T < m*60 {
						cum[m] += d.D
					}
				}
			}
			// OpenDota builds this one from the combat log with rounded times, so its minute may run a second
			// either way: ours must lie between its minutes m-1 and m+1
			timeline(add, "player.hero_damage_t", h, cum, o.HeroDamageT, 1, 0)
		}

		var ok, tk []ev
		for _, k := range s.KillEvents {
			if !k.Reincarnated {
				ok = append(ok, ev{T: fl(k.Time), K: k.TargetName})
			}
		}
		for _, k := range o.KillsLog {
			// the combat log flags Lone Druid's Spirit Bear as a hero and OpenDota logs its deaths; Valve's
			// scoreboard kills, which our kill events match, do not count them
			if strings.HasPrefix(string(k.Key), "npc_dota_lone_druid_bear") {
				continue
			}
			tk = append(tk, ev{T: k.Time, K: string(k.Key)})
		}
		evCheck(add, "player.kills_log", GroupReplay, h, ok, tk, odTimeTol, -1)

		// Starting items have no real time on either side (each takes the inventory at its first sighting of the
		// hero: we at about -73 s, OpenDota at about -81 s), so they compare as one set; recipes are purchases
		// OpenDota's purchase_log leaves out.
		var opu, tpu []ev
		for _, b := range s.ItemPurchases {
			if strings.HasPrefix(b.ItemName, "item_recipe_") {
				continue
			}
			opu = append(opu, ev{T: preHorn(fl(b.Time)), K: strings.TrimPrefix(b.ItemName, "item_")})
		}
		for _, b := range o.PurchaseLog {
			tpu = append(tpu, ev{T: preHorn(b.Time), K: string(b.Key)})
		}
		evCheck(add, "player.purchase_log", GroupReplay, h, opu, tpu, odTimeTol, -1)

		// Valve's m_iRunePickups (OpenDota's rune_pickups) leaves wisdom runes (type 8) out
		var oru, tru []ev
		pickups := 0
		for _, r := range s.Runes {
			if r.Action != 1 {
				continue
			}
			if r.Rune != 8 {
				pickups++
			}
			oru = append(oru, ev{T: fl(r.Time), K: strconv.Itoa(r.Rune)})
		}
		for _, r := range o.RunesLog {
			tru = append(tru, ev{T: r.Time, K: string(r.Key)})
		}
		evCheck(add, "player.runes_log", GroupReplay, h, oru, tru, odTimeTol, -1)
		if o.RunePickups != nil {
			eq("player.rune_pickups", GroupReplay, h, pickups, *o.RunePickups)
		}

		var obb, tbb []ev
		for _, b := range ours.Buybacks {
			if b.PlayerID == i {
				obb = append(obb, ev{T: fl(b.Time)})
			}
		}
		for _, b := range o.BuybackLog {
			tbb = append(tbb, ev{T: b.Time})
		}
		evCheck(add, "player.buyback_log", GroupReplay, h, obb, tbb, odTimeTol, -1)

		var oobs, osen, tobs, tsen []ev
		for _, w := range s.Wards {
			if w.Type == 0 {
				oobs = append(oobs, ev{T: fl(w.Time)})
			} else {
				osen = append(osen, ev{T: fl(w.Time)})
			}
		}
		for _, w := range o.ObsLog {
			tobs = append(tobs, ev{T: w.Time})
		}
		for _, w := range o.SenLog {
			tsen = append(tsen, ev{T: w.Time})
		}
		evCheck(add, "player.obs_log", GroupReplay, h, oobs, tobs, odTimeTol, -1)
		evCheck(add, "player.sen_log", GroupReplay, h, osen, tsen, odTimeTol, -1)

		if o.AbilityUses != nil {
			m := map[string]int{}
			for _, a := range s.AbilityCastReport {
				if !strings.HasPrefix(a.AbilityName, "item_") {
					m[a.AbilityName] += a.Count
				}
			}
			mapCheck(add, "player.ability_uses", GroupReplay, h, m, o.AbilityUses)
		}
		if o.ItemUses != nil {
			m := map[string]int{}
			for _, u := range s.ItemUsed {
				m[strings.TrimPrefix(u.ItemName, "item_")] += u.Count
			}
			mapCheck(add, "player.item_uses", GroupReplay, h, m, o.ItemUses)
			eq("player.tp_uses", GroupReplay, h, s.TPCount, o.ItemUses["tpscroll"])
		}
		if len(c.Abilities) > 0 && o.AbilityUpgradesArr != nil {
			var on, tn []string
			for _, sk := range s.SkillBuild {
				on = append(on, normAbility(sk.AbilityName))
			}
			for _, id := range o.AbilityUpgradesArr {
				name, okName := c.Abilities[id]
				if !okName {
					name = "id" + strconv.Itoa(id)
				}
				tn = append(tn, normAbility(name))
			}
			// Valve's ability_upgrades stop at 25 entries (a level-27 Magnus: 25)
			if len(on) > 25 {
				on = on[:25]
			}
			if len(tn) > 25 {
				tn = tn[:25]
			}
			same := strings.Join(on, ",") == strings.Join(tn, ",")
			note := ""
			if !same {
				note = firstDiff(on, tn)
			}
			add("player.ability_upgrades", GroupReplay, h, len(on), len(tn), same, note)
		}
		if o.CampsStacked != nil {
			eq("player.camps_stacked", GroupReplay, h, s.CampsStacked, *o.CampsStacked)
		}
		if o.CreepsStacked != nil {
			eq("player.creeps_stacked", GroupReplay, h, s.CreepsStacked, *o.CreepsStacked)
		}
		if o.LaneKills != nil {
			// OpenDota's lane_kills match "creep_goodguys/badguys" and so leave catapults out; ours count them
			siege := 0
			for k, v := range o.Killed {
				if strings.Contains(k, "_siege") {
					siege += v
				}
			}
			eq("player.lane_kills", GroupReplay, h, s.CreepKills.TotalLaneCreeps, *o.LaneKills+siege)
		}
		if o.NeutralKills != nil {
			eq("player.neutral_kills", GroupReplay, h, s.CreepKills.TotalJungleCreeps, *o.NeutralKills)
		}
		if o.ObserverKills != nil && o.SentryKills != nil {
			eq("player.dewards", GroupReplay, h, s.VisionStats.WardsDewarded, *o.ObserverKills+*o.SentryKills)
		}
		if o.RoshanKills != nil {
			n := 0
			for _, r := range ours.RoshanKills {
				if r.Killer == i {
					n++
				}
			}
			eq("player.roshan_kills", GroupReplay, h, n, *o.RoshanKills)
		}
		if o.FirstbloodClaimed != nil {
			fb := 0
			if firstKiller == i {
				fb = 1
			}
			eq("player.firstblood_claimed", GroupReplay, h, fb, *o.FirstbloodClaimed)
		}
		if o.LifeStateDead != nil {
			d := int(math.Round(s.TotalDeadTimeSec))
			add("player.time_dead", GroupReplay, h, d, *o.LifeStateDead, abs(d-*o.LifeStateDead) <= 2, fmt.Sprintf("Δ %+d s", d-*o.LifeStateDead))
		}
		if o.Stuns != nil {
			// both Valve's m_fStuns: equal to the rounding of the output
			a, b := s.StunDurationDealt, *o.Stuns
			add("player.stuns", GroupReplay, h, round2(a), round2(b), math.Abs(a-b) <= 0.011, "")
		}
		if len(c.Heroes) > 0 && o.DamageTargets != nil {
			// enemy heroes only: OpenDota also lists damage to self and allies
			enemy := map[int]bool{}
			for _, q := range ours.Players {
				if q.IsRadiant != p.IsRadiant {
					enemy[q.HeroID] = true
				}
			}
			theirs := map[string]int{}
			for _, targets := range o.DamageTargets {
				for name, d := range targets {
					if id, okID := c.Heroes[name]; okID && enemy[id] {
						theirs[strconv.Itoa(id)] += d
					}
				}
			}
			mine := map[string]int{}
			for _, d := range s.HeroDamageReport {
				if d.Target >= 0 && d.Target < len(ours.Players) {
					mine[strconv.Itoa(ours.Players[d.Target].HeroID)] += d.PhysicalDamage + d.MagicalDamage + d.PureDamage
				}
			}
			mapCheck(add, "player.damage_by_target", GroupReplay, h, mine, theirs)
		}
		if len(c.Heroes) > 0 && o.DamageTaken != nil && s.DamageReceivedReport != nil {
			theirs := 0
			for name, d := range o.DamageTaken {
				if _, okID := c.Heroes[name]; okID {
					theirs += d
				}
			}
			r := s.DamageReceivedReport
			mine := r.PhysicalDamage + r.MagicalDamage + r.PureDamage
			// information: OpenDota keys damage taken by the attacking unit (summons apart), ours by hero
			eq("player.damage_taken_from_heroes", GroupHeuristic, h, mine, theirs)
		}
		if o.LaneRole != nil {
			eq("player.lane_role", GroupHeuristic, h, p.Lane, *o.LaneRole)
		}
	}
	return out
}

// ---- helpers ------------------------------------------------------------------------------------

type ev struct {
	T int    // game second, floored
	K string // what happened (item, victim, rune type…); "" when only the time matters
	V int    // a value compared with its own tolerance (a pause's duration); -1 tolerance skips it
}

type addFn func(field, group string, hero int, o, t interface{}, ok bool, note string)

// evCheck pairs two event lists by key and time (±tol s; values ±vtol when vtol ≥ 0): one row, OK when every
// event has a partner. The note names the first unpaired ones on each side.
func evCheck(add addFn, field, group string, hero int, ours, theirs []ev, tol, vtol int) {
	if len(ours) == 0 && len(theirs) == 0 {
		return
	}
	used := make([]bool, len(ours))
	var missing []ev
	for _, t := range theirs {
		best, bestD := -1, tol+1
		for i, o := range ours {
			if used[i] || o.K != t.K {
				continue
			}
			if vtol >= 0 && abs(o.V-t.V) > vtol {
				continue
			}
			if d := abs(o.T - t.T); d < bestD {
				best, bestD = i, d
			}
		}
		if best < 0 {
			missing = append(missing, t)
			continue
		}
		used[best] = true
	}
	var extra []ev
	for i, o := range ours {
		if !used[i] {
			extra = append(extra, o)
		}
	}
	note := ""
	if len(missing) > 0 || len(extra) > 0 {
		note = fmt.Sprintf("missing %d %s; extra %d %s", len(missing), evList(missing), len(extra), evList(extra))
	}
	add(field, group, hero, len(ours), len(theirs), len(missing) == 0 && len(extra) == 0, note)
}

func evList(es []ev) string {
	var parts []string
	for i, e := range es {
		if i == 4 {
			parts = append(parts, "…")
			break
		}
		s := fmt.Sprintf("%d", e.T)
		if e.K != "" {
			s = e.K + "@" + s
		}
		parts = append(parts, s)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// setCheck compares two item slot lists as multisets (zeros are empty slots).
func setCheck(add addFn, field, group string, hero int, ours, theirs []int) {
	a, b := nonZeroSorted(ours), nonZeroSorted(theirs)
	add(field, group, hero, a, b, fmt.Sprint(a) == fmt.Sprint(b), "")
}

func nonZeroSorted(xs []int) []int {
	out := []int{}
	for _, x := range xs {
		if x != 0 {
			out = append(out, x)
		}
	}
	sort.Ints(out)
	return out
}

// mapCheck compares two name → count maps: one row, OK when every key has the same count.
func mapCheck(add addFn, field, group string, hero int, ours, theirs map[string]int) {
	keys := map[string]bool{}
	for k := range ours {
		keys[k] = true
	}
	for k := range theirs {
		keys[k] = true
	}
	var diffs []string
	for k := range keys {
		if ours[k] != theirs[k] {
			diffs = append(diffs, fmt.Sprintf("%s %d vs %d", k, ours[k], theirs[k]))
		}
	}
	sort.Strings(diffs)
	note := ""
	if len(diffs) > 0 {
		if len(diffs) > 5 {
			diffs = append(diffs[:5], "…")
		}
		note = strings.Join(diffs, "; ")
	}
	add(field, group, hero, len(ours), len(theirs), len(diffs) == 0, note)
}

func firstDiff(a, b []string) string {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return fmt.Sprintf("first difference at #%d: %q vs %q", i+1, x, y)
		}
	}
	return ""
}

// normAbility makes the parser's entity-style names (drowranger_frostarrows) and OpenDota's
// (drow_ranger_frost_arrows) comparable.
func normAbility(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "")) }

func fl(t float64) int { return int(math.Floor(t)) }

// preHorn folds every time before the horn into one: starting items compare by set.
func preHorn(t int) int {
	if t < 0 {
		return -1
	}
	return t
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

// timeline checks a cumulative per-minute counter (gold earned, XP, last hits, denies). The parser's minute m is
// the value at m:00 exactly; OpenDota's comes up to a second earlier — its parser rounds both the game time and
// the start time (odota/parser Parse.java: Math.round) and samples once the rounded time reaches the minute. So
// ours must lie between OpenDota's minute m and minute m+1 (the counter only grows); the last minute is the
// game's end on our side and is checked against Valve's totals elsewhere.
func timeline(add addFn, field string, hero int, ours, theirs []int, back, slack int) {
	if len(ours) == 0 && len(theirs) == 0 {
		return
	}
	n := len(ours)
	if len(theirs) < n {
		n = len(theirs)
	}
	bad, first := 0, -1
	for m := 0; m+1 < n; m++ {
		lo := theirs[m]
		if back > 0 {
			// before its first minute a cumulative series is 0
			lo = 0
			if m-back >= 0 {
				lo = theirs[m-back]
			}
		}
		if ours[m] < lo-slack || ours[m] > theirs[m+1]+slack {
			bad++
			if first < 0 {
				first = m
			}
		}
	}
	note := ""
	if bad > 0 {
		note = fmt.Sprintf("%d of %d minutes outside OpenDota's [m, m+1], first at %d: %d vs %d..%d", bad, n-1, first, ours[first], theirs[first], theirs[first+1])
	}
	if abs(len(ours)-len(theirs)) > 1 {
		note = strings.TrimPrefix(note+fmt.Sprintf("; length %d vs %d", len(ours), len(theirs)), "; ")
	}
	add(field, GroupReplay, hero, fmt.Sprintf("%d min", len(ours)), fmt.Sprintf("%d min", len(theirs)), bad == 0 && abs(len(ours)-len(theirs)) <= 1, note)
}

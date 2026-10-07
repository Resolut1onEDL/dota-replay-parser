// Package audit compares the parser's JSON with the public statistics of the same match: Valve's own numbers
// (scoreboard, items, score — OpenDota passes them through from the match details) and OpenDota's parse of the
// same replay (timelines, logs, per-ability counts). It never imports the parser: it reads its JSON output, so
// the same comparison serves `go test`, the 100-match audit and the weekly check.
package audit

import (
	"encoding/json"
	"strconv"
)

// ---- OpenDota /matches/{id} -------------------------------------------------------------------

type ODMatch struct {
	MatchID        int64         `json:"match_id"`
	RadiantWin     bool          `json:"radiant_win"`
	Duration       int           `json:"duration"`
	GameMode       int           `json:"game_mode"`
	LobbyType      int           `json:"lobby_type"`
	RadiantScore   int           `json:"radiant_score"`
	DireScore      int           `json:"dire_score"`
	FirstBloodTime *int          `json:"first_blood_time"`
	Version        *int          `json:"version"` // null: OpenDota has not parsed the replay — only Valve's numbers
	Patch          int           `json:"patch"`
	StartTime      int64         `json:"start_time"`
	Cluster        int           `json:"cluster"`
	ReplaySalt     int64         `json:"replay_salt"`
	ReplayURL      string        `json:"replay_url"`
	LeagueID       int           `json:"leagueid"`
	Objectives     []ODObjective `json:"objectives"`
	Pauses         []struct {
		Time     int `json:"time"`
		Duration int `json:"duration"`
	} `json:"pauses"`
	RadiantGoldAdv []int             `json:"radiant_gold_adv"`
	RadiantXPAdv   []int             `json:"radiant_xp_adv"`
	Teamfights     []json.RawMessage `json:"teamfights"`
	Players        []ODPlayer        `json:"players"`
}

type ODObjective struct {
	Time       int     `json:"time"`
	Type       string  `json:"type"`
	Key        ODKey   `json:"key"`
	Slot       *int    `json:"slot"`
	PlayerSlot *int    `json:"player_slot"`
	Team       *int    `json:"team"`
	Unit       *string `json:"unit"`
}

// ODKey is OpenDota's "key": a string in most logs, a number in some objectives.
type ODKey string

func (k *ODKey) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*k = ODKey(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		*k = ""
		return nil
	}
	*k = ODKey(n.String())
	return nil
}

type ODLog struct {
	Time int   `json:"time"`
	Key  ODKey `json:"key"`
}

type ODWard struct {
	Time       int     `json:"time"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Ehandle    int64   `json:"ehandle"`
	PlayerSlot int     `json:"player_slot"`
}

type ODPlayer struct {
	PlayerSlot   int    `json:"player_slot"`
	HeroID       int    `json:"hero_id"`
	AccountID    *int64 `json:"account_id"`
	Kills        int    `json:"kills"`
	Deaths       int    `json:"deaths"`
	Assists      int    `json:"assists"`
	LastHits     int    `json:"last_hits"`
	Denies       int    `json:"denies"`
	GoldPerMin   int    `json:"gold_per_min"`
	XPPerMin     int    `json:"xp_per_min"`
	NetWorth     *int   `json:"net_worth"`
	Level        int    `json:"level"`
	HeroDamage   *int   `json:"hero_damage"`
	TowerDamage  *int   `json:"tower_damage"`
	HeroHealing  *int   `json:"hero_healing"`
	Item0        int    `json:"item_0"`
	Item1        int    `json:"item_1"`
	Item2        int    `json:"item_2"`
	Item3        int    `json:"item_3"`
	Item4        int    `json:"item_4"`
	Item5        int    `json:"item_5"`
	Backpack0    int    `json:"backpack_0"`
	Backpack1    int    `json:"backpack_1"`
	Backpack2    int    `json:"backpack_2"`
	ItemNeutral  int    `json:"item_neutral"`
	ItemNeutral2 int    `json:"item_neutral2"`

	GoldT              []int                     `json:"gold_t"`
	XPT                []int                     `json:"xp_t"`
	LHT                []int                     `json:"lh_t"`
	DNT                []int                     `json:"dn_t"`
	HeroDamageT        []int                     `json:"hero_damage_t"`
	KillsLog           []ODLog                   `json:"kills_log"`
	PurchaseLog        []ODLog                   `json:"purchase_log"`
	RunesLog           []ODLog                   `json:"runes_log"`
	BuybackLog         []ODLog                   `json:"buyback_log"`
	ObsLog             []ODWard                  `json:"obs_log"`
	SenLog             []ODWard                  `json:"sen_log"`
	AbilityUses        map[string]int            `json:"ability_uses"`
	ItemUses           map[string]int            `json:"item_uses"`
	AbilityUpgradesArr []int                     `json:"ability_upgrades_arr"`
	DamageTargets      map[string]map[string]int `json:"damage_targets"`
	DamageTaken        map[string]int            `json:"damage_taken"`

	CampsStacked      *int     `json:"camps_stacked"`
	CreepsStacked     *int     `json:"creeps_stacked"`
	LaneKills         *int     `json:"lane_kills"`
	NeutralKills      *int     `json:"neutral_kills"`
	RunePickups       *int     `json:"rune_pickups"`
	ObserverKills     *int     `json:"observer_kills"`
	SentryKills       *int     `json:"sentry_kills"`
	RoshanKills       *int     `json:"roshan_kills"`
	LifeStateDead     *int     `json:"life_state_dead"`
	Stuns             *float64 `json:"stuns"`
	FirstbloodClaimed *int     `json:"firstblood_claimed"`
	Lane              *int     `json:"lane"`
	LaneRole          *int     `json:"lane_role"`
}

func (p ODPlayer) Radiant() bool { return p.PlayerSlot < 128 }

// ---- the parser's JSON (only what the audit reads) ---------------------------------------------

type Match struct {
	ID              int64 `json:"id"`
	GameMode        int   `json:"gameMode"`
	LobbyType       int   `json:"lobbyType"`
	DidRadiantWin   bool  `json:"didRadiantWin"`
	DurationSeconds int   `json:"durationSeconds"`
	Pauses          []struct {
		GameTime    float64 `json:"gameTime"`
		DurationSec float64 `json:"durationSec"`
	} `json:"pauses"`
	RadiantNetworthLeads   []int `json:"radiantNetworthLeads"`
	RadiantExperienceLeads []int `json:"radiantExperienceLeads"`
	RoshanKills            []struct {
		Time   float64 `json:"time"`
		Killer int     `json:"killer"`
		Team   string  `json:"team"`
	} `json:"roshanKills"`
	AegisEvents []struct {
		Time      float64 `json:"time"`
		PlayerIdx int     `json:"playerIdx"`
		Event     string  `json:"event"`
	} `json:"aegisEvents"`
	Buybacks []struct {
		Time     float64 `json:"time"`
		PlayerID int     `json:"playerId"`
	} `json:"buybacks"`
	BuildingKills []struct {
		Time     float64 `json:"time"`
		Building string  `json:"building"`
	} `json:"buildingKills"`
	Teamfights    []json.RawMessage `json:"teamfights"`
	DamageSeconds []struct {
		T int `json:"t"`
		A int `json:"a"`
		V int `json:"v"`
		D int `json:"d"`
	} `json:"damageSeconds"`
	Players       []Player `json:"players"`
	ParserVersion string   `json:"parserVersion"`
}

type Player struct {
	SteamAccountID      SteamID `json:"steamAccountId"`
	HeroID              int     `json:"heroId"`
	IsRadiant           bool    `json:"isRadiant"`
	IsVictory           bool    `json:"isVictory"`
	Kills               int     `json:"kills"`
	Deaths              int     `json:"deaths"`
	Assists             int     `json:"assists"`
	Networth            int     `json:"networth"`
	GoldPerMinute       int     `json:"goldPerMinute"`
	ExperiencePerMinute int     `json:"experiencePerMinute"`
	NumLastHits         int     `json:"numLastHits"`
	NumDenies           int     `json:"numDenies"`
	Level               int     `json:"level"`
	HeroDamage          int     `json:"heroDamage"`
	TowerDamage         int     `json:"towerDamage"`
	HeroHealing         int     `json:"heroHealing"`
	Lane                int     `json:"lane"`
	Role                int     `json:"role"`
	Item0ID             int     `json:"item0Id"`
	Item1ID             int     `json:"item1Id"`
	Item2ID             int     `json:"item2Id"`
	Item3ID             int     `json:"item3Id"`
	Item4ID             int     `json:"item4Id"`
	Item5ID             int     `json:"item5Id"`
	Backpack0           int     `json:"backpack0Id"`
	Backpack1           int     `json:"backpack1Id"`
	Backpack2           int     `json:"backpack2Id"`
	Neutral0ID          int     `json:"neutral0Id"`
	Stats               *Stats  `json:"stats"`
}

type Stats struct {
	GoldPerMinute       []int `json:"goldPerMinute"`
	ExperiencePerMinute []int `json:"experiencePerMinute"`
	LastHitsPerMinute   []int `json:"lastHitsPerMinute"`
	DeniesPerMinute     []int `json:"deniesPerMinute"`
	KillEvents          []struct {
		Time         float64 `json:"time"`
		Target       int     `json:"target"`
		TargetName   string  `json:"targetName"`
		Reincarnated bool    `json:"reincarnated"`
	} `json:"killEvents"`
	DeathEvents []struct {
		Time         float64 `json:"time"`
		Reincarnated bool    `json:"reincarnated"`
	} `json:"deathEvents"`
	AssistEvents []struct {
		Time float64 `json:"time"`
	} `json:"assistEvents"`
	Runes []struct {
		Time   float64 `json:"time"`
		Rune   int     `json:"rune"`
		Action int     `json:"action"`
	} `json:"runes"`
	Wards []struct {
		Time      float64 `json:"time"`
		Type      int     `json:"type"`
		PositionX float64 `json:"positionX"`
		PositionY float64 `json:"positionY"`
	} `json:"wards"`
	ItemPurchases []struct {
		Time     float64 `json:"time"`
		ItemName string  `json:"itemName"`
	} `json:"itemPurchases"`
	AbilityCastReport []struct {
		AbilityName string `json:"abilityName"`
		Count       int    `json:"count"`
	} `json:"abilityCastReport"`
	ItemUsed []struct {
		ItemName string `json:"itemName"`
		Count    int    `json:"count"`
	} `json:"itemUsed"`
	HeroDamageReport []struct {
		Target         int `json:"target"`
		PhysicalDamage int `json:"physicalDamage"`
		MagicalDamage  int `json:"magicalDamage"`
		PureDamage     int `json:"pureDamage"`
	} `json:"heroDamageReport"`
	DamageReceivedReport *struct {
		PhysicalDamage int `json:"physicalDamage"`
		MagicalDamage  int `json:"magicalDamage"`
		PureDamage     int `json:"pureDamage"`
	} `json:"damageReceivedReport"`
	StunDurationDealt float64 `json:"stunDurationDealt"`
	SkillBuild        []struct {
		Time        float64 `json:"time"`
		AbilityName string  `json:"abilityName"`
	} `json:"skillBuild"`
	CampsStacked  int `json:"campsStacked"`
	CreepsStacked int `json:"creepsStacked"`
	VisionStats   struct {
		WardsDewarded int `json:"wardsDewarded"`
	} `json:"visionStats"`
	TPCount    int `json:"tpCount"`
	CreepKills struct {
		TotalLaneCreeps   int `json:"totalLaneCreeps"`
		TotalJungleCreeps int `json:"totalJungleCreeps"`
	} `json:"creepKills"`
	TotalDeadTimeSec float64 `json:"totalDeadTimeSec"`
}

// SteamID reads the parser's steamAccountId (a 64-bit Steam id, or a 32-bit account id from older parses) as
// the 32-bit account id OpenDota uses, without passing it through a float.
type SteamID int64

const steam64Base = 76561197960265728

func (s *SteamID) UnmarshalJSON(b []byte) error {
	str := string(b)
	if len(str) > 1 && str[0] == '"' {
		str = str[1 : len(str)-1]
	}
	if str == "" || str == "null" {
		*s = 0
		return nil
	}
	v, err := strconv.ParseInt(str, 10, 64)
	if err != nil {
		*s = 0
		return nil
	}
	if v >= steam64Base {
		v -= steam64Base
	}
	*s = SteamID(v)
	return nil
}

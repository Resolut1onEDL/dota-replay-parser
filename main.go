package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/dotabuff/manta"
	"github.com/dotabuff/manta/dota"
)

// ============= TYPES (Stratz-compatible + extras) =============

type ItemPurchase struct {
	ItemID   int     `json:"itemId"`
	Time     float64 `json:"time"`
	ItemName string  `json:"itemName,omitempty"`
}

type WardEvent struct {
	Time      float64 `json:"time"`
	Type      int     `json:"type"` // 0 = observer, 1 = sentry
	PositionX float64 `json:"positionX,omitempty"`
	PositionY float64 `json:"positionY,omitempty"`
	PlayerID  int     `json:"playerId,omitempty"`
	// v4.3.0: ward lifecycle from entity create→delete. EndTime/Duration absent
	// (omitempty) when the ward was still alive at game end.
	EndTime  float64 `json:"endTime,omitempty"`
	Duration float64 `json:"durationSeconds,omitempty"`
}

// v4.3.0: in-flight ward, для расчёта длительности на удалении сущности.
type activeWard struct {
	playerIdx int
	sliceIdx  int // индекс в Players[playerIdx].Wards
	start     float64
}

type RuneEvent struct {
	Time     float64 `json:"time"`
	RuneType int     `json:"rune"` // 0=DD, 1=Haste, 2=Illusion, 3=Invis, 4=Regen, 5=Bounty, 6=Arcane, 7=Water, 9=Shield
	Action   int     `json:"action"` // 0 = spawned, 1 = pickup
}

type RoshanEvent struct {
	Time   float64 `json:"time"`
	Killer int     `json:"killer,omitempty"`
	Team   string  `json:"team"` // "radiant" or "dire"
}

// v4.4.1: who picked up the Aegis (no omitempty on PlayerIdx — slot 0 is real).
// Source: chat events, NOT the combat log — AEGIS_TAKEN (type 30) never occurs
// in real demos (verified on multiple replays); Valve announces pickups via
// CHAT_MESSAGE_AEGIS / _AEGIS_STOLEN / _DENIED_AEGIS, which also hands us the
// steal classification for free.
type AegisEvent struct {
	Time      float64 `json:"time"`
	PlayerIdx int     `json:"playerIdx"`
	Event     string  `json:"event"` // "taken" | "stolen" | "denied"
}

type BuildingEvent struct {
	Time         float64 `json:"time"`
	Building     string  `json:"building"`
	IsRadiant    bool    `json:"isRadiant"` // which team's building
	Killer       string  `json:"killer,omitempty"`
	KillerPlayer int     `json:"killerPlayer,omitempty"`
}

type BuybackEvent struct {
	Time     float64 `json:"time"`
	PlayerID int     `json:"playerId"`
	GoldCost int     `json:"goldCost,omitempty"`
}

type TeamfightDeath struct {
	Time      float64 `json:"time"`
	PlayerID  int     `json:"playerId"`
	IsRadiant bool    `json:"isRadiant"`
	Killer    int     `json:"killer"`
}

type Teamfight struct {
	StartTime     float64          `json:"startTime"`
	EndTime       float64          `json:"endTime"`
	Duration      float64          `json:"duration"`
	RadiantDeaths int              `json:"radiantDeaths"`
	DireDeaths    int              `json:"direDeaths"`
	Winner        string           `json:"winner"` // "radiant", "dire", "even"
	Deaths        []TeamfightDeath `json:"deaths"`
}

type KillEvent struct {
	Time       float64 `json:"time"`
	Target     int     `json:"target"`
	TargetName string  `json:"targetName,omitempty"`
	PositionX  float64 `json:"positionX,omitempty"`
	PositionY  float64 `json:"positionY,omitempty"`
}

type DeathEvent struct {
	Time            float64 `json:"time"`
	Killer          int     `json:"killer,omitempty"`
	KillerName      string  `json:"killerName,omitempty"`
	Assists         []int   `json:"assist,omitempty"`
	PositionX       float64 `json:"positionX,omitempty"`
	PositionY       float64 `json:"positionY,omitempty"`
	GoldLost        int     `json:"goldLost,omitempty"`
	TimeDead        int     `json:"timeDead,omitempty"`
	IsSmoke         bool    `json:"isSmoke,omitempty"`
	IsGank          bool    `json:"isGank,omitempty"`
	NetworthAtDeath int     `json:"networthAtDeath,omitempty"`
	HadTP           bool    `json:"hadTP,omitempty"`
	NearbyAllies    []int   `json:"nearbyAllies,omitempty"`
	NearbyEnemies   []int   `json:"nearbyEnemies,omitempty"`
}

type AssistEvent struct {
	Time   float64 `json:"time"`
	Target int     `json:"target"` // player index of the killed hero
}

type DamageReceivedReport struct {
	PhysicalDamage int `json:"physicalDamage"`
	MagicalDamage  int `json:"magicalDamage"`
	PureDamage     int `json:"pureDamage"`
}

type ItemUsed struct {
	ItemName string `json:"itemName"`
	ItemID   int    `json:"itemId,omitempty"`
	Count    int    `json:"count"`
}

type CampStack struct {
	Time       float64 `json:"time"`
	StackCount int     `json:"stackCount"`
}

// Position sampling for zone analysis (every minute)
type PositionSamplePlayer struct {
	Idx int     `json:"idx"`
	X   float64 `json:"x"`
	Y   float64 `json:"y"`
}

type PositionSample struct {
	Minute  int                    `json:"minute"`
	Players []PositionSamplePlayer `json:"players"`
}

// Creep kill breakdown by phase and source
type CreepKillPhases struct {
	LaneCreepsPre10    int `json:"laneCreepsPre10"`
	LaneCreeps10to25   int `json:"laneCreeps10to25"`
	LaneCreeps25Plus   int `json:"laneCreeps25Plus"`
	JungleCreepsPre10  int `json:"jungleCreepsPre10"`
	JungleCreeps10to25 int `json:"jungleCreeps10to25"`
	JungleCreeps25Plus int `json:"jungleCreeps25Plus"`
	TotalLaneCreeps    int `json:"totalLaneCreeps"`
	TotalJungleCreeps  int `json:"totalJungleCreeps"`
}

type AbilityCast struct {
	AbilityID   int    `json:"abilityId"`
	AbilityName string `json:"abilityName,omitempty"`
	Count       int    `json:"count"`
}

// v4.4.0: timestamped cast/usage timelines — the raw data for "moments"
// detection (clutch escapes, teamfight ults, stolen-spell plays). The old
// count-only reports stay for back-compat.
type AbilityCastEvent struct {
	Time       float64 `json:"time"`
	Ability    string  `json:"ability"`
	IsUltimate bool    `json:"isUltimate,omitempty"`
	IsStolen   bool    `json:"isStolen,omitempty"`
	TargetSelf bool    `json:"targetSelf,omitempty"`
	// Short hero name (no npc_dota_hero_ prefix) when unit-targeted on a hero.
	TargetHero string `json:"targetHero,omitempty"`
}

type ItemCastEvent struct {
	Time float64 `json:"time"`
	Item string  `json:"item"`
	// Unit-target info when the item was cast on a hero (Force/Glimmer/Eul…)
	TargetSelf bool   `json:"targetSelf,omitempty"`
	TargetHero string `json:"targetHero,omitempty"`
}

// Emitted when a hero-vs-hero hit leaves the target at ≤20% max HP (illusions
// excluded), at most one event per 5s window. HpPct is AFTER the hit.
type NearDeathEvent struct {
	Time     float64 `json:"time"`
	HpPct    int     `json:"hpPct"`
	Attacker string  `json:"attacker,omitempty"`
}

type SkillLevelUp struct {
	Time        float64 `json:"time"`
	AbilityName string  `json:"abilityName"`
	Level       int     `json:"level"`     // ability level after this point (1,2,3,4)
	HeroLevel   int     `json:"heroLevel"` // hero level when this was skilled
}

// AbilityEntityInfo tracks a single ability entity for skill build detection
type AbilityEntityInfo struct {
	Name  string
	Level int
}

// Talent is a chosen hero talent — a special_bonus_* ability on the hero
// entity with ability level >= 1, snapshotted at the end of the replay.
// Level is the talent tier (10/15/20/25) when derivable, 0 (omitted) otherwise.
type Talent struct {
	Level int    `json:"level,omitempty"`
	Name  string `json:"name"`
}

type DamageTarget struct {
	Target         int `json:"target"`
	PhysicalDamage int `json:"physicalDamage"`
	MagicalDamage  int `json:"magicalDamage"`
	PureDamage     int `json:"pureDamage"`
}

// EXTRA: Vision/stealth metrics
type VisionExposure struct {
	SmokeUsageCount int `json:"smokeUsageCount"` // Times used smoke of deceit
	// v4.3.0: сколько вражеских вардов (obs+sentry) снёс этот игрок (deward).
	// Истечение варда (attacker == target) не считается.
	WardsDewarded int `json:"wardsDewarded"`
}

// SmokeEvent — match-level: группа игроков одной команды одновременно
// (≥4 в окне 1.5s) получила modifier_smoke_of_deceit. Outcome считается за
// 60s после начала smoke: kills/deaths участников, towers сошедшие на этой
// стороне, runes подобранные участниками.
type SmokeEvent struct {
	GameTime     float64      `json:"gameTime"`     // sec since horn
	Participants []int        `json:"participants"` // player indices 0-9
	IsRadiant    bool         `json:"isRadiant"`    // which side smoked
	Outcome      SmokeOutcome `json:"outcome"`
}

type SmokeOutcome struct {
	KillsWithin60s  int `json:"killsWithin60s"`  // kills BY smokers
	DeathsWithin60s int `json:"deathsWithin60s"` // deaths OF smokers
	TowersWithin60s int `json:"towersWithin60s"` // enemy towers down by smokers' side
	RunesWithin60s  int `json:"runesWithin60s"`  // runes picked by smokers
}

// Rune pickup summary per player
type RuneSummary struct {
	Total        int `json:"total"`                  // all rune pickups
	PowerRunes   int `json:"powerRunes"`             // DD, Haste, Invis, Regen, Arcane, Shield, Illusion
	BountyRunes  int `json:"bountyRunes"`            // bounty rune pickups
	WaterRunes   int `json:"waterRunes"`             // water rune pickups
	// Type 8 in current patches — exact identity unverified (wisdom rune
	// was removed in 7.34+). Could be Lotus pickup or something else.
	// Tracked here as opaque "type 8" for parity with OpenDota's `runes` map.
	Rune8        int `json:"rune8,omitempty"`
	// Breakdown by type
	DoubleDamage int `json:"doubleDamage,omitempty"` // type 0
	Haste        int `json:"haste,omitempty"`        // type 1
	Illusion     int `json:"illusion,omitempty"`     // type 2
	Invis        int `json:"invisibility,omitempty"` // type 3
	Regen        int `json:"regeneration,omitempty"` // type 4
	Arcane       int `json:"arcane,omitempty"`       // type 6
	Shield       int `json:"shield,omitempty"`       // type 9
}

// EXTRA: Lane stats
type LaneStats struct {
	Lane              string  `json:"lane"` // "safe", "mid", "off", "jungle"
	LanePartner       int     `json:"lanePartner,omitempty"`
	LaneCreepKills    int     `json:"laneCreepKills"`
	LaneCreepDenies   int     `json:"laneCreepDenies"`
	ReaggroCount      int     `json:"reaggroCount"` // EXTRA: creep aggro manipulation
	DeathsPreTen      int     `json:"deathsPreTen"`
	LaneDamageTakenPre5 int   `json:"laneDamageTakenPre5"` // EXTRA: enemy-hero dmg taken before 5:00
	KillsPreTen       int     `json:"killsPreTen"`
	AssistsPreTen     int     `json:"assistsPreTen"`
	GPMAtTen          int     `json:"gpmAtTen"`
	XPMAtTen          int     `json:"xpmAtTen"`
	NetWorthAtTen     int     `json:"networthAtTen"`
	LevelAtTen        int     `json:"levelAtTen"`
}

type PlayerStats struct {
	// Per-minute arrays (Stratz format)
	GoldPerMinute       []int `json:"goldPerMinute,omitempty"`
	ExperiencePerMinute []int `json:"experiencePerMinute,omitempty"`
	LastHitsPerMinute   []int `json:"lastHitsPerMinute,omitempty"`
	DeniesPerMinute     []int `json:"deniesPerMinute,omitempty"`
	NetworthPerMinute   []int `json:"networthPerMinute,omitempty"`
	Level               []int `json:"level,omitempty"`
	
	// Events
	KillEvents    []KillEvent    `json:"killEvents,omitempty"`
	DeathEvents   []DeathEvent   `json:"deathEvents,omitempty"`
	AssistEvents  []AssistEvent  `json:"assistEvents,omitempty"`
	Runes         []RuneEvent    `json:"runes,omitempty"`
	Wards         []WardEvent    `json:"wards,omitempty"`
	ItemPurchases []ItemPurchase `json:"itemPurchases,omitempty"`

	// Timestamped timelines (v4.4.0)
	AbilityCastEvents []AbilityCastEvent `json:"abilityCastEvents,omitempty"`
	ItemCastEvents    []ItemCastEvent    `json:"itemCastEvents,omitempty"`
	NearDeathEvents   []NearDeathEvent   `json:"nearDeathEvents,omitempty"`

	// Reports
	AbilityCastReport    []AbilityCast        `json:"abilityCastReport,omitempty"`
	HeroDamageReport     []DamageTarget       `json:"heroDamageReport,omitempty"`
	DamageReceivedReport *DamageReceivedReport `json:"damageReceivedReport,omitempty"`
	StunDurationDealt    float64              `json:"stunDurationDealt,omitempty"`
	SkillBuild           []SkillLevelUp       `json:"skillBuild,omitempty"`
	ItemUsed             []ItemUsed           `json:"itemUsed,omitempty"`
	CampStacks           []CampStack          `json:"campStack,omitempty"`
	CampsStacked         int                  `json:"campsStacked"`
	CreepsStacked        int                  `json:"creepsStacked"`

	// EXTRA metrics
	VisionStats VisionExposure `json:"visionStats"`
	LaneStats   LaneStats      `json:"laneStats"`
	RuneStats   RuneSummary    `json:"runeStats"`

	// General stats
	TPCount    int             `json:"tpCount,omitempty"`
	CreepKills CreepKillPhases `json:"creepKills"`
}

type Player struct {
	SteamAccountID      int64        `json:"steamAccountId"`
	HeroID              int          `json:"heroId"`
	HeroName            string       `json:"heroName,omitempty"`
	IsRadiant           bool         `json:"isRadiant"`
	IsVictory           bool         `json:"isVictory"`
	Kills               int          `json:"kills"`
	Deaths              int          `json:"deaths"`
	Assists             int          `json:"assists"`
	Networth            int          `json:"networth"`
	GoldPerMinute       int          `json:"goldPerMinute"`
	ExperiencePerMinute int          `json:"experiencePerMinute"`
	NumLastHits         int          `json:"numLastHits"`
	NumDenies           int          `json:"numDenies"`
	Level               int          `json:"level"`
	HeroDamage          int          `json:"heroDamage"`
	TowerDamage         int          `json:"towerDamage"`
	HeroHealing         int          `json:"heroHealing"`
	Lane                int          `json:"lane"`     // 1=safe, 2=mid, 3=off, 4=jungle
	Role                int          `json:"role"`     // 0=core, 1=support
	Position            int          `json:"position"` // 1-5
	
	// Final items (IDs)
	Item0ID    int `json:"item0Id,omitempty"`
	Item1ID    int `json:"item1Id,omitempty"`
	Item2ID    int `json:"item2Id,omitempty"`
	Item3ID    int `json:"item3Id,omitempty"`
	Item4ID    int `json:"item4Id,omitempty"`
	Item5ID    int `json:"item5Id,omitempty"`
	Backpack0  int `json:"backpack0Id,omitempty"`
	Backpack1  int `json:"backpack1Id,omitempty"`
	Backpack2  int `json:"backpack2Id,omitempty"`
	Neutral0ID int `json:"neutral0Id,omitempty"`
	
	// Final items (names)
	Item0Name    string `json:"item0Name,omitempty"`
	Item1Name    string `json:"item1Name,omitempty"`
	Item2Name    string `json:"item2Name,omitempty"`
	Item3Name    string `json:"item3Name,omitempty"`
	Item4Name    string `json:"item4Name,omitempty"`
	Item5Name    string `json:"item5Name,omitempty"`
	NeutralName  string `json:"neutralName,omitempty"`

	// Talent tree choices (v4.1.0), final state at end of replay.
	Talents []Talent `json:"talents"`

	Stats *PlayerStats `json:"stats,omitempty"`
}

// PauseEvent records a single in-game pause. GameTime/WallStart are captured at
// pause start; DurationSec is the real-time length of the pause. Voice/chat
// transcripts at wall-clock T can be converted to game-clock by subtracting the
// cumulative duration of all pauses with WallStart <= T from (T - HornDateTime).
type PauseEvent struct {
	GameTime    float64 `json:"gameTime"`           // game-clock sec (0 = horn) at pause start
	WallStart   int64   `json:"wallStart"`          // unix sec when pause started
	DurationSec float64 `json:"durationSec"`        // real-time pause length
	PausedBy    int     `json:"pausedBy,omitempty"` // team that initiated (2=radiant, 3=dire)
}

type Match struct {
	ID               int64    `json:"id"`
	GameMode         int      `json:"gameMode"`
	LobbyType        int      `json:"lobbyType"`
	DidRadiantWin    bool     `json:"didRadiantWin"`
	DurationSeconds  int      `json:"durationSeconds"`
	StartDateTime    int64    `json:"startDateTime,omitempty"`
	// HornDateTime is unix seconds at game-clock 0 (horn). Use this — not
	// StartDateTime — to align wall-clock voice/chat timestamps with game events.
	// StartDateTime points to the demo's first packet (pick/strategy phase).
	HornDateTime int64        `json:"hornDateTime,omitempty"`
	Pauses       []PauseEvent `json:"pauses,omitempty"`

	RadiantNetworthLeads   []int `json:"radiantNetworthLeads,omitempty"`
	RadiantExperienceLeads []int `json:"radiantExperienceLeads,omitempty"`

	// Match events
	RoshanKills   []RoshanEvent   `json:"roshanKills,omitempty"`
	AegisEvents   []AegisEvent    `json:"aegisEvents,omitempty"`
	Buybacks      []BuybackEvent  `json:"buybacks,omitempty"`
	RuneSpawns    []RuneEvent     `json:"runeSpawns,omitempty"`
	BuildingKills []BuildingEvent `json:"buildingKills,omitempty"`
	Teamfights      []Teamfight      `json:"teamfights,omitempty"`
	PositionSamples []PositionSample `json:"positionSamples,omitempty"`
	SmokeEvents     []SmokeEvent     `json:"smokeEvents,omitempty"` // v4: group smoke detection

	Players []Player `json:"players"`
	
	ParsedFromReplay bool   `json:"parsedFromReplay"`
	ParserVersion    string `json:"parserVersion"`
}

// ============= HERO NAMES =============

var heroNames = map[int32]string{
	1: "Anti-Mage", 2: "Axe", 3: "Bane", 4: "Bloodseeker", 5: "Crystal Maiden",
	6: "Drow Ranger", 7: "Earthshaker", 8: "Juggernaut", 9: "Mirana", 10: "Morphling",
	11: "Shadow Fiend", 12: "Phantom Lancer", 13: "Puck", 14: "Pudge", 15: "Razor",
	16: "Sand King", 17: "Storm Spirit", 18: "Sven", 19: "Tiny", 20: "Vengeful Spirit",
	21: "Windranger", 22: "Zeus", 23: "Kunkka", 25: "Lina", 26: "Lion",
	27: "Shadow Shaman", 28: "Slardar", 29: "Tidehunter", 30: "Witch Doctor",
	31: "Lich", 32: "Riki", 33: "Enigma", 34: "Tinker", 35: "Sniper",
	36: "Necrophos", 37: "Warlock", 38: "Beastmaster", 39: "Queen of Pain", 40: "Venomancer",
	41: "Faceless Void", 42: "Wraith King", 43: "Death Prophet", 44: "Phantom Assassin",
	45: "Pugna", 46: "Templar Assassin", 47: "Viper", 48: "Luna", 49: "Dragon Knight",
	50: "Dazzle", 51: "Clockwerk", 52: "Leshrac", 53: "Nature's Prophet", 54: "Lifestealer",
	55: "Dark Seer", 56: "Clinkz", 57: "Omniknight", 58: "Enchantress", 59: "Huskar",
	60: "Night Stalker", 61: "Broodmother", 62: "Bounty Hunter", 63: "Weaver", 64: "Jakiro",
	65: "Batrider", 66: "Chen", 67: "Spectre", 68: "Ancient Apparition", 69: "Doom",
	70: "Ursa", 71: "Spirit Breaker", 72: "Gyrocopter", 73: "Alchemist", 74: "Invoker",
	75: "Silencer", 76: "Outworld Destroyer", 77: "Lycan", 78: "Brewmaster", 79: "Shadow Demon",
	80: "Lone Druid", 81: "Chaos Knight", 82: "Meepo", 83: "Treant Protector", 84: "Ogre Magi",
	85: "Undying", 86: "Rubick", 87: "Disruptor", 88: "Nyx Assassin", 89: "Naga Siren",
	90: "Keeper of the Light", 91: "Io", 92: "Visage", 93: "Slark", 94: "Medusa",
	95: "Troll Warlord", 96: "Centaur Warrunner", 97: "Magnus", 98: "Timbersaw",
	99: "Bristleback", 100: "Tusk", 101: "Skywrath Mage", 102: "Abaddon", 103: "Elder Titan",
	104: "Legion Commander", 105: "Techies", 106: "Ember Spirit", 107: "Earth Spirit",
	108: "Underlord", 109: "Terrorblade", 110: "Phoenix", 111: "Oracle", 112: "Winter Wyvern",
	113: "Arc Warden", 114: "Monkey King", 119: "Dark Willow", 120: "Pangolier",
	121: "Grimstroke", 123: "Hoodwink", 126: "Void Spirit", 128: "Snapfire", 129: "Mars",
	135: "Dawnbreaker", 136: "Marci", 137: "Primal Beast", 138: "Muerta", 131: "Ringmaster",
	145: "Kez", 155: "Largo",
}

func getHeroName(heroID int) string {
	if name, ok := heroNames[int32(heroID)]; ok {
		return name
	}
	return fmt.Sprintf("Hero_%d", heroID)
}

// decodeSelectedHeroID decodes m_vecPlayerTeamData.*.m_nSelectedHeroID.
// Older replays expose it as int32 carrying the hero ID directly. Newer
// replays expose it as uint32 with the ID doubled (shifted left by one bit) —
// the same encoding artifact m_iPlayerID has (see replayPlayerToIndex).
// Verified against match 8824123966: all ten uint32 slot values were exactly
// 2x the Stratz ground-truth hero IDs (82→41 Faceless Void, 310→155 Largo).
// Returns ok=false when the property is absent (manta Get returns nil).
func decodeSelectedHeroID(raw interface{}) (int, bool) {
	switch v := raw.(type) {
	case int32:
		return int(v), true
	case uint32:
		return int(v >> 1), true
	}
	return 0, false
}

// Dota 2 cumulative XP thresholds per hero level (index = level, value = total XP needed)
// Used as fallback when entity XP is not available
var xpForLevel = []int{
	0,     // level 0 (unused)
	0,     // level 1
	230,   // level 2
	600,   // level 3
	1080,  // level 4
	1660,  // level 5
	2260,  // level 6
	2980,  // level 7
	3820,  // level 8
	4780,  // level 9
	5860,  // level 10
	7060,  // level 11
	8380,  // level 12
	9820,  // level 13
	11380, // level 14
	13060, // level 15
	14860, // level 16
	16780, // level 17
	18820, // level 18
	20980, // level 19
	23260, // level 20
	25660, // level 21
	28180, // level 22
	30820, // level 23
	33580, // level 24
	36460, // level 25
	39460, // level 26
	42560, // level 27
	45760, // level 28
	49060, // level 29
	52460, // level 30
}

// ============= ITEM MAPPINGS =============

// entityClassToItemName maps CDOTA_Item_* class name (with prefix stripped) to standard item_ name
var entityClassToItemName = map[string]string{
	// Weapons & Damage
	"AbyssalBlade":      "item_abyssal_blade",
	"Battlefury":        "item_bfury",
	"BlinkDagger":       "item_blink",
	"Butterfly":         "item_butterfly",
	"CraniumBasher":     "item_basher",
	"Desolator":         "item_desolator",
	"Diffusal_Blade":    "item_diffusal_blade",
	"GreaterCritical":   "item_greater_crit",
	"Gungir":            "item_gungir",
	"GunpowderGauntlets":"item_gunpowder_gauntlets",
	"HandOfMidas":       "item_hand_of_midas",
	"Heart":             "item_heart",
	"Hyperstone":        "item_hyperstone",
	"Mjollnir":          "item_mjollnir",
	"MonkeyKingBar":     "item_monkey_king_bar",
	"Nullifier":         "item_nullifier",
	"OrchidMalevolence": "item_orchid",
	"Radiance":          "item_radiance",
	"Rapier":            "item_rapier",
	"RefresherOrb":      "item_refresher",
	"RefresherOrb_Shard":"item_refresher_shard",
	"Skadi":             "item_skadi",
	"SheepStick":        "item_sheepstick",
	"MantaStyle":        "item_manta",
	"Misericorde":       "item_misericorde",
	"EchoSabre":         "item_echo_sabre",
	"Satanic":           "item_satanic",
	"DragonLance":       "item_dragon_lance",
	"Maelstrom":         "item_maelstrom",
	"LesserCritical":    "item_lesser_crit",
	"Bloodthorn":        "item_bloodthorn",
	"Daedalus":          "item_greater_crit",

	// Armor & Defense
	"Assault_Cuirass":   "item_assault",
	"Black_King_Bar":    "item_black_king_bar",
	"Blade_Mail":        "item_blade_mail",
	"Crimson_Guard":     "item_crimson_guard",
	"GaleGuard":         "item_gale_guard",
	"GhostScepter":      "item_ghost",
	"GlimmerCape":       "item_glimmer_cape",
	"HeavensHalberd":    "item_heavens_halberd",
	"Pipe":              "item_pipe",
	"PyrrhicCloak":      "item_pyrrhic_cloak",
	"ShadowAmulet":      "item_shadow_amulet",
	"Shivas_Guard":      "item_shivas_guard",
	"Solar_Crest":       "item_solar_crest",
	"Vanguard":          "item_vanguard",
	"PlaneswalkersCloak":"item_planewalkers_cloak",
	"Lotus_Orb":         "item_lotus_orb",
	"LinkensSphere":     "item_sphere",
	"Halberd":           "item_heavens_halberd",

	// Boots
	"Arcane_Boots":      "item_arcane_boots",
	"PhaseBoots":        "item_phase_boots",
	"PowerTreads":       "item_power_treads",
	"TranquilBoots":     "item_tranquil_boots",
	"Boots":             "item_boots",
	"TravelBoots":       "item_travel_boots",
	"SamuraiTabi":       "item_samurai_tabi",
	"HermesSandals":     "item_hermes_sandals",
	"WitchesSwitch":     "item_witches_switch",

	// Intelligence / Caster
	"Aether_Lens":       "item_aether_lens",
	"Ancient_Janggo":    "item_ancient_janggo",
	"Cyclone":           "item_cyclone",
	"ForceStaff":        "item_force_staff",
	"Octarine_Core":     "item_octarine_core",
	"UltimateScepter":   "item_ultimate_scepter",
	"VeilofDiscord":     "item_veil_of_discord",
	"WindWaker":         "item_wind_waker",
	"Kaya":              "item_kaya",
	"Kaya_And_Sange":    "item_kaya_and_sange",
	"Yasha_And_Kaya":    "item_yasha_and_kaya",
	"Sange_And_Yasha":   "item_sange_and_yasha",
	"Sange":             "item_sange",
	"Yasha":             "item_yasha",
	"WitchBlade":        "item_witch_blade",
	"HurricanePike":     "item_hurricane_pike",
	"RodOfAtos":         "item_rod_of_atos",
	"Dagon":             "item_dagon",

	// Support
	"ObserverWard":      "item_ward_observer",
	"SentryWard":        "item_ward_sentry",
	"Ward_Dispenser":    "item_ward_dispenser",
	"Smoke_Of_Deceit":   "item_smoke_of_deceit",
	"Dust":              "item_dust",
	"HolyLocket":        "item_holy_locket",
	"Spirit_Vessel":     "item_spirit_vessel",
	"Mekansm":           "item_mekansm",
	"Guardian_Greaves":  "item_guardian_greaves",
	"Urn":               "item_urn_of_shadows",
	"Pavise":            "item_pavise",

	// Regen / Consumables
	"EmptyBottle":       "item_bottle",
	"Tango":             "item_tango",
	"Clarity":           "item_clarity",
	"Flask":             "item_flask",
	"Enchanted_Mango":   "item_enchanted_mango",
	"TpScroll":          "item_tpscroll",
	"Cheese":            "item_cheese",
	"Aegis":             "item_aegis",
	"Soul_Ring":         "item_soul_ring",

	// Basic components
	"Bracer":            "item_bracer",
	"WraithBand":        "item_wraith_band",
	"NullTalisman":      "item_null_talisman",
	"MagicStick":        "item_magic_stick",
	"MagicWand":         "item_magic_wand",
	"QuellingBlade":     "item_quelling_blade",
	"IronwoodBranch":    "item_branches",
	"PointBooster":      "item_point_booster",
	"StaffOfWizardry":   "item_staff_of_wizardry",
	"VoidStone":         "item_void_stone",
	"Fluffy_Hat":        "item_fluffy_hat",
	"Blight_Stone":      "item_blight_stone",
	"BladeOfAlacrity":   "item_blade_of_alacrity",
	"Buckler":           "item_buckler",
	"Headdress":         "item_headdress",
	"WindLace":          "item_wind_lace",
	"RingOfBasilius":    "item_ring_of_basilius",
	"OblivionStaff":     "item_oblivion_staff",
	"Perseverance":      "item_pers",
	"Cornucopia":        "item_cornucopia",
	"ManaDraught":       "item_mana_draught",
	"Diadem":            "item_diadem",
	"Crown":             "item_crown",

	// Lifesteal
	"MaskOfMadness":     "item_mask_of_madness",
	"Vladmir":           "item_vladmir",
	"HelmOfTheDominator":"item_helm_of_the_dominator",
	"HelmOfTheOverlord": "item_helm_of_the_overlord",

	// Neutral items
	"Giant_Maul":        "item_giant_maul",
	"SerratedShiv":      "item_serrated_shiv",
	"Chipped_Vest":      "item_chipped_vest",
	"Dezun_Bloodrite":   "item_dezun_bloodrite",
	"Dormant_Curio":     "item_dormant_curio",
	"Magnifying_Monocle":"item_magnifying_monocle",
	"Psychic_Headband":  "item_psychic_headband",
	"TiaraOfSelemene":   "item_tiara_of_selemene",
	"Whisper_Of_The_Dread":"item_whisper_of_the_dread",
}

// itemNameToID is generated from OpenDota constants — see items_constants.go.

// isBuildingTarget returns true if the combat-log target name refers to a
// destructible structure (tower, barracks, fort/ancient, shrine). OpenDota
// counts damage to all of these under tower_damage.
func isBuildingTarget(name string) bool {
	if name == "" {
		return false
	}
	for _, sub := range []string{"tower", "rax", "barrack", "fort", "ancient", "shrine", "building"} {
		if strings.Contains(name, sub) {
			return true
		}
	}
	return false
}

// buildingIsRadiant tells which side a structure belongs to based on naming
// (Dota uses goodguys = radiant, badguys = dire). Returns false for second
// return if undecidable.
func buildingIsRadiant(name string) (bool, bool) {
	if strings.Contains(name, "goodguys") {
		return true, true
	}
	if strings.Contains(name, "badguys") {
		return false, true
	}
	return false, false
}

// normalizeEntityItemName converts entity class name to standard item_ format
func normalizeEntityItemName(entityName string) string {
	if standard, ok := entityClassToItemName[entityName]; ok {
		return standard
	}
	// Fallback: convert CamelCase/Mixed_Case to snake_case with item_ prefix
	return "item_" + camelToSnake(entityName)
}

// camelToSnake converts CamelCase or Mixed_Case to snake_case
func camelToSnake(s string) string {
	var result []byte
	for i, c := range s {
		if c >= 'A' && c <= 'Z' {
			if i > 0 && s[i-1] != '_' {
				result = append(result, '_')
			}
			result = append(result, byte(c+32)) // toLower
		} else {
			result = append(result, byte(c))
		}
	}
	return strings.ToLower(string(result))
}

// ============= PARSER STATE =============

type MinuteSnapshot struct {
	Gold   int
	XP     int
	LH     int
	Denies int
	NW     int
	Level  int
}

type PlayerState struct {
	HeroID          int
	SteamID         int64
	IsRadiant       bool
	Kills           int
	Deaths          int
	Assists         int
	LastHits        int
	Denies          int
	Gold            int
	NetWorth        int
	XP              int
	Level           int
	HeroDamage      int
	TowerDamage     int
	HeroHealing     int
	
	// Final items (IDs - legacy)
	Items       [6]int
	Backpack    [3]int
	NeutralItem int
	
	// Final items (names from entity class)
	FinalItems   [6]string
	FinalNeutral string
	
	// Events
	ItemPurchases []ItemPurchase
	DeathEvents   []DeathEvent
	KillEvents    []KillEvent
	AssistEvents  []AssistEvent
	Wards         []WardEvent
	Runes         []RuneEvent

	// Per-minute snapshots
	MinuteSnapshots []MinuteSnapshot
	LastMinute      int

	// Ability tracking
	AbilityCasts map[string]int

	// Timestamped timelines (v4.4.0)
	AbilityCastEvents []AbilityCastEvent
	ItemCastEvents    []ItemCastEvent
	NearDeathEvents   []NearDeathEvent
	MaxHealth         int     // last known m_iMaxHealth from the hero entity
	LastNearDeathTime float64 // dedup window for NearDeathEvents

	// Damage tracking
	DamageByTarget         map[int]*DamageTarget
	DamageReceivedPhysical int
	DamageReceivedMagical  int
	DamageReceivedPure     int
	StunDurationDealt      float64

	// Item usage tracking (item_name → count)
	ItemUsage  map[string]int
	CampStacks []CampStack
	// v4.4.2: final totals from CDOTA_DataRadiant/Dire m_vecDataTeam —
	// the NEUTRAL_CAMP_STACK combat-log type (20) never fires in real
	// demos (verified empirically), so per-event CampStacks stays empty.
	CampsStacked int
	CreepsStacked int
	
	// Skill build tracking
	SkillBuild      []SkillLevelUp
	PrevAbilityLvls map[string]int // ability name → last known level

	// Talent tracking: ability slot index (m_vecAbilities.NNNN) → talent
	// info for special_bonus_* abilities. Overwritten on every hero entity
	// update so it holds the final state at end of replay.
	TalentSlots map[int]AbilityEntityInfo

	// Lane tracking
	LaneDeaths   int
	LaneKills    int
	LaneAssists  int
	ReaggroCount int
	LanePositions []struct{ X, Y float64 }
	
	// TP tracking
	TPCount      int
	CurrentHasTP bool // tracked from m_hItems.0015 entity

	// Smoke tracking
	SmokeCount int

	// Vision: enemy wards destroyed by this player (deward), v4.3.0
	WardsDewarded int

	// Reaggro tracking (physical attacks on enemy heroes in lane)
	LaneHarassCount int

	// Enemy-hero damage received before 5:00 (lane punishment taken)
	LaneDamageTakenPre5 int

	// Position tracking
	LastPosX float64
	LastPosY float64

	// Creep kill tracking (lane vs jungle)
	LaneCreepKills     int
	JungleCreepKills   int
	LaneCreepsPre10    int
	LaneCreeps10to25   int
	LaneCreeps25Plus   int
	JungleCreepsPre10  int
	JungleCreeps10to25 int
	JungleCreeps25Plus int
	
	// 10-min snapshots
	GoldAt10      int
	XPAt10        int
	NWAt10        int
	LevelAt10     int
	EntityXPAt10  int // from entity m_iTotalEarnedXP (more accurate than combat log)
}

type ParserState struct {
	Parser       *manta.Parser
	Players      [10]*PlayerState
	WardEvents   []WardEvent
	KillEvents   []KillEvent
	MatchID      int64
	GameMode     int
	LobbyType    int
	RadiantWin   bool
	StartTime    int64
	CurrentTick  uint32
	
	// Team tracking for leads
	RadiantGold []int
	DireGold    []int
	RadiantXP   []int
	DireXP      []int
	
	// Match events
	RoshanKills   []RoshanEvent
	AegisEvents   []AegisEvent
	Buybacks      []BuybackEvent
	RuneSpawns    []RuneEvent
	BuildingKills []BuildingEvent
	
	// Item entity tracking for final items
	ItemEntities map[int32]string

	// Ability entity tracking for skill build
	AbilityEntities map[int32]*AbilityEntityInfo

	// Ward entity → in-flight ward, для длительности (v4.3.0)
	ActiveWards map[int32]*activeWard

	// Game timing from CDOTAGamerulesProxy
	GameStartTime float64 // m_flGameStartTime (server time when game clock starts)
	GameEndTime   float64 // m_flGameEndTime (server time when game ends)

	// Wall-clock alignment. TickInterval comes from CSVCMsg_ServerInfo (seconds
	// per tick; ~1/30 for Dota 2). HornTick is the parser tick when GameStartTime
	// first became non-zero — i.e. the horn moment. Combined with StartTime
	// (replay-start unix sec) they yield hornDateTime in unix sec at output.
	TickInterval float32
	HornTick     uint32

	// Pause tracking. PauseActive mirrors m_pGameRules.m_bGamePaused. On false→true
	// we snapshot tick + game-time + team; on true→false we compute duration and
	// append to Pauses. PauseStartTicks is a parallel slice of start ticks used
	// by buildMatch to fill WallStart once StartTime is known.
	PauseActive        bool
	PauseStartTick     uint32
	PauseStartGameSec  float64
	PauseStartedByTeam int
	Pauses             []PauseEvent
	PauseStartTicks    []uint32
	// v4.4.3: суммарные тики завершённых пауз — вычитаются в GameTime(),
	// чтобы тиковые часы совпадали с серверной игровой осью (m_fGameTime),
	// на которой живут m_flGameStartTime/EndTime и таймстампы комбатлога.
	PausedTicksTotal uint32

	// Rune entity tracking for pickup attribution
	PendingRunes map[int32]*RuneEntityInfo // entityIdx → rune info

	// Bounty rune gold tracking (gold_reason=17) for confirmation
	BountyGoldEvents []BountyGoldEvent

	// Position sampling (every minute)
	PositionSamples   []PositionSample
	LastSampledMinute int

	// Gold lost tracking (GOLD event fires BEFORE DEATH event, so we buffer)
	PendingGoldLost [10]PendingGoldLoss

	// Raw smoke modifier-add events (time + player index). Группируются в
	// buildMatchOutput в SmokeEvent при ≥4 одной команды в окне 1.5s.
	SmokeModifierAdds []SmokeModifierAdd
}

type SmokeModifierAdd struct {
	Time      float64
	PlayerIdx int
}

type PendingGoldLoss struct {
	Time     float64
	GoldLost int
}

// RuneEntityInfo tracks a spawned rune entity for pickup attribution
type RuneEntityInfo struct {
	RuneType  int
	PosX      float64
	PosY      float64
	SpawnTime float64
}

// BountyGoldEvent tracks individual gold_reason=17 events
type BountyGoldEvent struct {
	Time      float64
	PlayerIdx int
	Gold      int
}

func NewParserState(p *manta.Parser) *ParserState {
	state := &ParserState{
		Parser:       p,
		Players:      [10]*PlayerState{},
		WardEvents:   make([]WardEvent, 0),
		KillEvents:   make([]KillEvent, 0),
		RadiantGold:  make([]int, 0),
		DireGold:     make([]int, 0),
		RadiantXP:    make([]int, 0),
		DireXP:       make([]int, 0),
		RoshanKills:   make([]RoshanEvent, 0),
		Buybacks:      make([]BuybackEvent, 0),
		RuneSpawns:    make([]RuneEvent, 0),
		BuildingKills: make([]BuildingEvent, 0),
		ItemEntities:       make(map[int32]string),
		AbilityEntities:    make(map[int32]*AbilityEntityInfo),
		ActiveWards:        make(map[int32]*activeWard),
		PendingRunes:       make(map[int32]*RuneEntityInfo),
		BountyGoldEvents:   make([]BountyGoldEvent, 0),
		PositionSamples:    make([]PositionSample, 0),
		Pauses:             make([]PauseEvent, 0),
	}
	for i := 0; i < 10; i++ {
		state.Players[i] = &PlayerState{
			// IsRadiant is resolved from CDOTA_PlayerResource.m_vecPlayerData.NNNN.m_iPlayerTeam
			// at parse time. Hardcoding `i < 5` here was wrong — player_id ordering
			// does not always align with team membership (see match 8788500456).
			LastMinute:      -1, // first snapshot fires at gameMinute=0 (horn) so series[i] aligns with OpenDota's gold_t/xp_t/lh_t/dn_t at minute i.
			ItemPurchases:   make([]ItemPurchase, 0),
			DeathEvents:     make([]DeathEvent, 0),
			KillEvents:      make([]KillEvent, 0),
			Wards:           make([]WardEvent, 0),
			Runes:           make([]RuneEvent, 0),
			MinuteSnapshots: make([]MinuteSnapshot, 0),
			AbilityCasts:    make(map[string]int),
			DamageByTarget:  make(map[int]*DamageTarget),
			LanePositions:   make([]struct{ X, Y float64 }, 0),
			PrevAbilityLvls: make(map[string]int),
			SkillBuild:      make([]SkillLevelUp, 0),
			TalentSlots:     make(map[int]AbilityEntityInfo),
		}
	}
	return state
}

func (s *ParserState) LookupName(index uint32) string {
	if name, ok := s.Parser.LookupStringByIndex("CombatLogNames", int32(index)); ok {
		return name
	}
	return fmt.Sprintf("unknown_%d", index)
}

// GameTime возвращает СЕРВЕРНЫЕ игровые часы (ось m_fGameTime, на которой
// живут m_flGameStartTime/EndTime и таймстампы комбатлога): тики демо минус
// все тики пауз. Сырой tick/30 убегает вперёд на сумму пауз: 21.6-минутная
// пауза в драфте (матч 8883829567) сдвигала окно лейн-детекта «1..10 минута»
// целиком в паузу — laneStats/lane умирали у всех десяти игроков, а в любых
// запаузенных матчах entity-таймстампы (касты, варды, руны, поминутные серии)
// расходились с комбатлогом на сумму пауз. Проверка оси: wall-прегейм 2085с −
// паузы 1306с = 779 ≈ m_flGameStartTime 778.4. (v4.4.3)
func (s *ParserState) GameTime() float64 {
	tick := s.CurrentTick
	if s.PauseActive && s.PauseStartTick > 0 && s.PauseStartTick < tick {
		tick = s.PauseStartTick // во время паузы часы стоят
	}
	if tick > s.PausedTicksTotal {
		tick -= s.PausedTicksTotal
	} else {
		tick = 0
	}
	ti := float64(s.TickInterval)
	if ti <= 0 {
		ti = 1.0 / 30.0
	}
	return float64(tick) * ti
}

func (s *ParserState) GameMinute() int {
	return int(s.GameTime() / 60.0)
}

// ActualGameSeconds converts raw server time to game clock (0 = horn, negative = pregame)
func (s *ParserState) ActualGameSeconds(rawTime float64) float64 {
	if s.GameStartTime > 0 {
		return rawTime - s.GameStartTime
	}
	return rawTime
}

// isWardDeward сообщает, что смерть варда — это снос врагом (а не естественное
// истечение). Combat-log называет истёкший вард так, что attacker == target
// ("npc_dota_*_wards" сам себя); снос даёт attacker = герой/юнит. (v4.3.0)
func isWardDeward(targetName, attackerName string) bool {
	if targetName != "npc_dota_observer_wards" && targetName != "npc_dota_sentry_wards" {
		return false
	}
	return attackerName != targetName
}

// finalizeWard закрывает жизнь варда на удалении сущности: проставляет EndTime
// и Duration в соответствующий WardEvent игрока. Варды, дожившие до конца игры,
// не получают удаления → остаются без Duration (omitempty) и исключаются из
// средней длительности агрегатором. (v4.3.0)
func finalizeWard(s *ParserState, idx int32, now float64) {
	w, ok := s.ActiveWards[idx]
	if !ok {
		return
	}
	delete(s.ActiveWards, idx)
	if w.playerIdx < 0 || w.playerIdx >= 10 {
		return
	}
	pw := s.Players[w.playerIdx].Wards
	if w.sliceIdx < 0 || w.sliceIdx >= len(pw) {
		return
	}
	dur := now - w.start
	if dur < 0 {
		dur = 0
	}
	pw[w.sliceIdx].EndTime = now
	pw[w.sliceIdx].Duration = dur
}

// Convert replay player ID to 0-9 index
// Source 2: m_iPlayerID uses even numbers (0,2,4,6,8 radiant; 10,12,14,16,18 dire)
func replayPlayerToIndex(replayID uint32) int {
	if replayID < 10 {
		return int(replayID / 2)
	}
	return int((replayID-10)/2) + 5
}

// ============= RUNE ATTRIBUTION HELPERS =============

// findNearestHero returns the player index (0-9) of the hero closest to the given position.
// Returns -1 if no hero has a valid position.
func findNearestHero(state *ParserState, posX, posY float64) int {
	bestIdx := -1
	bestDist := math.MaxFloat64
	for i := 0; i < 10; i++ {
		ps := state.Players[i]
		if ps.LastPosX == 0 && ps.LastPosY == 0 {
			continue // no position data yet
		}
		dx := ps.LastPosX - posX
		dy := ps.LastPosY - posY
		dist := dx*dx + dy*dy // squared distance is fine for comparison
		if dist < bestDist {
			bestDist = dist
			bestIdx = i
		}
	}
	return bestIdx
}

// heroDistToPoint returns the Euclidean distance from hero to a point.
func heroDistToPoint(state *ParserState, playerIdx int, posX, posY float64) float64 {
	ps := state.Players[playerIdx]
	dx := ps.LastPosX - posX
	dy := ps.LastPosY - posY
	return math.Sqrt(dx*dx + dy*dy)
}

// hasBountyGoldNear checks if there's a gold_reason=17 event for the given player
// within 5 seconds of the specified time (confirms the rune was actually picked up).
func hasBountyGoldNear(state *ParserState, time float64, playerIdx int) bool {
	if playerIdx < 0 || playerIdx >= 10 {
		return false
	}
	// Check if any teammate (same team) got bounty gold within 5 seconds
	isRadiant := state.Players[playerIdx].IsRadiant
	for _, bg := range state.BountyGoldEvents {
		if bg.Time >= time-5 && bg.Time <= time+5 {
			if bg.PlayerIdx < 0 || bg.PlayerIdx >= 10 {
				continue
			}
			if state.Players[bg.PlayerIdx].IsRadiant == isRadiant {
				return true
			}
		}
	}
	return false
}

// ============= MAIN PARSER =============

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: dota-replay-parser <replay.dem>")
	}

	replayPath := os.Args[1]
	log.Printf("Parsing replay: %s", replayPath)

	f, err := os.Open(replayPath)
	if err != nil {
		log.Fatalf("Failed to open replay: %v", err)
	}
	defer f.Close()

	p, err := manta.NewStreamParser(f)
	if err != nil {
		log.Fatalf("Failed to create parser: %v", err)
	}

	state := NewParserState(p)

	// Capture tick interval (seconds per tick) for wall-clock conversion of
	// per-tick events. Dota 2 ships at 30 tps so this is normally ~0.0333.
	p.Callbacks.OnCSVCMsg_ServerInfo(func(m *dota.CSVCMsg_ServerInfo) error {
		if ti := m.GetTickInterval(); ti > 0 {
			state.TickInterval = ti
		}
		return nil
	})

	// Demo file info callback (contains match ID)
	p.Callbacks.OnCDemoFileInfo(func(m *dota.CDemoFileInfo) error {
		if gi := m.GetGameInfo(); gi != nil {
			if d := gi.GetDota(); d != nil {
				state.MatchID = int64(d.GetMatchId())
				state.GameMode = int(d.GetGameMode())
				state.RadiantWin = d.GetGameWinner() == 2
				state.StartTime = int64(d.GetEndTime()) - int64(m.GetPlaybackTime())
			}
		}
		return nil
	})

	// Combat log callback
	// v4.4.1: Aegis pickups arrive as chat events (playerid_1 = player slot,
	// verified 0-9 direct — no entity-id remap needed).
	p.Callbacks.OnCDOTAUserMsg_ChatEvent(func(m *dota.CDOTAUserMsg_ChatEvent) error {
		var kind string
		switch m.GetType() {
		case dota.DOTA_CHAT_MESSAGE_CHAT_MESSAGE_AEGIS:
			kind = "taken"
		case dota.DOTA_CHAT_MESSAGE_CHAT_MESSAGE_AEGIS_STOLEN:
			kind = "stolen"
		case dota.DOTA_CHAT_MESSAGE_CHAT_MESSAGE_DENIED_AEGIS:
			kind = "denied"
		default:
			return nil
		}
		pid := int(m.GetPlayerid_1())
		if pid >= 0 && pid < 10 {
			state.AegisEvents = append(state.AegisEvents, AegisEvent{
				Time:      state.ActualGameSeconds(state.GameTime()),
				PlayerIdx: pid,
				Event:     kind,
			})
		}
		return nil
	})

	p.Callbacks.OnCMsgDOTACombatLogEntry(func(m *dota.CMsgDOTACombatLogEntry) error {
		gameTime := float64(m.GetTimestamp())
		actualTime := state.ActualGameSeconds(gameTime) // 0 = horn
		logType := m.GetType()

		// HornTick: first combat-log event at or after horn (game-clock >= 0)
		// once GameStartTime is known. Combat-log entries arrive every server
		// tick during gameplay so this lands within ~33ms of true horn. We
		// store the server tick (p.NetTick) — not p.Tick — because tick × the
		// interval from CSVCMsg_ServerInfo must yield server-time, which is
		// what GameStartTime is denominated in.
		if state.HornTick == 0 && state.GameStartTime > 0 && actualTime >= 0 {
			state.HornTick = p.NetTick
		}

		switch logType {
		case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_DEATH:
			targetName := state.LookupName(m.GetTargetName())
			attackerName := state.LookupName(m.GetAttackerName())

			// Deward attribution (v4.3.0): ward убит, если это не естественное
			// истечение (attacker == target = ward "сам себя").
			if isWardDeward(targetName, attackerName) {
				killerIdx := heroNameToPlayerIndex(attackerName, state)
				if killerIdx >= 0 && killerIdx < 10 {
					state.Players[killerIdx].WardsDewarded++
				}
			}

			if strings.Contains(targetName, "hero") {
				targetIdx := heroNameToPlayerIndex(targetName, state)
				attackerIdx := heroNameToPlayerIndex(attackerName, state)

				// Extract assist player indices from protobuf
				var assistIndices []int
				assistPlayers := m.GetAssistPlayers()
				if len(assistPlayers) > 0 {
					for _, apIdx := range assistPlayers {
						idx := int(apIdx)
						if idx >= 0 && idx < 10 {
							assistIndices = append(assistIndices, idx)
						}
					}
				} else {
					// Legacy fallback: assist_player0..3
					for _, ap := range []uint32{m.GetAssistPlayer0(), m.GetAssistPlayer1(), m.GetAssistPlayer2(), m.GetAssistPlayer3()} {
						if ap > 0 && int(ap) < 10 {
							assistIndices = append(assistIndices, int(ap))
						}
					}
				}

				if targetIdx >= 0 && targetIdx < 10 {
					state.Players[targetIdx].Deaths++

					// Compute nearby heroes at death for coaching patterns
					deathX := state.Players[targetIdx].LastPosX
					deathY := state.Players[targetIdx].LastPosY
					targetIsRadiant := state.Players[targetIdx].IsRadiant
					const deathProximity = 30.0 // ~1200 game units, approx spell range
					var nearbyAllies, nearbyEnemies []int
					for j := 0; j < 10; j++ {
						if j == targetIdx {
							continue
						}
						jp := state.Players[j]
						if jp.LastPosX == 0 && jp.LastPosY == 0 {
							continue
						}
						dx := jp.LastPosX - deathX
						dy := jp.LastPosY - deathY
						dist := math.Sqrt(dx*dx + dy*dy)
						if dist <= deathProximity {
							if jp.IsRadiant == targetIsRadiant {
								nearbyAllies = append(nearbyAllies, j)
							} else {
								nearbyEnemies = append(nearbyEnemies, j)
							}
						}
					}

					// Apply pending gold loss (GOLD event fires before DEATH in combat log)
					goldLost := 0
					pending := state.PendingGoldLost[targetIdx]
					if pending.GoldLost > 0 && actualTime-pending.Time < 2.0 && actualTime-pending.Time >= -0.5 {
						goldLost = pending.GoldLost
						state.PendingGoldLost[targetIdx] = PendingGoldLoss{} // clear
					}

					state.Players[targetIdx].DeathEvents = append(state.Players[targetIdx].DeathEvents, DeathEvent{
						Time:            actualTime,
						Killer:          attackerIdx,
						KillerName:      attackerName,
						Assists:         assistIndices,
						PositionX:       deathX,
						PositionY:       deathY,
						NetworthAtDeath: state.Players[targetIdx].NetWorth,
						HadTP:           state.Players[targetIdx].CurrentHasTP,
						NearbyAllies:    nearbyAllies,
						NearbyEnemies:   nearbyEnemies,
						TimeDead:        respawnTime(state.Players[targetIdx].Level),
						GoldLost:        goldLost,
					})
					if actualTime >= 0 && actualTime < 600 {
						state.Players[targetIdx].LaneDeaths++
					}
				}

				// Track assists (total + events + lane)
				for _, aIdx := range assistIndices {
					if aIdx >= 0 && aIdx < 10 {
						state.Players[aIdx].Assists++
						state.Players[aIdx].AssistEvents = append(state.Players[aIdx].AssistEvents, AssistEvent{
							Time:   actualTime,
							Target: targetIdx,
						})
						if actualTime >= 0 && actualTime < 600 {
							state.Players[aIdx].LaneAssists++
						}
					}
				}

				if attackerIdx >= 0 && attackerIdx < 10 {
					state.Players[attackerIdx].Kills++
					state.Players[attackerIdx].KillEvents = append(state.Players[attackerIdx].KillEvents, KillEvent{
						Time:       actualTime,
						Target:     targetIdx,
						TargetName: targetName,
					})
					if actualTime >= 0 && actualTime < 600 {
						state.Players[attackerIdx].LaneKills++
					}
				}
			}
			
			// Track Roshan death via combat log (exact match, deduplicate within 10s window)
			if targetName == "npc_dota_roshan" {
				isDuplicate := false
				for _, prev := range state.RoshanKills {
					if actualTime-prev.Time < 10 && actualTime-prev.Time > -10 {
						isDuplicate = true
						break
					}
				}
				if !isDuplicate {
					killerIdx := heroNameToPlayerIndex(attackerName, state)
					team := "dire"
					if killerIdx >= 0 && killerIdx < 10 && state.Players[killerIdx].IsRadiant {
						team = "radiant"
					}
					state.RoshanKills = append(state.RoshanKills, RoshanEvent{
						Time:   actualTime,
						Killer: killerIdx,
						Team:   team,
					})
				}
			}

			// Track building deaths
			if strings.Contains(targetName, "tower") || strings.Contains(targetName, "rax") ||
			   strings.Contains(targetName, "barracks") || strings.Contains(targetName, "fort") {
				isRadiantBuilding := strings.Contains(targetName, "goodguys")
				killerPlayerIdx := heroNameToPlayerIndex(attackerName, state)
				state.BuildingKills = append(state.BuildingKills, BuildingEvent{
					Time:         actualTime,
					Building:     targetName,
					IsRadiant:    isRadiantBuilding,
					Killer:       attackerName,
					KillerPlayer: killerPlayerIdx,
				})
			}

			// Track creep kills: lane vs jungle (for coaching pattern P-014)
			if strings.Contains(attackerName, "hero") {
				isLaneCreep := strings.Contains(targetName, "npc_dota_creep_goodguys") ||
					strings.Contains(targetName, "npc_dota_creep_badguys") ||
					strings.Contains(targetName, "npc_dota_goodguys_siege") ||
					strings.Contains(targetName, "npc_dota_badguys_siege")
				isJungleCreep := strings.Contains(targetName, "npc_dota_neutral_")

				if isLaneCreep || isJungleCreep {
					atkIdx := heroNameToPlayerIndex(attackerName, state)
					if atkIdx >= 0 && atkIdx < 10 {
						if isLaneCreep {
							state.Players[atkIdx].LaneCreepKills++
							if actualTime < 600 {
								state.Players[atkIdx].LaneCreepsPre10++
							} else if actualTime < 1500 {
								state.Players[atkIdx].LaneCreeps10to25++
							} else {
								state.Players[atkIdx].LaneCreeps25Plus++
							}
						}
						if isJungleCreep {
							state.Players[atkIdx].JungleCreepKills++
							if actualTime < 600 {
								state.Players[atkIdx].JungleCreepsPre10++
							} else if actualTime < 1500 {
								state.Players[atkIdx].JungleCreeps10to25++
							} else {
								state.Players[atkIdx].JungleCreeps25Plus++
							}
						}
					}
				}
			}

		case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_PURCHASE:
			targetName := state.LookupName(m.GetTargetName())
			valueName := state.LookupName(m.GetValue())
			playerIdx := heroNameToPlayerIndex(targetName, state)

			if playerIdx >= 0 && playerIdx < 10 {
				state.Players[playerIdx].ItemPurchases = append(state.Players[playerIdx].ItemPurchases, ItemPurchase{
					Time:     actualTime,
					ItemName: valueName,
					ItemID:   itemNameToID[valueName],
				})
			}

		case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_DAMAGE:
			attackerName := state.LookupName(m.GetAttackerName())
			targetName := state.LookupName(m.GetTargetName())
			damage := int(m.GetValue())
			damageType := m.GetDamageType()

			if strings.Contains(targetName, "hero") && strings.Contains(attackerName, "hero") {
				attackerIdx := heroNameToPlayerIndex(attackerName, state)
				targetIdx := heroNameToPlayerIndex(targetName, state)

				// hero_damage = damage dealt to *enemy* heroes only (OpenDota
				// definition). Skip self-damage and ally-damage.
				isEnemyHero := attackerIdx >= 0 && attackerIdx < 10 &&
					targetIdx >= 0 && targetIdx < 10 &&
					attackerIdx != targetIdx &&
					state.Players[attackerIdx].IsRadiant != state.Players[targetIdx].IsRadiant

				if isEnemyHero {
					state.Players[attackerIdx].HeroDamage += damage

					// Lane damage taken (victim side): enemy-hero damage the target
					// eats before 5:00 — inverse of LaneHarassCount; surfaces poor
					// lane standing ("how much you get punished on the lane").
					if actualTime < 300 {
						state.Players[targetIdx].LaneDamageTakenPre5 += damage
					}

					// Track damage by target
					if state.Players[attackerIdx].DamageByTarget[targetIdx] == nil {
						state.Players[attackerIdx].DamageByTarget[targetIdx] = &DamageTarget{Target: targetIdx}
					}
					switch damageType {
					case 1: // Physical
						state.Players[attackerIdx].DamageByTarget[targetIdx].PhysicalDamage += damage

						// Track reaggro (physical attacks on heroes during laning)
						if actualTime > 60 && actualTime < 600 {
							state.Players[attackerIdx].LaneHarassCount++
						}
					case 2: // Magical
						state.Players[attackerIdx].DamageByTarget[targetIdx].MagicalDamage += damage
					case 4: // Pure
						state.Players[attackerIdx].DamageByTarget[targetIdx].PureDamage += damage
					}
				}

				// v4.4.0: near-death timeline — the raw signal for clutch-escape
				// detection. GetHealth() is the target's HP AFTER this hit;
				// MaxHealth comes from the hero entity scan. Illusions excluded
				// (they die at low HP constantly), one event per 5s window.
				// Hero-attacker hits only (this branch) — enough for clutch
				// moments, tower/creep chip is not tracked here.
				if targetIdx >= 0 && targetIdx < 10 && !m.GetIsTargetIllusion() {
					tp := state.Players[targetIdx]
					if hp := int(m.GetHealth()); hp > 0 && tp.MaxHealth > 0 {
						if pct := hp * 100 / tp.MaxHealth; pct <= 20 &&
							actualTime-tp.LastNearDeathTime >= 5 {
							tp.LastNearDeathTime = actualTime
							ev := NearDeathEvent{Time: actualTime, HpPct: pct}
							if attackerIdx >= 0 && attackerIdx < 10 {
								ev.Attacker = strings.TrimPrefix(attackerName, "npc_dota_hero_")
							}
							tp.NearDeathEvents = append(tp.NearDeathEvents, ev)
						}
					}
				}

				// Track damage received by target (any hero→hero, even ally —
				// this is the inverse view, useful for separate analysis).
				if targetIdx >= 0 && targetIdx < 10 {
					switch damageType {
					case 1:
						state.Players[targetIdx].DamageReceivedPhysical += damage
					case 2:
						state.Players[targetIdx].DamageReceivedMagical += damage
					case 4:
						state.Players[targetIdx].DamageReceivedPure += damage
					}
				}
			}

			// tower_damage = damage by hero to *enemy* buildings only.
			// OpenDota lumps tower/rax/ancient/fort under tower_damage, but
			// excludes ally-side damage (e.g. teammate's tower hit by AoE).
			if isBuildingTarget(targetName) && strings.Contains(attackerName, "hero") {
				attackerIdx := heroNameToPlayerIndex(attackerName, state)
				if attackerIdx >= 0 && attackerIdx < 10 {
					if isRad, ok := buildingIsRadiant(targetName); ok && isRad != state.Players[attackerIdx].IsRadiant {
						state.Players[attackerIdx].TowerDamage += damage
					}
				}
			}

		case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_HEAL:
			attackerName := state.LookupName(m.GetAttackerName())
			targetName := state.LookupName(m.GetTargetName())
			value := int(m.GetValue())
			attackerIdx := heroNameToPlayerIndex(attackerName, state)
			targetIdx := heroNameToPlayerIndex(targetName, state)
			// hero_healing = healing applied to *allied* heroes excluding self,
			// to match OpenDota's definition. Skip self-heal, non-hero targets,
			// regen, and enemy heroes.
			if attackerIdx >= 0 && attackerIdx < 10 &&
				strings.Contains(targetName, "hero") &&
				attackerName != targetName &&
				targetIdx >= 0 && targetIdx < 10 &&
				state.Players[attackerIdx].IsRadiant == state.Players[targetIdx].IsRadiant {
				state.Players[attackerIdx].HeroHealing += value
			}

		case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_ABILITY:
			attackerName := state.LookupName(m.GetAttackerName())
			abilityName := state.LookupName(m.GetInflictorName())
			attackerIdx := heroNameToPlayerIndex(attackerName, state)
			if attackerIdx >= 0 && attackerIdx < 10 && abilityName != "" {
				state.Players[attackerIdx].AbilityCasts[abilityName]++
				// v4.4.0: timestamped cast timeline. Toggles are excluded —
				// armlet/treads-style spam would drown the real casts.
				if !m.GetIsAbilityToggleOn() && !m.GetIsAbilityToggleOff() &&
					len(state.Players[attackerIdx].AbilityCastEvents) < 3000 {
					ev := AbilityCastEvent{
						Time:       actualTime,
						Ability:    abilityName,
						IsUltimate: m.GetIsUltimateAbility(),
						IsStolen:   m.GetInflictorIsStolenAbility(),
					}
					targetName := state.LookupName(m.GetTargetName())
					if strings.Contains(targetName, "hero") && !m.GetIsTargetIllusion() {
						if targetName == attackerName {
							ev.TargetSelf = true
						} else {
							ev.TargetHero = strings.TrimPrefix(targetName, "npc_dota_hero_")
						}
					}
					state.Players[attackerIdx].AbilityCastEvents = append(
						state.Players[attackerIdx].AbilityCastEvents, ev)
				}
			}

		case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_MODIFIER_ADD:
			targetName := state.LookupName(m.GetTargetName())
			modifierName := state.LookupName(m.GetInflictorName())

			// Track stun duration dealt (from modifier events, not damage)
			stunDur := float64(m.GetStunDuration())
			if stunDur > 0 && strings.Contains(targetName, "hero") {
				attackerName := state.LookupName(m.GetAttackerName())
				attackerIdx := heroNameToPlayerIndex(attackerName, state)
				if attackerIdx >= 0 && attackerIdx < 10 {
					state.Players[attackerIdx].StunDurationDealt += stunDur
				}
			}

			// Track TP scroll / travel boots usage. Both apply
			// modifier_teleporting, so we have to disambiguate via the
			// modifier_ability field — otherwise hero ult teleports
			// (Nature's Prophet, Boots of Travel, Spirit Breaker charge,
			// etc.) get counted as TP scrolls and inflate the metric
			// dramatically (NP can show 50+ "TPs" in a 30-min game).
			if modifierName == "modifier_teleporting" && strings.Contains(targetName, "hero") {
				modAbility := state.LookupName(m.GetModifierAbility())
				isItemTP := modAbility == "item_tpscroll" ||
					strings.Contains(modAbility, "travel_boots")
				if isItemTP {
					targetIdx := heroNameToPlayerIndex(targetName, state)
					if targetIdx >= 0 && targetIdx < 10 {
						state.Players[targetIdx].TPCount++
					}
				}
			}
			
			// Track smoke usage. Counter оставляем для back-compat; параллельно
			// копим raw events для group detection в buildMatchOutput.
			if modifierName == "modifier_smoke_of_deceit" && strings.Contains(targetName, "hero") {
				targetIdx := heroNameToPlayerIndex(targetName, state)
				if targetIdx >= 0 && targetIdx < 10 {
					state.Players[targetIdx].SmokeCount++
					state.SmokeModifierAdds = append(state.SmokeModifierAdds, SmokeModifierAdd{
						Time:      actualTime,
						PlayerIdx: targetIdx,
					})
				}
			}

			// Track rune pickups from modifier buffs (since combat log type 21 doesn't fire)
			if runeType, isRune := runeModifierToType[modifierName]; isRune && strings.Contains(targetName, "hero") {
				targetIdx := heroNameToPlayerIndex(targetName, state)
				if targetIdx >= 0 && targetIdx < 10 {
					state.Players[targetIdx].Runes = append(state.Players[targetIdx].Runes, RuneEvent{
						Time:     actualTime,
						RuneType: runeType,
						Action:   1, // pickup
					})
				}
			}

		case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_GOLD:
			targetName := state.LookupName(m.GetTargetName())
			gold := int(int32(m.GetValue())) // Cast via int32 to handle negative gold (purchases, death penalty)
			goldReason := m.GetGoldReason()
			playerIdx := heroNameToPlayerIndex(targetName, state)
			if playerIdx >= 0 && playerIdx < 10 {
				state.Players[playerIdx].Gold += gold
			}
			// Track bounty rune gold (gold_reason=17) for pickup confirmation
			if goldReason == 17 && playerIdx >= 0 && playerIdx < 10 {
				state.BountyGoldEvents = append(state.BountyGoldEvents, BountyGoldEvent{
					Time:      actualTime,
					PlayerIdx: playerIdx,
					Gold:      gold,
				})
			}
			// Track death gold penalty (gold_reason=1)
			// GOLD event fires BEFORE DEATH event in combat log, so we store pending penalties
			// and apply them when the DEATH event is created
			if goldReason == 1 && playerIdx >= 0 && playerIdx < 10 && gold < 0 {
				state.PendingGoldLost[playerIdx] = PendingGoldLoss{
					Time:     actualTime,
					GoldLost: -gold,
				}
			}

		case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_XP:
			targetName := state.LookupName(m.GetTargetName())
			xp := int(m.GetValue())
			playerIdx := heroNameToPlayerIndex(targetName, state)
			if playerIdx >= 0 && playerIdx < 10 {
				state.Players[playerIdx].XP += xp
			}

		case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_BUYBACK:
			// Value field contains player slot (0-9)
			playerIdx := int(m.GetValue())
			if playerIdx >= 0 && playerIdx < 10 {
				state.Buybacks = append(state.Buybacks, BuybackEvent{
					Time:     actualTime,
					PlayerID: playerIdx,
				})
			}

		case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_PICKUP_RUNE:
			// Attacker = hero who picks up, Target = rune entity.
			// NOTE: this combat-log type doesn't fire in current Dota replays
			// (kept for backwards compat); actual pickup attribution happens
			// in the entity-destroyed handler (~line 1693).
			heroName := state.LookupName(m.GetAttackerName())
			runeType := int(m.GetRuneType())
			playerIdx := heroNameToPlayerIndex(heroName, state)
			if playerIdx >= 0 && playerIdx < 10 {
				state.Players[playerIdx].Runes = append(state.Players[playerIdx].Runes, RuneEvent{
					Time:     actualTime,
					RuneType: runeType,
					Action:   1, // pickup
				})
			}

		case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_ITEM:
			// Track active item usage (BKB activations, PT toggles, etc.)
			heroName := state.LookupName(m.GetAttackerName())
			itemName := state.LookupName(m.GetInflictorName())
			playerIdx := heroNameToPlayerIndex(heroName, state)
			if playerIdx >= 0 && playerIdx < 10 && itemName != "" {
				if state.Players[playerIdx].ItemUsage == nil {
					state.Players[playerIdx].ItemUsage = make(map[string]int)
				}
				state.Players[playerIdx].ItemUsage[itemName]++
				// v4.4.0: timestamped item-use timeline. Power Treads stat
				// switching fires ITEM events WITHOUT toggle flags (observed:
				// 87 "uses" per game) — excluded by name; armlet toggling is
				// kept deliberately (clutch micro signal).
				if !m.GetIsAbilityToggleOn() && !m.GetIsAbilityToggleOff() &&
					itemName != "item_power_treads" &&
					len(state.Players[playerIdx].ItemCastEvents) < 3000 {
					ev := ItemCastEvent{Time: actualTime, Item: itemName}
					targetName := state.LookupName(m.GetTargetName())
					if strings.Contains(targetName, "hero") && !m.GetIsTargetIllusion() {
						if targetName == heroName {
							ev.TargetSelf = true
						} else {
							ev.TargetHero = strings.TrimPrefix(targetName, "npc_dota_hero_")
						}
					}
					state.Players[playerIdx].ItemCastEvents = append(
						state.Players[playerIdx].ItemCastEvents, ev)
				}
			}

		case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_NEUTRAL_CAMP_STACK:
			// Track camp stacking events
			heroName := state.LookupName(m.GetAttackerName())
			stackCount := int(m.GetStackCount())
			playerIdx := heroNameToPlayerIndex(heroName, state)
			if playerIdx >= 0 && playerIdx < 10 {
				state.Players[playerIdx].CampStacks = append(state.Players[playerIdx].CampStacks, CampStack{
					Time:       actualTime,
					StackCount: stackCount,
				})
			}
		}

		return nil
	})

	// Entity callback
	p.OnEntity(func(e *manta.Entity, op manta.EntityOp) error {
		state.CurrentTick = p.NetTick

		// Ward lifecycle: фиксируем удаление (истечение/снос) ДО early-return,
		// т.к. чистый EntityOpDeleted ниже выходит сразу. (v4.3.0)
		if op&manta.EntityOpDeleted != 0 {
			cn := e.GetClassName()
			if cn == "CDOTA_NPC_Observer_Ward" || cn == "CDOTA_NPC_Observer_Ward_TrueSight" {
				finalizeWard(state, e.GetIndex(), state.ActualGameSeconds(state.GameTime()))
			}
		}

		if op == manta.EntityOpDeleted {
			return nil
		}

		className := e.GetClassName()
		actualGameTime := state.ActualGameSeconds(state.GameTime())
		gameMinute := int(actualGameTime / 60.0)

		// Extract hero ID from hero entities
		// v4.4.2: per-player stacked totals live on the team-data entities
		// (m_vecDataTeam.N.m_iCampsStacked); index N maps to team slot →
		// global player index 0-4 (Radiant) / 5-9 (Dire).
		if className == "CDOTA_DataRadiant" || className == "CDOTA_DataDire" {
			offset := 0
			if className == "CDOTA_DataDire" {
				offset = 5
			}
			for i := 0; i < 5; i++ {
				if v, ok := e.GetInt32(fmt.Sprintf("m_vecDataTeam.%04d.m_iCampsStacked", i)); ok {
					state.Players[offset+i].CampsStacked = int(v)
				}
				if v, ok := e.GetInt32(fmt.Sprintf("m_vecDataTeam.%04d.m_iCreepsStacked", i)); ok {
					state.Players[offset+i].CreepsStacked = int(v)
				}
			}
		}

		if strings.HasPrefix(className, "CDOTA_Unit_Hero_") {
			if replayPlayerID, ok := e.GetUint32("m_iPlayerID"); ok {
				playerIdx := replayPlayerToIndex(replayPlayerID)

				if playerIdx >= 0 && playerIdx < 10 {
					heroName := strings.TrimPrefix(className, "CDOTA_Unit_Hero_")
					heroID := heroNameStringToID(heroName)
					if heroID > 0 && state.Players[playerIdx].HeroID == 0 {
						state.Players[playerIdx].HeroID = heroID
					}
					
					// v4.4.0: last known max HP — denominator for near-death
					// events in the combat-log damage handler.
					if maxHP, ok := e.GetInt32("m_iMaxHealth"); ok && maxHP > 0 {
						state.Players[playerIdx].MaxHealth = int(maxHP)
					}

					// Track position for lane detection
					if cellX, ok := e.GetUint64("CBodyComponent.m_cellX"); ok {
						if cellY, ok2 := e.GetUint64("CBodyComponent.m_cellY"); ok2 {
							state.Players[playerIdx].LastPosX = float64(cellX)
							state.Players[playerIdx].LastPosY = float64(cellY)
							
							// Store positions from 1-10 minutes for lane detection (skip fountain time)
							gt := state.ActualGameSeconds(state.GameTime())
							if state.GameStartTime > 0 && gt > 60 && gt < 600 {
								state.Players[playerIdx].LanePositions = append(
									state.Players[playerIdx].LanePositions,
									struct{ X, Y float64 }{float64(cellX), float64(cellY)},
								)
							}
						}
					}
					
					// Track current items. Resolve handle → entity → className live;
					// the ItemEntities cache can return stale names after entity index
					// reuse (manta recycles indexes when entities are destroyed).
					resolveItem := func(slotKey string) string {
						handle, ok := e.GetUint32(slotKey)
						if !ok || handle == 0 || handle >= 16777215 {
							return ""
						}
						entityIdx := int32(handle & 0x3FFF)
						if itemEnt := state.Parser.FindEntity(entityIdx); itemEnt != nil {
							cn := itemEnt.GetClassName()
							if strings.HasPrefix(cn, "CDOTA_Item_") {
								return normalizeEntityItemName(strings.TrimPrefix(cn, "CDOTA_Item_"))
							}
						}
						return state.ItemEntities[entityIdx]
					}
					for i := 0; i < 6; i++ {
						state.Players[playerIdx].FinalItems[i] = resolveItem(fmt.Sprintf("m_hItems.%04d", i))
					}
					for i := 0; i < 3; i++ {
						name := resolveItem(fmt.Sprintf("m_hItems.%04d", 6+i))
						state.Players[playerIdx].Backpack[i] = itemNameToID[name]
					}
					state.Players[playerIdx].FinalNeutral = resolveItem("m_hItems.0016")

					// Track TP scroll in dedicated slot 15 (Dota 2 7.23+)
					if handle, ok := e.GetUint32("m_hItems.0015"); ok && handle > 0 && handle < 16777215 {
						entityIdx := int32(handle & 0x3FFF)
						hasTP := false
						if itemName, exists := state.ItemEntities[entityIdx]; exists {
							hasTP = strings.Contains(itemName, "teleport") || strings.Contains(itemName, "tpscroll") ||
								strings.Contains(itemName, "travel_boots")
						} else {
							itemEnt := state.Parser.FindEntity(entityIdx)
							if itemEnt != nil {
								cn := itemEnt.GetClassName()
								hasTP = strings.Contains(cn, "TpScroll") || strings.Contains(cn, "TeleportScroll") ||
									strings.Contains(cn, "TravelBoots")
							}
						}
						state.Players[playerIdx].CurrentHasTP = hasTP
					} else {
						state.Players[playerIdx].CurrentHasTP = false
					}

					// Track ability ownership for skill build (scan ability handles via m_vecAbilities)
					for ai := 0; ai < 24; ai++ {
						key := fmt.Sprintf("m_vecAbilities.%04d", ai)
						if handle, ok := e.GetUint32(key); ok && handle > 0 && handle < 16777215 {
							entityIdx := int32(handle & 0x3FFF)
							// Look up ability entity directly from parser
							abEnt := state.Parser.FindEntity(entityIdx)
							if abEnt == nil {
								continue
							}

							// Track talent choices (v4.1.0). Talents are
							// special_bonus_* abilities; their identity is not
							// in the entity class name, so resolve the real
							// ability name via the EntityNames string table.
							// Runs before the CDOTA_Ability_ class filter
							// below because talent entities may use a
							// different class.
							if nameIdx, okN := abEnt.GetInt32("m_pEntity.m_nameStringTableIndex"); okN && nameIdx >= 0 {
								if entName, okT := state.Parser.LookupStringByIndex("EntityNames", nameIdx); okT && strings.HasPrefix(entName, "special_bonus_") {
									talentLvl := 0
									if l, okL := abEnt.GetInt32("m_iLevel"); okL {
										talentLvl = int(l)
									}
									state.Players[playerIdx].TalentSlots[ai] = AbilityEntityInfo{Name: entName, Level: talentLvl}
								}
							}

							abClass := abEnt.GetClassName()
							if !strings.HasPrefix(abClass, "CDOTA_Ability_") {
								continue
							}
							// Get clean ability name
							abilityName := strings.TrimPrefix(abClass, "CDOTA_Ability_")
							abilityName = strings.ToLower(abilityName)

							abilityLevel := 0
							if lvl, ok2 := abEnt.GetInt32("m_iLevel"); ok2 {
								abilityLevel = int(lvl)
							}

							if _, tracked := state.Players[playerIdx].PrevAbilityLvls[abilityName]; !tracked {
								// First time seeing this ability — just register baseline, don't record
								state.Players[playerIdx].PrevAbilityLvls[abilityName] = abilityLevel
							} else if abilityLevel > state.Players[playerIdx].PrevAbilityLvls[abilityName] {
								// Level increased — record skill up
								state.Players[playerIdx].SkillBuild = append(state.Players[playerIdx].SkillBuild, SkillLevelUp{
									Time:        actualGameTime,
									AbilityName: abilityName,
									Level:       abilityLevel,
									HeroLevel:   state.Players[playerIdx].Level,
								})
								state.Players[playerIdx].PrevAbilityLvls[abilityName] = abilityLevel
							}
						}
					}
				}
			}
		}

		// Track game rules
		if className == "CDOTAGamerulesProxy" {
			if gameWinner, ok := e.GetInt32("m_pGameRules.m_nGameWinner"); ok {
				state.RadiantWin = gameWinner == 2
			}
			if gameMode, ok := e.GetInt32("m_pGameRules.m_iGameMode"); ok {
				state.GameMode = int(gameMode)
			}
			// Track game start/end times for duration calculation
			if startTime, ok := e.GetFloat32("m_pGameRules.m_flGameStartTime"); ok {
				state.GameStartTime = float64(startTime)
			}
			// HornTick is set from the combat-log callback (see below) on the
			// first event with actualTime >= 0. m_pGameRules.m_iGameState is
			// not reliably exposed via Entity Get() on the proxy in this
			// schema, so we use the combat-log signal instead — it fires
			// within one server tick of horn, which is plenty for second-
			// level wall-clock alignment.
			if endTime, ok := e.GetFloat32("m_pGameRules.m_flGameEndTime"); ok {
				if endTime > 0 {
					state.GameEndTime = float64(endTime)
				}
			}

			// Pause tracking. m_bGamePaused flips true on pause-in, false on
			// pause-out. We snapshot at the edges and compute duration from
			// the tick delta × tickInterval. WallStart is filled in buildMatch
			// (depends on StartTime from CDemoFileInfo, which fires at end
			// of demo). The parallel PauseStartTicks slice carries the start
			// tick of each entry in Pauses.
			if paused, ok := e.GetBool("m_pGameRules.m_bGamePaused"); ok {
				if paused && !state.PauseActive {
					state.PauseActive = true
					state.PauseStartTick = p.NetTick
					if state.GameStartTime > 0 && state.TickInterval > 0 {
						state.PauseStartGameSec = float64(p.NetTick)*float64(state.TickInterval) - state.GameStartTime
					} else {
						state.PauseStartGameSec = 0
					}
					if team, ok2 := e.GetInt32("m_pGameRules.m_iPauseTeam"); ok2 {
						state.PauseStartedByTeam = int(team)
					} else {
						state.PauseStartedByTeam = 0
					}
				} else if !paused && state.PauseActive {
					state.PauseActive = false
					ticks := p.NetTick - state.PauseStartTick
					dur := float64(ticks) * float64(state.TickInterval)
					if dur < 0 {
						dur = 0
					}
					state.Pauses = append(state.Pauses, PauseEvent{
						GameTime:    state.PauseStartGameSec,
						DurationSec: dur,
						PausedBy:    state.PauseStartedByTeam,
					})
					state.PauseStartTicks = append(state.PauseStartTicks, state.PauseStartTick)
					state.PausedTicksTotal += ticks
				}
			}
		}

		// Track player resource
		if className == "CDOTA_PlayerResource" {
			for i := 0; i < 10; i++ {
				if kills, ok := e.GetInt32(fmt.Sprintf("m_vecPlayerTeamData.%04d.m_iKills", i)); ok {
					state.Players[i].Kills = int(kills)
				}
				if deaths, ok := e.GetInt32(fmt.Sprintf("m_vecPlayerTeamData.%04d.m_iDeaths", i)); ok {
					state.Players[i].Deaths = int(deaths)
				}
				if assists, ok := e.GetInt32(fmt.Sprintf("m_vecPlayerTeamData.%04d.m_iAssists", i)); ok {
					state.Players[i].Assists = int(assists)
				}
				if level, ok := e.GetInt32(fmt.Sprintf("m_vecPlayerTeamData.%04d.m_iLevel", i)); ok {
					state.Players[i].Level = int(level)
				}
				if steamID, ok := e.GetUint64(fmt.Sprintf("m_vecPlayerData.%04d.m_iPlayerSteamID", i)); ok {
					state.Players[i].SteamID = int64(steamID)
				}
				// Source 2 team enum: 2 = Radiant (DOTA_TEAM_GOODGUYS), 3 = Dire (DOTA_TEAM_BADGUYS).
				// player_id (m_vecPlayerData index) does NOT always match team membership —
				// must read this field per slot, not infer from `i < 5`.
				if team, ok := e.GetInt32(fmt.Sprintf("m_vecPlayerData.%04d.m_iPlayerTeam", i)); ok && team != 0 {
					state.Players[i].IsRadiant = team == 2
				}
				// m_nSelectedHeroID is int32 in older replays but uint32 (with
				// the value doubled) in newer ones — see decodeSelectedHeroID.
				if heroID, ok := decodeSelectedHeroID(e.Get(fmt.Sprintf("m_vecPlayerTeamData.%04d.m_nSelectedHeroID", i))); ok && heroID > 0 {
					state.Players[i].HeroID = heroID
				}
			}
		}

		// Track team data (per-minute updates)
		if className == "CDOTA_DataRadiant" || className == "CDOTA_DataDire" {
			for i := 0; i < 5; i++ {
				// Resolve team-slot → player_id via SteamID. Was hardcoded to baseIdx+i,
				// which silently swapped per-player networth/lh/denies/xp when player_id
				// ordering didn't align with team membership.
				steamID, ok := e.GetUint64(fmt.Sprintf("m_vecDataTeam.%04d.m_iPlayerSteamID", i))
				if !ok || steamID == 0 {
					continue
				}
				playerIdx := -1
				for j := 0; j < 10; j++ {
					if uint64(state.Players[j].SteamID) == steamID {
						playerIdx = j
						break
					}
				}
				if playerIdx < 0 {
					continue
				}
				ps := state.Players[playerIdx]

				if lh, ok := e.GetInt32(fmt.Sprintf("m_vecDataTeam.%04d.m_iLastHitCount", i)); ok {
					ps.LastHits = int(lh)
				}
				if denies, ok := e.GetInt32(fmt.Sprintf("m_vecDataTeam.%04d.m_iDenyCount", i)); ok {
					ps.Denies = int(denies)
				}
				if nw, ok := e.GetInt32(fmt.Sprintf("m_vecDataTeam.%04d.m_iNetWorth", i)); ok {
					ps.NetWorth = int(nw)
				}
				// Try to read entity-based XP (more accurate than combat log)
				if xp, ok := e.GetInt32(fmt.Sprintf("m_vecDataTeam.%04d.m_iTotalEarnedXP", i)); ok && xp > 0 {
					ps.XP = int(xp) // Override combat log XP with entity value
				}
				
				// Record per-minute snapshot (only after GameStartTime is known
				// and before game-end). LastMinute starts at -1 so the first
				// snapshot fires at gameMinute=0 (horn), aligning ours[i] with
				// OpenDota's *_t[i] at minute i.
				gameOver := state.GameEndTime > 0 && state.GameTime() > state.GameEndTime
				if gameMinute > ps.LastMinute && state.GameStartTime > 0 && !gameOver {
					ps.MinuteSnapshots = append(ps.MinuteSnapshots, MinuteSnapshot{
						Gold:   ps.Gold,
						XP:     ps.XP,
						LH:     ps.LastHits,
						Denies: ps.Denies,
						NW:     ps.NetWorth,
						Level:  ps.Level,
					})
					ps.LastMinute = gameMinute
					
					// Save 10-minute snapshot
					if gameMinute == 10 {
						ps.GoldAt10 = ps.Gold
						ps.XPAt10 = ps.XP
						ps.NWAt10 = ps.NetWorth
						ps.LevelAt10 = ps.Level
						ps.EntityXPAt10 = ps.XP // Will be entity XP if m_iTotalEarnedXP was found
					}
				}
			}

			// Sample all 10 hero positions at each minute boundary (for zone analysis)
			if gameMinute > state.LastSampledMinute && gameMinute > 0 && state.GameStartTime > 0 {
				sample := PositionSample{
					Minute:  gameMinute,
					Players: make([]PositionSamplePlayer, 0, 10),
				}
				for pi := 0; pi < 10; pi++ {
					pp := state.Players[pi]
					if pp.LastPosX != 0 || pp.LastPosY != 0 {
						sample.Players = append(sample.Players, PositionSamplePlayer{
							Idx: pi, X: pp.LastPosX, Y: pp.LastPosY,
						})
					}
				}
				state.PositionSamples = append(state.PositionSamples, sample)
				state.LastSampledMinute = gameMinute
			}
		}

		// Track wards
		if className == "CDOTA_NPC_Observer_Ward" || className == "CDOTA_NPC_Observer_Ward_TrueSight" {
			if op&manta.EntityOpCreated != 0 {
				wardType := 0
				if className == "CDOTA_NPC_Observer_Ward_TrueSight" {
					wardType = 1
				}
				wardTime := state.ActualGameSeconds(state.GameTime())

				playerIdx := -1
				if replayPlayerID, ok := e.GetUint32("m_nPlayerOwnerID"); ok {
					playerIdx = replayPlayerToIndex(replayPlayerID)
				}

				wardEvent := WardEvent{
					Time:     wardTime,
					Type:     wardType,
					PlayerID: playerIdx,
				}

				state.WardEvents = append(state.WardEvents, wardEvent)

				if playerIdx >= 0 && playerIdx < 10 {
					state.Players[playerIdx].Wards = append(state.Players[playerIdx].Wards, wardEvent)
					// Регистрируем для расчёта длительности на удалении (v4.3.0).
					state.ActiveWards[e.GetIndex()] = &activeWard{
						playerIdx: playerIdx,
						sliceIdx:  len(state.Players[playerIdx].Wards) - 1,
						start:     wardTime,
					}
				}
			}
		}

		// Track rune spawns and store entity info for pickup attribution
		if className == "CDOTA_Item_Rune" && op&manta.EntityOpCreated != 0 {
			runeTime := state.ActualGameSeconds(state.GameTime())
			runeType := 0
			if rt, ok := e.GetInt32("m_iRuneType"); ok {
				runeType = int(rt)
			}
			var runePosX, runePosY float64
			if cx, ok := e.GetUint64("CBodyComponent.m_cellX"); ok {
				runePosX = float64(cx)
			}
			if cy, ok := e.GetUint64("CBodyComponent.m_cellY"); ok {
				runePosY = float64(cy)
			}
			state.RuneSpawns = append(state.RuneSpawns, RuneEvent{
				Time:     runeTime,
				RuneType: runeType,
				Action:   0, // spawned
			})
			// Store for pickup attribution on destroy
			state.PendingRunes[e.GetIndex()] = &RuneEntityInfo{
				RuneType:  runeType,
				PosX:      runePosX,
				PosY:      runePosY,
				SpawnTime: runeTime,
			}
		}

		// Rune entity destroyed = picked up or expired. Power runes (DD,
		// Haste, Invis, Regen, Arcane, Shield) trigger modifier_rune_*
		// buffs and are already tracked above. Non-power runes (bounty,
		// water, illusion, type-8) don't apply persistent modifiers, so
		// we attribute them to the nearest hero at destroy time.
		if className == "CDOTA_Item_Rune" && op&manta.EntityOpDeleted != 0 {
			runeTime := state.ActualGameSeconds(state.GameTime())
			idx := e.GetIndex()
			if runeInfo, exists := state.PendingRunes[idx]; exists {
				nearestPlayer := findNearestHero(state, runeInfo.PosX, runeInfo.PosY)
				if nearestPlayer >= 0 {
					attribute := false
					switch runeInfo.RuneType {
					case 5: // bounty: confirm via gold_reason=17 within 5s
						attribute = hasBountyGoldNear(state, runeTime, nearestPlayer)
					case 2, 7, 8: // illusion, water, type-8: proximity-based
						dist := heroDistToPoint(state, nearestPlayer, runeInfo.PosX, runeInfo.PosY)
						attribute = dist < 40
					}
					if attribute {
						state.Players[nearestPlayer].Runes = append(state.Players[nearestPlayer].Runes, RuneEvent{
							Time:     runeTime,
							RuneType: runeInfo.RuneType,
							Action:   1, // pickup
						})
					}
				}
				delete(state.PendingRunes, idx)
			}
		}

		// Aegis-based Roshan detection removed — using combat log death handler instead

		// Track item entities for final items lookup (normalize to standard item_ names)
		if strings.HasPrefix(className, "CDOTA_Item_") && !strings.Contains(className, "Rune") {
			idx := e.GetIndex()
			rawName := strings.TrimPrefix(className, "CDOTA_Item_")
			state.ItemEntities[idx] = normalizeEntityItemName(rawName)
		}

		// Track ability entities for skill build detection
		if strings.HasPrefix(className, "CDOTA_Ability_") || strings.HasPrefix(className, "CDOTA_Item_Ability_") {
			idx := e.GetIndex()
			abilityName := className
			// Strip prefixes to get clean ability name
			abilityName = strings.TrimPrefix(abilityName, "CDOTA_Ability_")
			abilityName = strings.TrimPrefix(abilityName, "CDOTA_Item_Ability_")
			abilityName = strings.ToLower(abilityName)

			abilityLevel := 0
			if lvl, ok := e.GetInt32("m_iLevel"); ok {
				abilityLevel = int(lvl)
			}

			info, exists := state.AbilityEntities[idx]
			if !exists {
				state.AbilityEntities[idx] = &AbilityEntityInfo{Name: abilityName, Level: abilityLevel}
			} else {
				// Check for level change
				if abilityLevel > info.Level && abilityLevel > 0 {
					// Find which player owns this ability
					for pi := 0; pi < 10; pi++ {
						if prevLvl, ok := state.Players[pi].PrevAbilityLvls[abilityName]; ok && prevLvl < abilityLevel {
							state.Players[pi].SkillBuild = append(state.Players[pi].SkillBuild, SkillLevelUp{
								Time:        actualGameTime,
								AbilityName: abilityName,
								Level:       abilityLevel,
								HeroLevel:   state.Players[pi].Level,
							})
							state.Players[pi].PrevAbilityLvls[abilityName] = abilityLevel
							break
						} else if !ok {
							// Not yet tracked for this player — skip, will be assigned on first hero entity scan
						}
					}
				}
				info.Level = abilityLevel
				info.Name = abilityName
			}
		}

		return nil
	})

	log.Println("Starting parse...")
	if err := p.Start(); err != nil {
		log.Fatalf("Parse error: %v", err)
	}

	// Calculate duration from game start/end times (excludes pregame)
	duration := 0.0
	if state.GameEndTime > 0 && state.GameStartTime > 0 {
		duration = state.GameEndTime - state.GameStartTime
		log.Printf("Game duration from entity times: %.0fs (start=%.0f end=%.0f)", duration, state.GameStartTime, state.GameEndTime)
	}
	if duration <= 0 {
		duration = state.GameTime() // fallback to tick-based
		log.Printf("Using tick-based duration: %.0fs", duration)
	}

	// Make series[final_minute] reflect game-end values. Our regular minute
	// snapshot fires at the *first* tick after each minute boundary — so the
	// snapshot at minute N captures values at ~N:00, not the activity that
	// happened during minute N. OpenDota's *_t[N] is the cumulative value at
	// the *end* of minute N. Either replace the existing last snapshot with
	// current values, or append one if we never reached the final minute.
	finalMinute := int(duration / 60)
	finalSnap := func(ps *PlayerState) MinuteSnapshot {
		return MinuteSnapshot{Gold: ps.Gold, XP: ps.XP, LH: ps.LastHits, Denies: ps.Denies, NW: ps.NetWorth, Level: ps.Level}
	}
	for i := 0; i < 10; i++ {
		ps := state.Players[i]
		if len(ps.MinuteSnapshots) > finalMinute {
			ps.MinuteSnapshots[finalMinute] = finalSnap(ps)
			ps.MinuteSnapshots = ps.MinuteSnapshots[:finalMinute+1]
		} else {
			ps.MinuteSnapshots = append(ps.MinuteSnapshots, finalSnap(ps))
		}
	}

	match := buildMatchOutput(state, duration)

	jsonData, err := json.MarshalIndent(match, "", "  ")
	if err != nil {
		log.Fatalf("Failed to marshal JSON: %v", err)
	}

	fmt.Println(string(jsonData))
	log.Printf("Parse complete. Duration: %.0f seconds, Match ID: %d", duration, state.MatchID)
}

// ============= HELPERS =============

// respawnTime returns approximate respawn time in seconds for a given hero level.
// Based on Dota 2 respawn time table (patch 7.37+). Does not account for talents/Bloodstone.
func respawnTime(level int) int {
	if level <= 0 {
		return 6
	}
	if level >= 30 {
		return 100
	}
	// Index 0 unused, levels 1-30
	table := []int{0, 6, 8, 10, 14, 16, 26, 28, 30, 32, 34, 36, 46, 48, 50, 52, 54, 56, 66, 70, 74, 78, 82, 86, 90, 100, 100, 100, 100, 100, 100}
	return table[level]
}

func heroNameToPlayerIndex(combatLogName string, state *ParserState) int {
	name := strings.TrimPrefix(combatLogName, "npc_dota_hero_")
	heroID := heroNameStringToID(name)

	for i := 0; i < 10; i++ {
		if state.Players[i].HeroID == heroID {
			return i
		}
	}
	return -1
}

// heroNpcNameToID maps npc_dota_hero_* suffixes (combat log) and normalized
// CDOTA_Unit_Hero_* class suffixes to hero IDs.
var heroNpcNameToID = map[string]int{
		// Combat log format
		"antimage": 1, "axe": 2, "bane": 3, "bloodseeker": 4, "crystal_maiden": 5,
		"drow_ranger": 6, "earthshaker": 7, "juggernaut": 8, "mirana": 9, "morphling": 10,
		"nevermore": 11, "phantom_lancer": 12, "puck": 13, "pudge": 14, "razor": 15,
		"sand_king": 16, "storm_spirit": 17, "sven": 18, "tiny": 19, "vengefulspirit": 20,
		"windrunner": 21, "zuus": 22, "kunkka": 23, "lina": 25, "lion": 26,
		"shadow_shaman": 27, "slardar": 28, "tidehunter": 29, "witch_doctor": 30,
		"lich": 31, "riki": 32, "enigma": 33, "tinker": 34, "sniper": 35,
		"necrolyte": 36, "warlock": 37, "beastmaster": 38, "queenofpain": 39, "venomancer": 40,
		"faceless_void": 41, "skeleton_king": 42, "death_prophet": 43, "phantom_assassin": 44,
		"pugna": 45, "templar_assassin": 46, "viper": 47, "luna": 48, "dragon_knight": 49,
		"dazzle": 50, "rattletrap": 51, "leshrac": 52, "furion": 53, "life_stealer": 54,
		"dark_seer": 55, "clinkz": 56, "omniknight": 57, "enchantress": 58, "huskar": 59,
		"night_stalker": 60, "broodmother": 61, "bounty_hunter": 62, "weaver": 63, "jakiro": 64,
		"batrider": 65, "chen": 66, "spectre": 67, "ancient_apparition": 68, "doom_bringer": 69,
		"ursa": 70, "spirit_breaker": 71, "gyrocopter": 72, "alchemist": 73, "invoker": 74,
		"silencer": 75, "obsidian_destroyer": 76, "lycan": 77, "brewmaster": 78, "shadow_demon": 79,
		"lone_druid": 80, "chaos_knight": 81, "meepo": 82, "treant": 83, "ogre_magi": 84,
		"undying": 85, "rubick": 86, "disruptor": 87, "nyx_assassin": 88, "naga_siren": 89,
		"keeper_of_the_light": 90, "wisp": 91, "visage": 92, "slark": 93, "medusa": 94,
		"troll_warlord": 95, "centaur": 96, "magnataur": 97, "shredder": 98,
		"bristleback": 99, "tusk": 100, "skywrath_mage": 101, "abaddon": 102, "elder_titan": 103,
		"legion_commander": 104, "techies": 105, "ember_spirit": 106, "earth_spirit": 107,
		"abyssal_underlord": 108, "terrorblade": 109, "phoenix": 110, "oracle": 111, "winter_wyvern": 112,
		"arc_warden": 113, "monkey_king": 114, "dark_willow": 119, "pangolier": 120,
		"grimstroke": 121, "hoodwink": 123, "void_spirit": 126, "snapfire": 128, "mars": 129,
		"dawnbreaker": 135, "marci": 136, "primal_beast": 137, "muerta": 138, "ringmaster": 131,
		"kez": 145, "largo": 155,

		// Class name format (normalized)
		"shadowshaman": 27, "spiritbreaker": 71, "obsidiandestroyer": 76,
		"phantomlancer": 12, "phantomassassin": 44, "dragonknight": 49,
		"sandking": 16, "stormspirit": 17, "drowranger": 6, "crystalmaiden": 5,
		"shadowfiend": 11, "witchdoctor": 30, "deathprophet": 43, "lifestealer": 54,
		"darkseer": 55, "nightstalker": 60, "bountyhunter": 62, "ancientapparition": 68,
		"doombringer": 69, "chaosknight": 81, "trollwarlord": 95, "centaurwarrunner": 96,
		"skywrathmage": 101, "eldertitan": 103, "legioncommander": 104,
		"emberspirit": 106, "earthspirit": 107, "keeperofthelight": 90,
		"voidspirit": 126, "monkeyking": 114, "darkwillow": 119, "primalbeast": 137,
		"winterwyvern": 112, "arcwarden": 113, "abyssalunderlord": 108,
		"treantprotector": 83, "templarassassin": 46, "naturesprophet": 53,
		"outworlddestroyer": 76, "outworlddevourer": 76, "wraithking": 42, "io": 91,
		"ogremagi": 84,
}

// heroNpcNameToIDNoSep indexes the same IDs by name with separators removed,
// derived from the canonical map so entity class suffixes like "FacelessVoid"
// resolve without hand-maintained aliases (the alias list above had holes —
// e.g. Faceless Void resolved to 0 in match 8824123966).
var heroNpcNameToIDNoSep = func() map[string]int {
	m := make(map[string]int, len(heroNpcNameToID))
	for k, v := range heroNpcNameToID {
		m[strings.ReplaceAll(k, "_", "")] = v
	}
	return m
}()

func heroNameStringToID(name string) int {
	name = strings.ToLower(name)
	// Try with underscores first (matches combat log names like "shadow_demon")
	if id, ok := heroNpcNameToID[name]; ok {
		return id
	}
	// Try without underscores/spaces (matches entity class names like "ShadowDemon")
	name = strings.ReplaceAll(name, "_", "")
	name = strings.ReplaceAll(name, " ", "")

	if id, ok := heroNpcNameToID[name]; ok {
		return id
	}
	// Derived no-separator index covers multi-word names without explicit aliases
	if id, ok := heroNpcNameToIDNoSep[name]; ok {
		return id
	}

	return 0
}

// detectLane classifies a hero's lane during the 1–10 minute laning phase
// based on average map cell position. Source 2 cell coords: X grows east,
// Y grows north. Radiant base is at the SW corner (~64,64), Dire base at
// the NE corner (~192,192).
//
// Lane geometry (during laning phase, players stand near these areas):
//   - bottom lane (south edge):  low Y,  X ~80–140  → radiant safe / dire off
//   - top lane (west edge):      low X,  Y ~80–140  → radiant off / dire safe
//   - mid:                       on diagonal X≈Y around 110–150
func detectLane(positions []struct{ X, Y float64 }, isRadiant bool) string {
	if len(positions) < 100 {
		return "unknown"
	}
	// Skip first 20% (early movement out of fountain) and avg over the rest.
	start := len(positions) / 5
	var sumX, sumY float64
	n := 0
	for i := start; i < len(positions); i++ {
		sumX += positions[i].X
		sumY += positions[i].Y
		n++
	}
	if n == 0 {
		return "unknown"
	}
	avgX := sumX / float64(n)
	avgY := sumY / float64(n)

	// Bottom lane corridor (Y is small, X varies 60..180).
	bottomLane := avgY < 110 && avgX >= 60 && avgX <= 200
	// Top lane corridor (X is small, Y varies 60..180).
	topLane := avgX < 110 && avgY >= 60 && avgY <= 200
	// Mid lane: along the diagonal X≈Y, in the central band.
	midLane := math.Abs(avgX-avgY) < 30 && avgX >= 100 && avgX <= 170

	// Mid takes priority because mid corridor partly overlaps
	// jungle/transition zones.
	if midLane {
		return "mid"
	}
	if bottomLane {
		if isRadiant {
			return "safe"
		}
		return "off"
	}
	if topLane {
		if isRadiant {
			return "off"
		}
		return "safe"
	}
	// Far from any lane corridor → likely jungling or roaming between lanes.
	if avgX > 110 && avgX < 180 && avgY > 110 && avgY < 180 {
		return "jungle"
	}
	return "roam"
}

// detectLanePartner finds the hero (player index 0..9) on the same team
// that this hero spent most of the laning phase next to. Uses the
// minute-snapshot position samples shared between players. Returns -1 if
// no clear partner.
//
// "next to" = within 30 cells. We count overlapping positions per minute
// snapshot; the partner is the teammate with the highest match count.
func detectLanePartner(state *ParserState, playerIdx int) int {
	if playerIdx < 0 || playerIdx >= 10 {
		return -1
	}
	me := state.Players[playerIdx]
	if len(me.LanePositions) < 30 {
		return -1
	}
	bestPartner := -1
	bestScore := 0
	for j := 0; j < 10; j++ {
		if j == playerIdx {
			continue
		}
		other := state.Players[j]
		if other.IsRadiant != me.IsRadiant {
			continue
		}
		if len(other.LanePositions) < 30 {
			continue
		}
		// Position arrays have one entry per server tick during 1-10 min.
		// Lengths may differ slightly; use the shorter.
		n := len(me.LanePositions)
		if len(other.LanePositions) < n {
			n = len(other.LanePositions)
		}
		score := 0
		for k := 0; k < n; k++ {
			dx := me.LanePositions[k].X - other.LanePositions[k].X
			dy := me.LanePositions[k].Y - other.LanePositions[k].Y
			if dx*dx+dy*dy < 30*30 { // within 30 cells (squared comparison)
				score++
			}
		}
		// Require at least 30% overlap to consider as partners.
		if score > bestScore && score > n/3 {
			bestScore = score
			bestPartner = j
		}
	}
	return bestPartner
}

// assignPositions classifies each player to a Dota position 1-5:
//   1 = hard carry  (highest NW core on safe lane)
//   2 = mid         (solo player on mid)
//   3 = offlaner    (highest NW core on off lane)
//   4 = soft support
//   5 = hard support
//
// Algorithm:
//  1. Group players by team and detected lane.
//  2. Mid solo = pos 2.
//  3. On safe/off, the higher-NW@10 player is core (1 or 3),
//     the other is hard/soft support (5 or 4).
//  4. Roamers default to pos 4 (soft support / roaming gank role).
//
// Returns map[playerIdx]position.
func assignPositions(state *ParserState, lanes map[int]string) map[int]int {
	pos := make(map[int]int, 10)
	// Bucket per team+lane
	type bucket struct{ idxs []int }
	buckets := map[string]*bucket{}
	for i := 0; i < 10; i++ {
		side := "rad"
		if !state.Players[i].IsRadiant {
			side = "dire"
		}
		key := side + ":" + lanes[i]
		if buckets[key] == nil {
			buckets[key] = &bucket{}
		}
		buckets[key].idxs = append(buckets[key].idxs, i)
	}
	for key, b := range buckets {
		// Sort by NW@10 desc — highest farm goes to core position.
		sort.Slice(b.idxs, func(a, c int) bool {
			return state.Players[b.idxs[a]].NWAt10 > state.Players[b.idxs[c]].NWAt10
		})
		switch {
		case strings.HasSuffix(key, ":mid"):
			for _, idx := range b.idxs {
				pos[idx] = 2
			}
		case strings.HasSuffix(key, ":safe"):
			// Highest NW = pos 1, lowest = pos 5.
			for k, idx := range b.idxs {
				if k == 0 {
					pos[idx] = 1
				} else {
					pos[idx] = 5
				}
			}
		case strings.HasSuffix(key, ":off"):
			for k, idx := range b.idxs {
				if k == 0 {
					pos[idx] = 3
				} else {
					pos[idx] = 4
				}
			}
		default:
			// Roamers / jungle / unknown — default to pos 4.
			for _, idx := range b.idxs {
				pos[idx] = 4
			}
		}
	}
	return pos
}

// xpmAtTen returns XP per minute at 10 minutes using the best available source:
// 1. Entity XP (m_iTotalEarnedXP) if available
// 2. Combat log accumulated XP
// 3. Level-based XP estimation as fallback
func xpmAtTen(ps *PlayerState) int {
	// Prefer entity XP if captured at minute 10
	if ps.EntityXPAt10 > 0 {
		return ps.EntityXPAt10 / 10
	}
	// Combat log XP (may undercount)
	if ps.XPAt10 > 0 {
		return ps.XPAt10 / 10
	}
	// Level-based fallback
	if ps.LevelAt10 > 0 && ps.LevelAt10 < len(xpForLevel) {
		return xpForLevel[ps.LevelAt10] / 10
	}
	return 0
}

// genericAbilities is a set of abilities shared by all heroes that should not appear in skill build
var genericAbilities = map[string]bool{
	"capture":                       true,
	"twin_gate_portal_warp":         true,
	"abyssalunderlord_portal_warp":  true,
	"lamp_use":                      true,
	"plus_highfive":                 true,
	"plus_guildbanner":              true,
	"generic_hidden":                true,
	"neutral_upgrade":               true,
	"special_bonus_base":            true,
}

// filterSkillBuild removes generic/shared abilities and sorts by time
func filterSkillBuild(raw []SkillLevelUp) []SkillLevelUp {
	result := make([]SkillLevelUp, 0, len(raw))
	// Deduplicate: same ability+level = keep only first occurrence
	seen := make(map[string]bool)
	for _, s := range raw {
		if genericAbilities[s.AbilityName] {
			continue
		}
		key := fmt.Sprintf("%s_%d", s.AbilityName, s.Level)
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, s)
	}
	// Sort by time
	sort.Slice(result, func(i, j int) bool {
		return result[i].Time < result[j].Time
	})
	return result
}

// talentExcludedNames are special_bonus_* abilities that are not real
// talent-tree choices (shared stat/placeholder abilities present on heroes).
var talentExcludedNames = map[string]bool{
	"special_bonus_attributes": true,
	"special_bonus_base":       true,
}

// buildTalents converts observed talent slots (ability slot index → info)
// into the list of chosen talents (ability level >= 1). Hero ability lists
// carry the 8 talents in tier order (two per tier: 10/10/15/15/20/20/25/25),
// so when exactly 8 talent slots are present the tier is derived from the
// pair position. Otherwise Level is left 0 (omitted in JSON) and chosen
// talents are listed in slot order.
func buildTalents(slots map[int]AbilityEntityInfo) []Talent {
	keys := make([]int, 0, len(slots))
	for k := range slots {
		if talentExcludedNames[slots[k].Name] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Ints(keys)

	tiers := []int{10, 10, 15, 15, 20, 20, 25, 25}
	deriveTier := len(keys) == len(tiers)

	talents := make([]Talent, 0, 4)
	for pos, k := range keys {
		info := slots[k]
		if info.Level < 1 {
			continue
		}
		t := Talent{Name: info.Name}
		if deriveTier {
			t.Level = tiers[pos]
		}
		talents = append(talents, t)
	}
	return talents
}

// runeModifierToType maps modifier names to rune type IDs
var runeModifierToType = map[string]int{
	"modifier_rune_doubledamage": 0,
	"modifier_rune_haste":        1,
	"modifier_rune_illusion":     2,
	"modifier_rune_invis":        3,
	"modifier_rune_regen":        4,
	"modifier_rune_arcane":       6,
	"modifier_rune_shield":       9,
}

// buildRuneSummary computes rune pickup summary from rune events (action=1 only)
func buildRuneSummary(runes []RuneEvent) RuneSummary {
	var s RuneSummary
	for _, r := range runes {
		if r.Action != 1 { // only pickups
			continue
		}
		s.Total++
		switch r.RuneType {
		case 0: // DD
			s.DoubleDamage++
			s.PowerRunes++
		case 1: // Haste
			s.Haste++
			s.PowerRunes++
		case 2: // Illusion
			s.Illusion++
			s.PowerRunes++
		case 3: // Invis
			s.Invis++
			s.PowerRunes++
		case 4: // Regen
			s.Regen++
			s.PowerRunes++
		case 5: // Bounty
			s.BountyRunes++
		case 6: // Arcane
			s.Arcane++
			s.PowerRunes++
		case 7: // Water
			s.WaterRunes++
		case 8: // Type 8 — identity unverified in current patch; see RuneSummary doc
			s.Rune8++
		case 9: // Shield
			s.Shield++
			s.PowerRunes++
		}
	}
	return s
}

// detectTeamfights groups hero deaths within a 20-second window into teamfights.
// Requires minimum 3 deaths to qualify as a teamfight.
func detectTeamfights(state *ParserState) []Teamfight {
	type deathInfo struct {
		Time      float64
		PlayerIdx int
		IsRadiant bool
		Killer    int
	}

	// Collect all hero deaths into a single timeline
	var allDeaths []deathInfo
	for i := 0; i < 10; i++ {
		for _, d := range state.Players[i].DeathEvents {
			allDeaths = append(allDeaths, deathInfo{
				Time:      d.Time,
				PlayerIdx: i,
				IsRadiant: state.Players[i].IsRadiant,
				Killer:    d.Killer,
			})
		}
	}

	// Sort by time
	sort.Slice(allDeaths, func(a, b int) bool {
		return allDeaths[a].Time < allDeaths[b].Time
	})

	const window = 20.0  // seconds between deaths to group as same fight
	const minDeaths = 3  // minimum deaths to qualify as teamfight

	var teamfights []Teamfight
	i := 0
	for i < len(allDeaths) {
		cluster := []deathInfo{allDeaths[i]}
		j := i + 1
		for j < len(allDeaths) && allDeaths[j].Time-cluster[len(cluster)-1].Time <= window {
			cluster = append(cluster, allDeaths[j])
			j++
		}

		if len(cluster) >= minDeaths {
			radDeaths, direDeaths := 0, 0
			var deaths []TeamfightDeath
			for _, d := range cluster {
				if d.IsRadiant {
					radDeaths++
				} else {
					direDeaths++
				}
				deaths = append(deaths, TeamfightDeath{
					Time:      d.Time,
					PlayerID:  d.PlayerIdx,
					IsRadiant: d.IsRadiant,
					Killer:    d.Killer,
				})
			}

			winner := "even"
			if radDeaths < direDeaths {
				winner = "radiant"
			} else if direDeaths < radDeaths {
				winner = "dire"
			}

			teamfights = append(teamfights, Teamfight{
				StartTime:     cluster[0].Time,
				EndTime:       cluster[len(cluster)-1].Time,
				Duration:      cluster[len(cluster)-1].Time - cluster[0].Time,
				RadiantDeaths: radDeaths,
				DireDeaths:    direDeaths,
				Winner:        winner,
				Deaths:        deaths,
			})
		}
		i = j
	}
	return teamfights
}

func buildMatchOutput(state *ParserState, duration float64) Match {
	players := make([]Player, 10)

	// Three-pass build: pass 1 classifies lane per player (geometry),
	// pass 2 reconciles via lane partners (if my partner is on a "stronger"
	// lane I'm probably with them — supports who roam through mid get
	// pulled back to their core's lane), pass 3 assigns positions 1-5.
	lanes := make(map[int]string, 10)
	for i := 0; i < 10; i++ {
		lanes[i] = detectLane(state.Players[i].LanePositions, state.Players[i].IsRadiant)
	}
	partners := make(map[int]int, 10)
	for i := 0; i < 10; i++ {
		partners[i] = detectLanePartner(state, i)
	}
	// Lane reconcile: prefer partner's lane if it's safe/off (more reliable
	// than mid for support detection — supports often pass through mid).
	for i := 0; i < 10; i++ {
		p := partners[i]
		if p < 0 {
			continue
		}
		other := lanes[p]
		if other == "safe" || other == "off" {
			lanes[i] = other
		}
	}
	positions := assignPositions(state, lanes)

	for i := 0; i < 10; i++ {
		ps := state.Players[i]

		gpm := 0
		xpm := 0
		if duration > 0 {
			// gpm uses NetWorth/minutes as a proxy. The "true" total-gold-
			// earned isn't available without reconstructing from combat log
			// gold events with reason filtering — matching OpenDota's
			// gold_per_min exactly would require reverse-engineering their
			// reason allowlist. Currently within ~10% of OpenDota; consumers
			// that need higher precision should compute from `goldPerMinute[]`
			// (cumulative current cash) or pull gold_per_min from OpenDota
			// directly. See deriveGpm() in src/lib/games.ts (Next.js).
			gpm = int(float64(ps.NetWorth) / (duration / 60.0))
			xpm = int(float64(ps.XP) / (duration / 60.0))
		}

		// Build per-minute arrays
		var goldPM, xpPM, lhPM, denyPM, nwPM, levelPM []int
		for _, snap := range ps.MinuteSnapshots {
			goldPM = append(goldPM, snap.Gold)
			xpPM = append(xpPM, snap.XP)
			lhPM = append(lhPM, snap.LH)
			denyPM = append(denyPM, snap.Denies)
			nwPM = append(nwPM, snap.NW)
			levelPM = append(levelPM, snap.Level)
		}

		// v4.4.0 timelines pass through as-is (already time-ordered)
		abilityCastEvents := ps.AbilityCastEvents
		itemCastEvents := ps.ItemCastEvents
		nearDeathEvents := ps.NearDeathEvents

		// Build ability cast report
		var abilityCasts []AbilityCast
		for name, count := range ps.AbilityCasts {
			abilityCasts = append(abilityCasts, AbilityCast{
				AbilityName: name,
				Count:       count,
			})
		}

		// Build damage report
		var damageReport []DamageTarget
		for _, dmg := range ps.DamageByTarget {
			damageReport = append(damageReport, *dmg)
		}

		// Build item usage report (map → sorted slice)
		var itemUsed []ItemUsed
		for name, count := range ps.ItemUsage {
			itemUsed = append(itemUsed, ItemUsed{
				ItemName: name,
				ItemID:   itemNameToID[name],
				Count:    count,
			})
		}
		sort.Slice(itemUsed, func(a, b int) bool { return itemUsed[a].Count > itemUsed[b].Count })

		// Build damage received report
		var damageReceivedReport *DamageReceivedReport
		if ps.DamageReceivedPhysical > 0 || ps.DamageReceivedMagical > 0 || ps.DamageReceivedPure > 0 {
			damageReceivedReport = &DamageReceivedReport{
				PhysicalDamage: ps.DamageReceivedPhysical,
				MagicalDamage:  ps.DamageReceivedMagical,
				PureDamage:     ps.DamageReceivedPure,
			}
		}

		// Lane (from pass-1 detection above)
		lane := lanes[i]
		laneInt := 0
		switch lane {
		case "safe":
			laneInt = 1
		case "mid":
			laneInt = 2
		case "off":
			laneInt = 3
		case "jungle":
			laneInt = 4
		}

		// Position 1-5 from pass-2 assignment.
		position := positions[i]
		if position == 0 {
			position = 4 // fallback
		}
		// Role: 0 = core (positions 1, 2, 3), 1 = support (positions 4, 5).
		role := 0
		if position >= 4 {
			role = 1
		}

		stats := &PlayerStats{
			GoldPerMinute:       goldPM,
			ExperiencePerMinute: xpPM,
			LastHitsPerMinute:   lhPM,
			DeniesPerMinute:     denyPM,
			NetworthPerMinute:   nwPM,
			Level:               levelPM,
			KillEvents:           ps.KillEvents,
			DeathEvents:          ps.DeathEvents,
			AssistEvents:         ps.AssistEvents,
			ItemPurchases:        ps.ItemPurchases,
			Wards:                ps.Wards,
			Runes:                ps.Runes,
			AbilityCastEvents:    abilityCastEvents,
			ItemCastEvents:       itemCastEvents,
			NearDeathEvents:      nearDeathEvents,
			AbilityCastReport:    abilityCasts,
			HeroDamageReport:     damageReport,
			DamageReceivedReport: damageReceivedReport,
			StunDurationDealt:    ps.StunDurationDealt,
			SkillBuild:           filterSkillBuild(ps.SkillBuild),
			ItemUsed:             itemUsed,
			CampStacks:           ps.CampStacks,
			CampsStacked:         ps.CampsStacked,
			CreepsStacked:        ps.CreepsStacked,
			LaneStats: LaneStats{
				Lane:          lane,
				LanePartner:   partners[i],
				ReaggroCount:  ps.LaneHarassCount, // Lane harass events as aggro proxy
				DeathsPreTen:  ps.LaneDeaths,
				LaneDamageTakenPre5: ps.LaneDamageTakenPre5,
				KillsPreTen:   ps.LaneKills,
				AssistsPreTen: ps.LaneAssists,
				NetWorthAtTen: ps.NWAt10,
				LevelAtTen:    ps.LevelAt10,
				GPMAtTen:      ps.NWAt10 / 10,
				XPMAtTen:      xpmAtTen(ps),
			},
			VisionStats: VisionExposure{
				SmokeUsageCount: ps.SmokeCount,
				WardsDewarded:   ps.WardsDewarded,
			},
			RuneStats: buildRuneSummary(ps.Runes),
			TPCount:   ps.TPCount,
			CreepKills: CreepKillPhases{
				LaneCreepsPre10:    ps.LaneCreepsPre10,
				LaneCreeps10to25:   ps.LaneCreeps10to25,
				LaneCreeps25Plus:   ps.LaneCreeps25Plus,
				JungleCreepsPre10:  ps.JungleCreepsPre10,
				JungleCreeps10to25: ps.JungleCreeps10to25,
				JungleCreeps25Plus: ps.JungleCreeps25Plus,
				TotalLaneCreeps:    ps.LaneCreepKills,
				TotalJungleCreeps:  ps.JungleCreepKills,
			},
		}

		players[i] = Player{
			SteamAccountID:      ps.SteamID,
			HeroID:              ps.HeroID,
			HeroName:            getHeroName(ps.HeroID),
			IsRadiant:           ps.IsRadiant,
			IsVictory:           (ps.IsRadiant && state.RadiantWin) || (!ps.IsRadiant && !state.RadiantWin),
			Kills:               ps.Kills,
			Deaths:              ps.Deaths,
			Assists:             ps.Assists,
			Networth:            ps.NetWorth,
			GoldPerMinute:       gpm,
			ExperiencePerMinute: xpm,
			NumLastHits:         ps.LastHits,
			NumDenies:           ps.Denies,
			Level:               ps.Level,
			HeroDamage:          ps.HeroDamage,
			TowerDamage:         ps.TowerDamage,
			HeroHealing:         ps.HeroHealing,
			Lane:                laneInt,
			Role:                role,
			Position:            position,
			Item0ID:             itemNameToID[ps.FinalItems[0]],
			Item1ID:             itemNameToID[ps.FinalItems[1]],
			Item2ID:             itemNameToID[ps.FinalItems[2]],
			Item3ID:             itemNameToID[ps.FinalItems[3]],
			Item4ID:             itemNameToID[ps.FinalItems[4]],
			Item5ID:             itemNameToID[ps.FinalItems[5]],
			Backpack0:           ps.Backpack[0],
			Backpack1:           ps.Backpack[1],
			Backpack2:           ps.Backpack[2],
			Neutral0ID:          itemNameToID[ps.FinalNeutral],
			Item0Name:           ps.FinalItems[0],
			Item1Name:           ps.FinalItems[1],
			Item2Name:           ps.FinalItems[2],
			Item3Name:           ps.FinalItems[3],
			Item4Name:           ps.FinalItems[4],
			Item5Name:           ps.FinalItems[5],
			NeutralName:         ps.FinalNeutral,
			Talents:             buildTalents(ps.TalentSlots),
			Stats:               stats,
		}
	}

	// Sort item purchases by time
	for i := range players {
		if players[i].Stats != nil {
			sort.Slice(players[i].Stats.ItemPurchases, func(a, b int) bool {
				return players[i].Stats.ItemPurchases[a].Time < players[i].Stats.ItemPurchases[b].Time
			})
		}
	}

	// Calculate networth leads (use real team membership, not player_id index)
	var nwLeads, xpLeads []int
	maxMinutes := int(duration / 60)
	for m := 0; m < maxMinutes; m++ {
		var radNW, direNW, radXP, direXP int
		for i := 0; i < 10; i++ {
			if m < len(players[i].Stats.NetworthPerMinute) {
				if players[i].IsRadiant {
					radNW += players[i].Stats.NetworthPerMinute[m]
					radXP += players[i].Stats.ExperiencePerMinute[m]
				} else {
					direNW += players[i].Stats.NetworthPerMinute[m]
					direXP += players[i].Stats.ExperiencePerMinute[m]
				}
			}
		}
		nwLeads = append(nwLeads, radNW-direNW)
		xpLeads = append(xpLeads, radXP-direXP)
	}

	// Horn wall-clock: replay-start wall-clock + (hornTick × tickInterval).
	// Only emit if we captured both endpoints; otherwise 0 and the field is
	// omitempty-elided so consumers can detect "unknown" cleanly.
	var hornDateTime int64
	if state.StartTime > 0 && state.HornTick > 0 && state.TickInterval > 0 {
		hornDateTime = state.StartTime + int64(float64(state.HornTick)*float64(state.TickInterval))
	}

	// Fill WallStart of each pause from its start tick (parallel slice).
	if state.StartTime > 0 && state.TickInterval > 0 {
		for i := range state.Pauses {
			if i < len(state.PauseStartTicks) {
				state.Pauses[i].WallStart = state.StartTime + int64(float64(state.PauseStartTicks[i])*float64(state.TickInterval))
			}
		}
	}

	return Match{
		ID:                     state.MatchID,
		GameMode:               state.GameMode,
		LobbyType:              state.LobbyType,
		DidRadiantWin:          state.RadiantWin,
		DurationSeconds:        int(duration),
		StartDateTime:          state.StartTime,
		HornDateTime:           hornDateTime,
		Pauses:                 state.Pauses,
		RadiantNetworthLeads:   nwLeads,
		RadiantExperienceLeads: xpLeads,
		RoshanKills:            state.RoshanKills,
		AegisEvents:            state.AegisEvents,
		Buybacks:               state.Buybacks,
		RuneSpawns:             state.RuneSpawns,
		BuildingKills:          state.BuildingKills,
		Teamfights:             detectTeamfights(state),
		PositionSamples:        state.PositionSamples,
		SmokeEvents:            detectSmokeEvents(state),
		Players:                players,
		ParsedFromReplay:       true,
		ParserVersion:          "4.4.3",
	}
}

// detectSmokeEvents группирует raw modifier_smoke_of_deceit events в командные
// смоки: ≥4 героев одной стороны в окне 1.5s. Для каждой группы — outcome за
// 60s: kills/deaths участников, towers упавшие на стороне противника от
// участников, runes подобранные участниками.
func detectSmokeEvents(state *ParserState) []SmokeEvent {
	if len(state.SmokeModifierAdds) == 0 {
		return nil
	}
	// Сортируем по времени.
	raw := make([]SmokeModifierAdd, len(state.SmokeModifierAdds))
	copy(raw, state.SmokeModifierAdds)
	sort.Slice(raw, func(i, j int) bool { return raw[i].Time < raw[j].Time })

	// Какая сторона у игрока (radiant)? IsRadiant в Player итоговом — но он не
	// готов на момент detectSmokeEvents. PlayerState хранит TeamNumber (2=rad,3=dire)?
	// Проще: state.Players[idx] *PlayerState — у него уже есть IsRadiant установлен.
	playerIsRadiant := func(idx int) bool {
		if idx < 0 || idx >= 10 || state.Players[idx] == nil {
			return false
		}
		return state.Players[idx].IsRadiant
	}

	// v4.3.1: rolling-gap кластеризация по стороне. Смок-применения у команды
	// приходят ступенчато (игроки жмут смок не идеально синхронно), поэтому
	// мерджим add в открытый кластер той же стороны, если он в пределах gap от
	// ПОСЛЕДНЕГО add (а не от старта). Раздельные смоки (>gap друг от друга)
	// дают разные кластеры. Порог участников снижен до 2 — дуо/трио-смоки это
	// тоже координированный смок-мув. Старый порог (≥4 в окне 1.5с от старта)
	// ловил только полнокомандные синхронные смоки и кратно занижал счёт.
	const gapWindowSec = 4.0
	const outcomeWindowSec = 60.0
	const minParticipants = 2

	type cluster struct {
		startTime    float64
		isRadiant    bool
		participants map[int]struct{}
	}
	var clusters []cluster
	open := [2]int{-1, -1}        // индекс открытого кластера: [0]=radiant, [1]=dire
	lastTime := [2]float64{0, 0}  // время последнего add в открытом кластере
	for _, ev := range raw {
		s := 1
		if playerIsRadiant(ev.PlayerIdx) {
			s = 0
		}
		if open[s] >= 0 && ev.Time-lastTime[s] <= gapWindowSec {
			clusters[open[s]].participants[ev.PlayerIdx] = struct{}{}
		} else {
			clusters = append(clusters, cluster{
				startTime:    ev.Time,
				isRadiant:    s == 0,
				participants: map[int]struct{}{ev.PlayerIdx: {}},
			})
			open[s] = len(clusters) - 1
		}
		lastTime[s] = ev.Time
	}

	// Эмитим только кластеры с ≥minParticipants. Для каждого считаем outcome.
	var out []SmokeEvent
	for _, c := range clusters {
		if len(c.participants) < minParticipants {
			continue
		}
		parts := make([]int, 0, len(c.participants))
		for idx := range c.participants {
			parts = append(parts, idx)
		}
		sort.Ints(parts)
		partSet := c.participants

		windowEnd := c.startTime + outcomeWindowSec
		kills, deaths, towers, runes := 0, 0, 0, 0

		// Kills BY смокеров: смотрим Stats.KillEvents игроков-участников.
		for idx := range partSet {
			if state.Players[idx] == nil {
				continue
			}
			for _, k := range state.Players[idx].KillEvents {
				if k.Time >= c.startTime && k.Time <= windowEnd {
					kills++
				}
			}
			for _, d := range state.Players[idx].DeathEvents {
				if d.Time >= c.startTime && d.Time <= windowEnd {
					deaths++
				}
			}
			for _, r := range state.Players[idx].Runes {
				if r.Action == 1 && r.Time >= c.startTime && r.Time <= windowEnd {
					runes++
				}
			}
		}

		// Towers: BuildingKills где IsRadiant противника, killer в участниках, время в окне.
		for _, b := range state.BuildingKills {
			if b.Time < c.startTime || b.Time > windowEnd {
				continue
			}
			// Только вражеские здания (противоположная сторона).
			if b.IsRadiant == c.isRadiant {
				continue
			}
			if _, ok := partSet[b.KillerPlayer]; !ok {
				continue
			}
			towers++
		}

		out = append(out, SmokeEvent{
			GameTime:     c.startTime,
			Participants: parts,
			IsRadiant:    c.isRadiant,
			Outcome: SmokeOutcome{
				KillsWithin60s:  kills,
				DeathsWithin60s: deaths,
				TowersWithin60s: towers,
				RunesWithin60s:  runes,
			},
		})
	}
	return out
}

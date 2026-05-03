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

type DamageTarget struct {
	Target         int `json:"target"`
	PhysicalDamage int `json:"physicalDamage"`
	MagicalDamage  int `json:"magicalDamage"`
	PureDamage     int `json:"pureDamage"`
}

// EXTRA: Vision/stealth metrics
type VisionExposure struct {
	SmokeUsageCount int `json:"smokeUsageCount"` // Times used smoke of deceit
}

// Rune pickup summary per player
type RuneSummary struct {
	Total        int `json:"total"`                  // all rune pickups
	PowerRunes   int `json:"powerRunes"`             // DD, Haste, Invis, Regen, Arcane, Shield, Illusion
	BountyRunes  int `json:"bountyRunes"`            // bounty rune pickups
	WaterRunes   int `json:"waterRunes"`             // water rune pickups
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

	// Reports
	AbilityCastReport    []AbilityCast        `json:"abilityCastReport,omitempty"`
	HeroDamageReport     []DamageTarget       `json:"heroDamageReport,omitempty"`
	DamageReceivedReport *DamageReceivedReport `json:"damageReceivedReport,omitempty"`
	StunDurationDealt    float64              `json:"stunDurationDealt,omitempty"`
	SkillBuild           []SkillLevelUp       `json:"skillBuild,omitempty"`
	ItemUsed             []ItemUsed           `json:"itemUsed,omitempty"`
	CampStacks           []CampStack          `json:"campStack,omitempty"`

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
	
	Stats *PlayerStats `json:"stats,omitempty"`
}

type Match struct {
	ID               int64    `json:"id"`
	GameMode         int      `json:"gameMode"`
	LobbyType        int      `json:"lobbyType"`
	DidRadiantWin    bool     `json:"didRadiantWin"`
	DurationSeconds  int      `json:"durationSeconds"`
	StartDateTime    int64    `json:"startDateTime,omitempty"`
	
	RadiantNetworthLeads   []int `json:"radiantNetworthLeads,omitempty"`
	RadiantExperienceLeads []int `json:"radiantExperienceLeads,omitempty"`
	
	// Match events
	RoshanKills   []RoshanEvent   `json:"roshanKills,omitempty"`
	Buybacks      []BuybackEvent  `json:"buybacks,omitempty"`
	RuneSpawns    []RuneEvent     `json:"runeSpawns,omitempty"`
	BuildingKills []BuildingEvent `json:"buildingKills,omitempty"`
	Teamfights      []Teamfight      `json:"teamfights,omitempty"`
	PositionSamples []PositionSample `json:"positionSamples,omitempty"`

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
	135: "Dawnbreaker", 136: "Marci", 137: "Primal Beast", 138: "Muerta", 145: "Ringmaster",
}

func getHeroName(heroID int) string {
	if name, ok := heroNames[int32(heroID)]; ok {
		return name
	}
	return fmt.Sprintf("Hero_%d", heroID)
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

	// Damage tracking
	DamageByTarget         map[int]*DamageTarget
	DamageReceivedPhysical int
	DamageReceivedMagical  int
	DamageReceivedPure     int
	StunDurationDealt      float64

	// Item usage tracking (item_name → count)
	ItemUsage  map[string]int
	CampStacks []CampStack
	
	// Skill build tracking
	SkillBuild      []SkillLevelUp
	PrevAbilityLvls map[string]int // ability name → last known level

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

	// Reaggro tracking (physical attacks on enemy heroes in lane)
	LaneHarassCount int

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
	Buybacks      []BuybackEvent
	RuneSpawns    []RuneEvent
	BuildingKills []BuildingEvent
	
	// Item entity tracking for final items
	ItemEntities map[int32]string

	// Ability entity tracking for skill build
	AbilityEntities map[int32]*AbilityEntityInfo

	// Game timing from CDOTAGamerulesProxy
	GameStartTime float64 // m_flGameStartTime (server time when game clock starts)
	GameEndTime   float64 // m_flGameEndTime (server time when game ends)

	// Rune entity tracking for pickup attribution
	PendingRunes map[int32]*RuneEntityInfo // entityIdx → rune info

	// Bounty rune gold tracking (gold_reason=17) for confirmation
	BountyGoldEvents []BountyGoldEvent

	// Position sampling (every minute)
	PositionSamples   []PositionSample
	LastSampledMinute int

	// Gold lost tracking (GOLD event fires BEFORE DEATH event, so we buffer)
	PendingGoldLost [10]PendingGoldLoss
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
		PendingRunes:       make(map[int32]*RuneEntityInfo),
		BountyGoldEvents:   make([]BountyGoldEvent, 0),
		PositionSamples:    make([]PositionSample, 0),
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

func (s *ParserState) GameTime() float64 {
	return float64(s.CurrentTick) / 30.0
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
	p.Callbacks.OnCMsgDOTACombatLogEntry(func(m *dota.CMsgDOTACombatLogEntry) error {
		gameTime := float64(m.GetTimestamp())
		actualTime := state.ActualGameSeconds(gameTime) // 0 = horn
		logType := m.GetType()

		switch logType {
		case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_DEATH:
			targetName := state.LookupName(m.GetTargetName())
			attackerName := state.LookupName(m.GetAttackerName())

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

			// Track TP usage (modifier_teleporting)
			if modifierName == "modifier_teleporting" && strings.Contains(targetName, "hero") {
				targetIdx := heroNameToPlayerIndex(targetName, state)
				if targetIdx >= 0 && targetIdx < 10 {
					state.Players[targetIdx].TPCount++
				}
			}
			
			// Track smoke usage
			if modifierName == "modifier_smoke_of_deceit" && strings.Contains(targetName, "hero") {
				targetIdx := heroNameToPlayerIndex(targetName, state)
				if targetIdx >= 0 && targetIdx < 10 {
					state.Players[targetIdx].SmokeCount++
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
			// Attacker = hero who picks up, Target = rune entity
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
		
		if op == manta.EntityOpDeleted {
			return nil
		}

		className := e.GetClassName()
		actualGameTime := state.ActualGameSeconds(state.GameTime())
		gameMinute := int(actualGameTime / 60.0)

		// Extract hero ID from hero entities
		if strings.HasPrefix(className, "CDOTA_Unit_Hero_") {
			if replayPlayerID, ok := e.GetUint32("m_iPlayerID"); ok {
				playerIdx := replayPlayerToIndex(replayPlayerID)

				if playerIdx >= 0 && playerIdx < 10 {
					heroName := strings.TrimPrefix(className, "CDOTA_Unit_Hero_")
					heroID := heroNameStringToID(heroName)
					if heroID > 0 && state.Players[playerIdx].HeroID == 0 {
						state.Players[playerIdx].HeroID = heroID
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
			if endTime, ok := e.GetFloat32("m_pGameRules.m_flGameEndTime"); ok {
				if endTime > 0 {
					state.GameEndTime = float64(endTime)
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
				if heroID, ok := e.GetInt32(fmt.Sprintf("m_vecPlayerTeamData.%04d.m_nSelectedHeroID", i)); ok && heroID > 0 {
					state.Players[i].HeroID = int(heroID)
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

		// Rune entity destroyed = picked up or expired
		if className == "CDOTA_Item_Rune" && op&manta.EntityOpDeleted != 0 {
			runeTime := state.ActualGameSeconds(state.GameTime())
			idx := e.GetIndex()
			if runeInfo, exists := state.PendingRunes[idx]; exists {
				// Find nearest hero by position
				nearestPlayer := findNearestHero(state, runeInfo.PosX, runeInfo.PosY)
				if nearestPlayer >= 0 {
					// For bounty runes (type=5): verify with gold_reason=17 event within 5 seconds
					if runeInfo.RuneType == 5 {
						if hasBountyGoldNear(state, runeTime, nearestPlayer) {
							state.Players[nearestPlayer].Runes = append(state.Players[nearestPlayer].Runes, RuneEvent{
								Time:     runeTime,
								RuneType: runeInfo.RuneType,
								Action:   1, // pickup
							})
						}
					} else if runeInfo.RuneType == 7 {
						// Water runes (type=7): attribute via proximity (no gold/modifier to verify)
						dist := heroDistToPoint(state, nearestPlayer, runeInfo.PosX, runeInfo.PosY)
						if dist < 30 { // reasonable proximity threshold
							state.Players[nearestPlayer].Runes = append(state.Players[nearestPlayer].Runes, RuneEvent{
								Time:     runeTime,
								RuneType: runeInfo.RuneType,
								Action:   1, // pickup
							})
						}
					}
					// Power runes already tracked via modifier_add — skip entity-based for those
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

func heroNameStringToID(name string) int {
	nameMap := map[string]int{
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
		"dawnbreaker": 135, "marci": 136, "primal_beast": 137, "muerta": 138, "ringmaster": 145,
		
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

	name = strings.ToLower(name)
	// Try with underscores first (matches combat log names like "shadow_demon")
	if id, ok := nameMap[name]; ok {
		return id
	}
	// Try without underscores/spaces (matches entity class names like "ShadowDemon")
	name = strings.ReplaceAll(name, "_", "")
	name = strings.ReplaceAll(name, " ", "")
	
	if id, ok := nameMap[name]; ok {
		return id
	}
	
	name2 := strings.ToLower(strings.ReplaceAll(name, " ", "_"))
	if id, ok := nameMap[name2]; ok {
		return id
	}
	
	return 0
}

func detectLane(positions []struct{ X, Y float64 }, isRadiant bool) string {
	if len(positions) < 500 {
		return "unknown"
	}
	
	// Skip first 1000 samples (~spawn time), sample next 2000 (core laning phase)
	start := 1000
	if start >= len(positions) {
		start = len(positions) / 4
	}
	end := start + 2000
	if end > len(positions) {
		end = len(positions)
	}
	
	var avgX, avgY float64
	count := 0
	for i := start; i < end; i++ {
		avgX += positions[i].X
		avgY += positions[i].Y
		count++
	}
	if count == 0 {
		return "unknown"
	}
	avgX /= float64(count)
	avgY /= float64(count)
	
	// Map is roughly 256x256 cells
	// Radiant base ~(64,64), Dire base ~(192,192)
	// Safe lane for Radiant = bottom (high Y), Off = top (low Y)
	// Safe lane for Dire = top (low Y), Off = bottom (high Y)
	
	// Determine quadrant
	midX, midY := 128.0, 128.0
	
	if avgY > midY+30 { // Bottom half (Y > 158)
		if avgX < midX-10 { // Bottom-left = Radiant safe lane area
			if isRadiant {
				return "safe"
			}
			return "off"
		} else { // Bottom-right = transitional/mid area
			return "roam"
		}
	} else if avgY < midY-30 { // Top half (Y < 98)  
		if avgX > midX+10 { // Top-right = Dire safe lane area
			if isRadiant {
				return "off"
			}
			return "safe"
		} else { // Top-left = transitional
			return "roam"
		}
	} else { // Middle band
		// Check for mid lane (diagonal)
		if avgX > midX-20 && avgX < midX+20 && avgY > midY-20 && avgY < midY+20 {
			return "mid"
		}
		return "roam"
	}
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

		// Detect lane
		lane := detectLane(ps.LanePositions, ps.IsRadiant)
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

		// Calculate role/position based on networth at 10 min
		role := 0 // core
		position := i%5 + 1
		if ps.NWAt10 < 3000 { // Low farm = support
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
			AbilityCastReport:    abilityCasts,
			HeroDamageReport:     damageReport,
			DamageReceivedReport: damageReceivedReport,
			StunDurationDealt:    ps.StunDurationDealt,
			SkillBuild:           filterSkillBuild(ps.SkillBuild),
			ItemUsed:             itemUsed,
			CampStacks:           ps.CampStacks,
			LaneStats: LaneStats{
				Lane:          lane,
				ReaggroCount:  ps.LaneHarassCount, // Lane harass events as aggro proxy
				DeathsPreTen:  ps.LaneDeaths,
				KillsPreTen:   ps.LaneKills,
				AssistsPreTen: ps.LaneAssists,
				NetWorthAtTen: ps.NWAt10,
				LevelAtTen:    ps.LevelAt10,
				GPMAtTen:      ps.NWAt10 / 10,
				XPMAtTen:      xpmAtTen(ps),
			},
			VisionStats: VisionExposure{
				SmokeUsageCount: ps.SmokeCount,
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

	return Match{
		ID:                     state.MatchID,
		GameMode:               state.GameMode,
		LobbyType:              state.LobbyType,
		DidRadiantWin:          state.RadiantWin,
		DurationSeconds:        int(duration),
		StartDateTime:          state.StartTime,
		RadiantNetworthLeads:   nwLeads,
		RadiantExperienceLeads: xpLeads,
		RoshanKills:            state.RoshanKills,
		Buybacks:               state.Buybacks,
		RuneSpawns:             state.RuneSpawns,
		BuildingKills:          state.BuildingKills,
		Teamfights:             detectTeamfights(state),
		PositionSamples:        state.PositionSamples,
		Players:                players,
		ParsedFromReplay:       true,
		ParserVersion:          "3.1.2",
	}
}

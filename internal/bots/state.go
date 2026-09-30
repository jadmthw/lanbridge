package bots

import (
	"math"
	"sort"
	"strings"
	"time"

	"lanbridge/internal/bridge"
)

// State is what the bridge reports about an AI player.
type State struct {
	Online  bridge.Flag        `json:"online"`
	X       float64            `json:"x"`
	Y       float64            `json:"y"`
	Z       float64            `json:"z"`
	Yaw     float64            `json:"yaw"`
	Pitch   float64            `json:"pitch"`
	Health  float64            `json:"health"`
	Food    float64            `json:"food"`
	Dim     string             `json:"dim"`
	Ground  bridge.Flag        `json:"ground"`
	Mode    string             `json:"mode"`
	Holds   *Held              `json:"holds"`
	Inv     map[string]float64 `json:"inv"`
	Near    []Entity           `json:"near"`
	Items   float64            `json:"items"`
	Players []PlayerPos        `json:"players"`
	Daytime float64            `json:"daytime"`

	at time.Time
}

// Held is the item in the main hand.
type Held struct {
	Item  string  `json:"item"`
	Count float64 `json:"count"`
}

// Entity is a creature or player near the bot.
type Entity struct {
	Type string  `json:"type"`
	Name string  `json:"name"`
	ID   float64 `json:"id"`
	Cat  string  `json:"cat"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	Z    float64 `json:"z"`
}

// PlayerPos is a real (human) player.
type PlayerPos struct {
	Name string      `json:"name"`
	X    float64     `json:"x"`
	Y    float64     `json:"y"`
	Z    float64     `json:"z"`
	Dim  string      `json:"dim"`
	Host bridge.Flag `json:"host"`
}

func (s *State) player(name string) *PlayerPos {
	for i := range s.Players {
		if strings.EqualFold(s.Players[i].Name, name) {
			return &s.Players[i]
		}
	}
	return nil
}

func (s *State) dist(x, y, z float64) float64 {
	return math.Sqrt((s.X-x)*(s.X-x) + (s.Y-y)*(s.Y-y) + (s.Z-z)*(s.Z-z))
}

func (s *State) flatDist(x, z float64) float64 { return math.Hypot(s.X-x, s.Z-z) }

func (s *State) count(item string) int { return int(s.Inv[item]) }

// entity finds a creature by id.
func (s *State) entity(id float64) *Entity {
	for i := range s.Near {
		if s.Near[i].ID == id {
			return &s.Near[i]
		}
	}
	return nil
}

// Summary describes the bot's situation for the model.
func (s *State) Summary(self string) string {
	var b strings.Builder
	dim := strings.TrimPrefix(s.Dim, "minecraft:")
	b.WriteString("You are at x=" + itoa(int(math.Floor(s.X))) + ", y=" + itoa(int(math.Floor(s.Y))) + ", z=" + itoa(int(math.Floor(s.Z))) + " in the " + strings.ReplaceAll(dim, "_", " "))
	b.WriteString(". Health " + itoa(int(math.Round(s.Health))) + "/20, food " + itoa(int(math.Round(s.Food))) + "/20")
	if s.Holds != nil && s.Holds.Item != "" && s.Holds.Item != "air" {
		b.WriteString(", holding " + s.Holds.Item)
	}
	b.WriteString(". It is " + timeOfDay(s.Daytime) + ".\nInventory: ")
	type kv struct {
		k string
		v int
	}
	var inv []kv
	for k, v := range s.Inv {
		if v > 0 {
			inv = append(inv, kv{k, int(v)})
		}
	}
	sort.Slice(inv, func(i, j int) bool { return inv[i].v > inv[j].v || (inv[i].v == inv[j].v && inv[i].k < inv[j].k) })
	if len(inv) == 0 {
		b.WriteString("empty")
	}
	for i, e := range inv {
		if i == 20 {
			b.WriteString(", …")
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(e.k + " x" + itoa(e.v))
	}
	b.WriteString("\nPlayers: ")
	for i, p := range s.Players {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p.Name)
		if p.Dim != s.Dim {
			b.WriteString(" (in another dimension)")
		} else {
			b.WriteString(" (" + itoa(int(s.dist(p.X, p.Y, p.Z))) + " blocks away)")
		}
	}
	if len(s.Players) == 0 {
		b.WriteString("none")
	}
	var mobs []string
	seen := map[string]int{}
	for _, e := range s.Near {
		if e.Type == "player" {
			continue
		}
		if seen[e.Type] == 0 {
			mobs = append(mobs, e.Type)
		}
		seen[e.Type]++
	}
	if len(mobs) > 0 {
		b.WriteString("\nCreatures nearby: ")
		for i, m := range mobs {
			if i > 0 {
				b.WriteString(", ")
			}
			if seen[m] > 1 {
				b.WriteString(itoa(seen[m]) + " " + m)
			} else {
				b.WriteString(m)
			}
		}
	}
	return b.String()
}

func timeOfDay(t float64) string {
	switch {
	case t < 12000:
		return "day"
	case t < 13000:
		return "sunset"
	case t < 23000:
		return "night"
	}
	return "sunrise"
}

func itoa(i int) string {
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	n := len(b)
	for {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
		if i == 0 {
			break
		}
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}

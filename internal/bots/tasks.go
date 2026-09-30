package bots

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// body is how tasks move an AI player around. *Bot implements it; tests use a fake.
type body interface {
	act(a string)
	lookAt(x, y, z float64)
	walk(on bool)
	sprint(on bool)
	jump()
	stopAll()
	tpTo(player string)
	call(ctx context.Context, op string, args map[string]any, out any) error
	name() string
}

// task is something an AI player is doing. step runs a few times a second.
type task interface {
	desc() string
	step(ctx context.Context, b body, st *State) (done bool, msg string)
}

// mover steers toward a point and notices when the bot is stuck.
type mover struct {
	last     [2]float64
	progress time.Time
	jumped   time.Time
}

func (mv *mover) reset() { mv.progress = time.Time{} }

// toward walks toward (x, y, z). It reports arriving within r blocks, or
// being stuck (no progress for 6 seconds).
func (mv *mover) toward(b body, st *State, x, y, z, r float64) (arrived, stuck bool) {
	d := st.flatDist(x, z)
	if d <= r && math.Abs(y-st.Y) < 2.5 {
		b.walk(false)
		b.sprint(false)
		mv.reset()
		return true, false
	}
	b.lookAt(x, st.Y+1.62, z) // eye level, so it walks straight
	b.walk(true)
	b.sprint(d > 7)
	now := time.Now()
	if mv.progress.IsZero() || math.Hypot(st.X-mv.last[0], st.Z-mv.last[1]) > 0.3 {
		mv.last, mv.progress = [2]float64{st.X, st.Z}, now
	} else if now.Sub(mv.progress) > 500*time.Millisecond && now.Sub(mv.jumped) > 700*time.Millisecond {
		b.jump() // blocked by something: hop up
		mv.jumped = now
	}
	return false, now.Sub(mv.progress) > 6*time.Second
}

type followTask struct {
	target string
	mv     mover
}

func (t *followTask) desc() string { return "following " + t.target }

func (t *followTask) step(_ context.Context, b body, st *State) (bool, string) {
	p := st.player(t.target)
	if p == nil {
		return true, "I lost track of " + t.target + "."
	}
	if p.Dim != st.Dim || st.dist(p.X, p.Y, p.Z) > 48 {
		b.tpTo(p.Name)
		t.mv.reset()
		return false, ""
	}
	if arrived, stuck := t.mv.toward(b, st, p.X, p.Y, p.Z, 2.5); arrived {
		b.lookAt(p.X, p.Y+1.62, p.Z)
	} else if stuck {
		b.tpTo(p.Name)
		t.mv.reset()
	}
	return false, ""
}

type comeTask struct{ target string }

func (t *comeTask) desc() string { return "coming to " + t.target }

func (t *comeTask) step(_ context.Context, b body, st *State) (bool, string) {
	if st.player(t.target) == nil {
		return true, "I can't find " + t.target + "."
	}
	b.tpTo(t.target)
	return true, ""
}

type gotoTask struct {
	x, y, z float64
	mv      mover
	start   time.Time
}

func (t *gotoTask) desc() string { return fmt.Sprintf("walking to %.0f %.0f %.0f", t.x, t.y, t.z) }

func (t *gotoTask) step(_ context.Context, b body, st *State) (bool, string) {
	if t.start.IsZero() {
		t.start = time.Now()
	}
	arrived, stuck := t.mv.toward(b, st, t.x, t.y, t.z, 1.5)
	switch {
	case arrived:
		return true, "I'm there."
	case stuck:
		return true, "I can't find a way there."
	case time.Since(t.start) > 3*time.Minute:
		return true, "That's too far for me to walk."
	}
	return false, ""
}

type lookTask struct{ target string }

func (t *lookTask) desc() string { return "looking at " + t.target }

func (t *lookTask) step(_ context.Context, b body, st *State) (bool, string) {
	if p := st.player(t.target); p != nil {
		b.lookAt(p.X, p.Y+1.62, p.Z)
	}
	return true, ""
}

// simpleTask sends one Carpet action.
type simpleTask struct{ label, action string }

func (t *simpleTask) desc() string { return t.label }

func (t *simpleTask) step(_ context.Context, b body, _ *State) (bool, string) {
	b.act(t.action)
	return true, ""
}

type equipTask struct{ item string }

func (t *equipTask) desc() string { return "equipping " + t.item }

func (t *equipTask) step(ctx context.Context, b body, st *State) (bool, string) {
	item := resolveItem(st, t.item)
	if item == "" {
		return true, "I don't have any " + readable(t.item) + "."
	}
	if err := b.call(ctx, "select", map[string]any{"name": b.name(), "item": item}, nil); err != nil {
		return true, "I couldn't hold " + readable(item) + "."
	}
	return true, ""
}

var foods = []string{"golden_carrot", "cooked_beef", "cooked_porkchop", "cooked_mutton", "cooked_salmon", "cooked_chicken", "cooked_rabbit",
	"cooked_cod", "baked_potato", "bread", "pumpkin_pie", "rabbit_stew", "mushroom_stew", "beetroot_soup", "apple", "carrot", "melon_slice",
	"sweet_berries", "glow_berries", "cookie", "dried_kelp", "beef", "porkchop", "mutton", "potato", "beetroot"}

type eatTask struct {
	started time.Time
	food    string
}

func (t *eatTask) desc() string { return "eating" }

func (t *eatTask) step(ctx context.Context, b body, st *State) (bool, string) {
	if t.started.IsZero() {
		for _, f := range foods {
			if st.count(f) > 0 {
				t.food = f
				break
			}
		}
		if t.food == "" {
			return true, "I don't have any food."
		}
		if err := b.call(ctx, "select", map[string]any{"name": b.name(), "item": t.food}, nil); err != nil {
			return true, "I couldn't get my food out."
		}
		b.act("use continuous")
		t.started = time.Now()
		return false, ""
	}
	if time.Since(t.started) < 1800*time.Millisecond {
		return false, ""
	}
	b.stopAll()
	return true, ""
}

type giveTask struct {
	target, item string
	count        int
	mv           mover
	start        time.Time
}

func (t *giveTask) desc() string { return "bringing " + readable(t.item) + " to " + t.target }

func (t *giveTask) step(ctx context.Context, b body, st *State) (bool, string) {
	if t.start.IsZero() {
		t.start = time.Now()
	}
	item := resolveItem(st, t.item)
	if item == "" {
		return true, "I don't have any " + readable(t.item) + "."
	}
	p := st.player(t.target)
	if p == nil {
		return true, "I can't find " + t.target + "."
	}
	if p.Dim != st.Dim || st.dist(p.X, p.Y, p.Z) > 48 {
		b.tpTo(p.Name)
		return false, ""
	}
	arrived, stuck := t.mv.toward(b, st, p.X, p.Y, p.Z, 2.2)
	if stuck || time.Since(t.start) > 90*time.Second {
		b.tpTo(p.Name)
		t.mv.reset()
		return false, ""
	}
	if !arrived {
		return false, ""
	}
	b.lookAt(p.X, p.Y+1.3, p.Z)
	var res struct {
		Dropped float64 `json:"dropped"`
	}
	if err := b.call(ctx, "give", map[string]any{"name": b.name(), "item": item, "count": t.count}, &res); err != nil || res.Dropped == 0 {
		return true, "I couldn't toss it to you."
	}
	return true, fmt.Sprintf("Here you go: %d %s.", int(res.Dropped), readable(item))
}

type attackTask struct {
	mob    string
	target float64
	hit    bool
	mv     mover
	start  time.Time
	last   time.Time
	armed  bool
}

func (t *attackTask) desc() string { return "fighting " + readable(t.mob) }

func (t *attackTask) matches(e *Entity) bool {
	if e.Type == "player" {
		return false
	}
	switch t.mob {
	case "hostile", "monster", "monsters", "mob", "mobs", "enemy", "enemies":
		return e.Cat == "monster"
	}
	return e.Type == t.mob
}

func (t *attackTask) step(ctx context.Context, b body, st *State) (bool, string) {
	if t.start.IsZero() {
		t.start = time.Now()
	}
	if !t.armed {
		t.armed = true
		if w := bestWeapon(st); w != "" {
			_ = b.call(ctx, "select", map[string]any{"name": b.name(), "item": w}, nil)
		}
	}
	e := st.entity(t.target)
	if e == nil {
		if t.hit {
			return true, "Got it!"
		}
		best := math.MaxFloat64
		for i := range st.Near {
			if d := st.dist(st.Near[i].X, st.Near[i].Y, st.Near[i].Z); t.matches(&st.Near[i]) && d < best {
				best, e = d, &st.Near[i]
			}
		}
		if e == nil {
			return true, "I don't see any " + readable(t.mob) + " nearby."
		}
		t.target = e.ID
	}
	if time.Since(t.start) > 45*time.Second {
		return true, "I couldn't catch it."
	}
	if st.dist(e.X, e.Y, e.Z) > 3.0 {
		t.mv.toward(b, st, e.X, e.Y, e.Z, 1.8)
		return false, ""
	}
	b.walk(false)
	b.lookAt(e.X, e.Y+0.9, e.Z)
	if time.Since(t.last) > 650*time.Millisecond {
		b.act("attack")
		t.last, t.hit = time.Now(), true
	}
	return false, ""
}

type collectTask struct {
	pattern *regexp.Regexp
	label   string
	want    int
	got     int
	phase   int // 0 find, 1 walk, 2 dig, 3 pick up
	target  [3]int
	skip    map[[3]int]bool
	since   time.Time
	mv      mover
	tool    bool
}

func newCollect(block string, count int) *collectTask {
	pat, label := blockPattern(block)
	return &collectTask{pattern: regexp.MustCompile(pat), label: label, want: count, skip: map[[3]int]bool{}}
}

func (t *collectTask) desc() string {
	return fmt.Sprintf("collecting %s (%d/%d)", t.label, t.got, t.want)
}

func (t *collectTask) center() (float64, float64, float64) {
	return float64(t.target[0]) + 0.5, float64(t.target[1]) + 0.5, float64(t.target[2]) + 0.5
}

func (t *collectTask) step(ctx context.Context, b body, st *State) (bool, string) {
	switch t.phase {
	case 0:
		if t.got >= t.want {
			return true, fmt.Sprintf("Done, I got %d %s.", t.got, t.label)
		}
		var found [][3]float64
		if err := b.call(ctx, "find", map[string]any{"name": b.name(), "pattern": t.pattern.String(), "radius": 16}, &found); err != nil {
			return true, "I couldn't look around: " + err.Error()
		}
		for _, f := range found {
			p := [3]int{int(f[0]), int(f[1]), int(f[2])}
			if !t.skip[p] {
				t.target, t.phase, t.since = p, 1, time.Now()
				t.mv.reset()
				return false, ""
			}
		}
		if t.got > 0 {
			return true, fmt.Sprintf("That's all the %s I can find nearby. I got %d.", t.label, t.got)
		}
		return true, "I can't find any " + t.label + " nearby."
	case 1:
		cx, cy, cz := t.center()
		if st.dist(cx, cy-1.1, cz) <= 4.0 {
			b.walk(false)
			b.sprint(false)
			if !t.tool {
				t.tool = true
				if tool := bestTool(st, t.label); tool != "" {
					_ = b.call(ctx, "select", map[string]any{"name": b.name(), "item": tool}, nil)
				}
			}
			b.lookAt(cx, cy, cz)
			b.act("attack continuous")
			t.phase, t.since = 2, time.Now()
			return false, ""
		}
		_, stuck := t.mv.toward(b, st, cx, cy, cz, 1.2)
		if stuck || time.Since(t.since) > 30*time.Second || (st.flatDist(cx, cz) < 1.5 && math.Abs(cy-st.Y) > 4) {
			t.skip[t.target] = true
			t.phase = 0
			b.stopAll()
		}
		return false, ""
	case 2:
		cx, cy, cz := t.center()
		var blk struct {
			Block string `json:"block"`
		}
		if err := b.call(ctx, "block", map[string]any{"x": t.target[0], "y": t.target[1], "z": t.target[2]}, &blk); err != nil {
			return false, ""
		}
		if !t.pattern.MatchString(blk.Block) {
			b.stopAll()
			t.got++
			t.phase, t.since = 3, time.Now()
			return false, ""
		}
		if time.Since(t.since) > 15*time.Second {
			b.stopAll()
			t.skip[t.target] = true
			t.phase = 0
			return false, ""
		}
		b.lookAt(cx, cy, cz)
		return false, ""
	default: // walk over the drop so it gets picked up
		cx, cy, cz := t.center()
		arrived, stuck := t.mv.toward(b, st, cx, cy, cz, 0.6)
		if arrived || stuck || time.Since(t.since) > 2500*time.Millisecond {
			b.walk(false)
			t.phase = 0
		}
		return false, ""
	}
}

var (
	logTypes = `(oak|spruce|birch|jungle|acacia|dark_oak|mangrove|cherry|pale_oak)_log`
	ores     = map[string]bool{"coal": true, "iron": true, "copper": true, "gold": true, "diamond": true, "emerald": true, "lapis": true, "redstone": true}
	idRe     = regexp.MustCompile(`^[a-z0-9_]+$`)
)

// blockPattern turns what someone asked for into a block-name regexp.
func blockPattern(name string) (pattern, label string) {
	n := normID(name)
	switch n {
	case "log", "logs", "wood", "woods", "tree", "trees", "timber":
		return "^" + logTypes + "$", "logs"
	case "stone", "stones", "rock", "rocks", "cobble", "cobblestone":
		return "^(stone|cobblestone)$", "stone"
	case "dirt", "grass", "grass_block":
		return "^(dirt|grass_block|coarse_dirt|rooted_dirt)$", "dirt"
	case "sand", "sands":
		return "^(sand|red_sand)$", "sand"
	case "leaves", "leaf":
		return "^[a-z_]+_leaves$", "leaves"
	}
	n = strings.TrimSuffix(strings.TrimSuffix(n, "s"), "_ore")
	if ores[n] {
		if n == "lapis" {
			return "^(deepslate_)?lapis_ore$", "lapis ore"
		}
		return "^(deepslate_)?" + n + "_ore$", n + " ore"
	}
	n = normID(name)
	if !idRe.MatchString(n) {
		n = "stone"
	}
	if strings.HasSuffix(n, "_log") || strings.HasSuffix(n, "_ore") {
		return "^" + n + "$", readable(n)
	}
	return "^" + n + "s?$", readable(n)
}

func normID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "minecraft:")
	return strings.ReplaceAll(strings.ReplaceAll(s, " ", "_"), "-", "_")
}

func readable(id string) string { return strings.ReplaceAll(id, "_", " ") }

// resolveItem finds what the bot actually carries that matches a request.
func resolveItem(st *State, want string) string {
	n := normID(want)
	if st.count(n) > 0 {
		return n
	}
	single := strings.TrimSuffix(n, "s")
	if st.count(single) > 0 {
		return single
	}
	best, bestN := "", 0
	for item, c := range st.Inv {
		if c > 0 && (strings.Contains(item, single) || (single == "wood" && strings.HasSuffix(item, "_log"))) && int(c) > bestN {
			best, bestN = item, int(c)
		}
	}
	return best
}

var tiers = []string{"netherite", "diamond", "iron", "stone", "golden", "copper", "wooden"}

func bestTool(st *State, block string) string {
	kind := ""
	switch {
	case strings.Contains(block, "log") || strings.Contains(block, "wood") || strings.Contains(block, "plank"):
		kind = "_axe"
	case strings.Contains(block, "dirt") || strings.Contains(block, "sand") || strings.Contains(block, "gravel") ||
		strings.Contains(block, "clay") || strings.Contains(block, "snow") || strings.Contains(block, "mud"):
		kind = "_shovel"
	case strings.Contains(block, "leaves"):
		return ""
	default:
		kind = "_pickaxe"
	}
	for _, t := range tiers {
		if st.count(t+kind) > 0 {
			return t + kind
		}
	}
	return ""
}

func bestWeapon(st *State) string {
	for _, kind := range []string{"_sword", "_axe"} {
		for _, t := range tiers {
			if st.count(t+kind) > 0 {
				return t + kind
			}
		}
	}
	return ""
}

// parseAction turns an action string from the model into a task.
// from is the player who asked; players lists who is online.
func parseAction(s, from string, players []string) (task, error) {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) == 0 {
		return nil, fmt.Errorf("empty action")
	}
	verb := strings.ToLower(strings.Trim(f[0], "!/"))
	args := f[1:]
	who := func(i int) string {
		if i < len(args) {
			if p := matchPlayer(args[i], players); p != "" {
				return p
			}
		}
		return from
	}
	var nums []float64
	var words []string
	for _, a := range args {
		if v, err := strconv.ParseFloat(strings.TrimSuffix(a, ","), 64); err == nil {
			nums = append(nums, v)
		} else if matchPlayer(a, players) == "" {
			words = append(words, a)
		}
	}
	count := func(def int) int {
		if len(nums) > 0 && nums[0] >= 1 {
			return int(math.Min(nums[0], 64))
		}
		return def
	}
	switch verb {
	case "follow":
		return &followTask{target: who(0)}, nil
	case "come", "tp", "teleport":
		return &comeTask{target: who(0)}, nil
	case "goto", "go", "walk":
		if len(nums) >= 3 {
			return &gotoTask{x: nums[0], y: nums[1], z: nums[2]}, nil
		}
		return &comeTask{target: who(0)}, nil
	case "stop", "stay", "wait", "halt":
		return &simpleTask{label: "stopping", action: "stop"}, nil
	case "collect", "mine", "gather", "chop", "dig", "get", "harvest":
		if len(words) == 0 {
			return nil, fmt.Errorf("collect what?")
		}
		return newCollect(strings.Join(words, "_"), count(1)), nil
	case "attack", "kill", "fight", "hunt":
		if len(words) == 0 {
			return &attackTask{mob: "hostile"}, nil
		}
		return &attackTask{mob: strings.TrimSuffix(normID(strings.Join(words, "_")), "s")}, nil
	case "give", "drop", "toss", "bring":
		if len(words) == 0 {
			return nil, fmt.Errorf("give what?")
		}
		target := from
		for _, a := range args {
			if p := matchPlayer(a, players); p != "" {
				target = p
			}
		}
		return &giveTask{target: target, item: strings.Join(words, "_"), count: count(1)}, nil
	case "equip", "hold", "wield", "select":
		if len(words) == 0 {
			return nil, fmt.Errorf("equip what?")
		}
		return &equipTask{item: strings.Join(words, "_")}, nil
	case "eat":
		return &eatTask{}, nil
	case "look":
		return &lookTask{target: who(0)}, nil
	case "jump":
		return &simpleTask{label: "jumping", action: "jump"}, nil
	case "sneak", "crouch":
		return &simpleTask{label: "sneaking", action: "sneak"}, nil
	case "unsneak", "stand":
		return &simpleTask{label: "standing up", action: "unsneak"}, nil
	}
	return nil, fmt.Errorf("unknown action %q", verb)
}

func matchPlayer(s string, players []string) string {
	s = strings.Trim(s, "@,.!?")
	for _, p := range players {
		if strings.EqualFold(p, s) {
			return p
		}
	}
	return ""
}

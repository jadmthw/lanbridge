package bots

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"lanbridge/internal/ai"
)

const (
	siteRadius   = 16  // survey a 33x33 area around the build spot
	spotAhead    = 4   // the build spot is this many blocks in front of the player
	maxBuildCmds = 800 // per build
)

// site is a survey of the ground around a build spot.
type site struct {
	x, y, z int // the build spot, ~0 ~0 ~0; the ground under it is y-1
	dim     string
	rad     int
	top     [][]int    // absolute height of the top block, [dz+rad][dx+rad]
	blk     [][]string // name of that block
	px, pz  int        // where the player who asked stands, relative to the spot
	hasP    bool
	face    string
}

// survey asks the game for the ground height and surface block around a spot.
func (b *Bot) survey(ctx context.Context, x, y, z int, dim string) (*site, error) {
	var cells [][]any
	if err := b.m.call(ctx, "terrain", map[string]any{"x": x, "z": z, "dim": dim, "radius": siteRadius}, &cells); err != nil {
		return nil, err
	}
	n := 2*siteRadius + 1
	if len(cells) != n*n {
		return nil, fmt.Errorf("unexpected survey size %d", len(cells))
	}
	s := &site{x: x, y: y, z: z, dim: dim, rad: siteRadius, top: make([][]int, n), blk: make([][]string, n)}
	for r := 0; r < n; r++ {
		s.top[r], s.blk[r] = make([]int, n), make([]string, n)
		for c := 0; c < n; c++ {
			cell := cells[r*n+c]
			if len(cell) == 2 {
				if h, ok := cell[0].(float64); ok {
					s.top[r][c] = int(h)
				}
				s.blk[r][c], _ = cell[1].(string)
			}
		}
	}
	return s, nil
}

// surfaceKind sorts a block into a map symbol.
func surfaceKind(name string) byte {
	n := strings.TrimPrefix(name, "minecraft:")
	has := func(parts ...string) bool {
		for _, p := range parts {
			if strings.Contains(n, p) {
				return true
			}
		}
		return false
	}
	switch {
	case has("water", "seagrass", "kelp", "bubble_column"):
		return 'w'
	case has("lava"):
		return 'L'
	case has("_log", "_wood", "leaves", "_stem", "mushroom_block"):
		return 'T'
	case has("planks", "brick", "glass", "wool", "concrete", "terracotta", "fence", "door", "stairs", "slab", "torch", "lantern",
		"chest", "crafting", "furnace", "bed", "polished", "smooth", "cut_", "chiseled", "tiles", "wall", "carpet", "rail"):
		return 'B'
	case has("snow", "ice"):
		return '~'
	case has("sand", "gravel", "clay"):
		return 'a'
	case has("grass_block", "dirt", "podzol", "mycelium", "mud", "moss", "farmland", "path"):
		return '.'
	case has("stone", "andesite", "diorite", "granite", "deepslate", "tuff", "calcite", "ore", "netherrack", "basalt", "blackstone", "end_stone"):
		return 's'
	}
	return '#'
}

// describe turns the survey into text a model can plan with.
func (s *site) describe() string {
	n := 2*s.rad + 1
	ground := s.y - 1
	lo, hi := math.MaxInt, math.MinInt
	var sb strings.Builder
	fmt.Fprintf(&sb, "SITE SURVEY (%dx%d blocks around the build spot)\n", n, n)
	sb.WriteString("Coordinates below are relative to the build spot ~0 ~0 ~0. Columns go west to east (x = -16 … +16), rows go north to south (z = -16 … +16).\n\n")
	sb.WriteString("Height map: height of the top solid block (or water surface) relative to ~-1, the ground under the build spot. 0 = level ground, 2 = the ground there is two blocks higher (top block at ~1), -3 = three blocks lower.\n")
	for r := 0; r < n; r++ {
		fmt.Fprintf(&sb, "z=%+03d:", r-s.rad)
		for c := 0; c < n; c++ {
			d := s.top[r][c] - ground
			lo, hi = min(lo, d), max(hi, d)
			fmt.Fprintf(&sb, " %d", d)
		}
		sb.WriteByte('\n')
	}
	sb.WriteString("\nSurface map: . grass/dirt, s stone, a sand/gravel, w water, L lava, T tree (logs), ~ snow/ice, B blocks someone built (don't destroy these), # other; O = build spot; P = player who asked.\n")
	for r := 0; r < n; r++ {
		fmt.Fprintf(&sb, "z=%+03d: ", r-s.rad)
		for c := 0; c < n; c++ {
			ch := surfaceKind(s.blk[r][c])
			switch {
			case r == s.rad && c == s.rad:
				ch = 'O'
			case s.hasP && r == s.pz+s.rad && c == s.px+s.rad:
				ch = 'P'
			}
			sb.WriteByte(ch)
		}
		sb.WriteByte('\n')
	}
	fmt.Fprintf(&sb, "\nTerrain relief in this area: from %d to %+d relative to ~-1.", lo, hi)
	if s.hasP {
		fmt.Fprintf(&sb, " The player stands at x=%d, z=%d (relative) facing %s, toward the build spot: keep the build at least 2 blocks away from them and put the entrance on the side facing them.", s.px, s.pz, s.face)
	}
	sb.WriteByte('\n')
	return sb.String()
}

const architectSystem = `You are a master Minecraft: Java Edition builder working through commands. You design a complete, good-looking build that fits the real terrain and output it as fill and setblock commands. The commands are pasted into the game exactly as you write them.

COORDINATES
- Every command uses relative coordinates (~x ~y ~z) from the build spot ~0 ~0 ~0, which is the air block on top of the ground at the spot the player chose. ~0 ~-1 ~0 is the ground block under it.
- +x is east, -x is west, +z is south, -z is north, +y is up.
- You get a height map and a surface map of the 33x33 area (x and z from -16 to +16). Stay inside it.

COMMANDS YOU CAN USE (no leading slash)
- fill <x1> <y1> <z1> <x2> <y2> <z2> <block> [replace|hollow|outline|keep|destroy]
  hollow = walls, floor and ceiling of the box with air inside; outline = the shell only, leaving the inside alone.
  At most 32768 blocks per fill, so split giant volumes.
- setblock <x> <y> <z> <block>
- clone <x1> <y1> <z1> <x2> <y2> <z2> <x> <y> <z>   (to repeat a finished section)
- summon <entity> <x> <y> <z> [data], only for decoration entities: text_display, block_display, item_display, armor_stand, item_frame, glow_item_frame, painting

BLOCK IDS AND STATES (Java Edition names, no "minecraft:" needed)
- Woods: oak, spruce, birch, jungle, acacia, dark_oak, mangrove, cherry, pale_oak, bamboo (bamboo_planks), crimson, warped (these two use _stem/_hyphae instead of _log/_wood).
  Each wood has _planks, _log[axis=x|y|z], stripped_<wood>_log, _wood (bark on all sides), _stairs, _slab, _fence, _fence_gate, _door, _trapdoor, _button, _pressure_plate.
- Stone: stone, cobblestone, mossy_cobblestone, stone_bricks, mossy_stone_bricks, cracked_stone_bricks, chiseled_stone_bricks, smooth_stone, andesite/diorite/granite and polished_ versions, deepslate_bricks, deepslate_tiles, polished_deepslate, cobbled_deepslate, tuff_bricks, bricks, mud_bricks, sandstone, cut_sandstone, smooth_sandstone, quartz_block, quartz_pillar[axis=y], blackstone, polished_blackstone_bricks, calcite. Most have _stairs, _slab and _wall variants (for example stone_brick_stairs, cobblestone_wall).
- Glass: glass, glass_pane, tinted_glass, white_stained_glass (any color).
- Colors (white, light_gray, gray, black, brown, red, orange, yellow, lime, green, cyan, light_blue, blue, purple, magenta, pink): <color>_wool, _concrete, _terracotta, _carpet, _bed, _banner.
- Lights: lantern, lantern[hanging=true] (hang it under a block), soul_lantern, torch (on top of a block), wall_torch[facing=north] (attached to the wall on the opposite side, e.g. facing=north sits on the south face of the block north of it), sea_lantern, glowstone, campfire[lit=true].
- Stairs: <type>_stairs[facing=north|south|east|west,half=bottom|top]. facing is the direction you walk UP the stairs: the tall side points that way. half=top flips it upside down (good for roof eaves and window sills).
- Slabs: <type>_slab[type=bottom|top|double].
- Logs and pillars: [axis=y] vertical, [axis=x] runs east-west, [axis=z] runs north-south.
- Doors take two blocks: setblock the lower half, then the upper half right above it, e.g. oak_door[facing=south,half=lower,hinge=left] then oak_door[facing=south,half=upper,hinge=left]. Clear the doorway first.
- Beds take two blocks: red_bed[facing=south,part=foot] and red_bed[facing=south,part=head] one block further south.
- Useful details: crafting_table, furnace[facing=south], chest[facing=south], barrel, bookshelf, flower_pot, potted_poppy, lectern, anvil, cauldron, hay_block, composter, ladder[facing=north], vine, scaffolding, oak_leaves[persistent=true] (for hedges; plain leaves decay), grass_block, dirt_path, moss_block, flowering_azalea, water, lava.
- Fences, walls and glass panes connect to neighbors automatically when placed.
- Signs: oak_sign[rotation=0..15] on the ground or oak_wall_sign[facing=...] on a wall, with text: setblock ~ ~ ~ oak_wall_sign[facing=south]{front_text:{messages:['"Welcome"','""','""','""']}}.

REDSTONE AND MECHANISMS (when asked for machines, doors, clocks, timers, displays, farms)
- Components: redstone_wire (dust; connects by itself), redstone_block (always on), redstone_torch / redstone_wall_torch[facing=...] (inverter: on unless the block it sits on is powered), lever[face=floor|wall,facing=...], stone_button[face=wall,facing=...], redstone_lamp (lights while powered), target, daylight_detector, note_block, observer[facing=...], piston / sticky_piston[facing=...], dispenser, dropper, hopper, redstone_lamp.
- Repeaters and comparators are diodes: repeater[facing=north,delay=1..4] and comparator[facing=north,mode=compare|subtract]. Their facing points toward the INPUT, and the signal comes out the opposite side. So a repeater that carries a signal from west to east uses facing=west.
- Pistons and observers: a piston's facing is the way its head pushes. An observer's facing is the side it watches, and it pulses out of the opposite side.
- Redstone sits on top of solid blocks (put a floor of smooth_stone or stone under circuits). Dust and signals travel 15 blocks; add a repeater to go further.
- Clocks: two observers facing each other, a ring of repeaters with one redstone_block to start it, or a comparator in subtract mode feeding back into itself. Always add a lever to turn machines on and off.
- Displays: build pixel or 7-segment digits from redstone_lamp blocks set into a wall of a solid block, and drive each segment with its own line; label them with signs. A counting display can use a ring of repeaters or droppers passing an item around. Keep circuits compact and hide the wiring behind or under the display.
- Circuits placed by commands start unpowered. Place blocks first, then dust, repeaters and torches, then the lever or button last, so the player can switch it on.
- Only if a request truly can't be made with redstone, you may use text_display entities for labels: summon text_display ~x ~y ~z {text:'"12:00"',billboard:"center"}.

HOW TO BUILD WELL (follow this order)
1. Choose a footprint that fits the site: avoid water, lava, trees and B cells unless the request says otherwise. Read the height map under the footprint to find its lowest and highest ground.
2. Clear the space: fill the whole footprint with air from the floor level up to the roof peak plus 1, so grass, flowers and trees don't poke through.
3. Foundation: fill from the lowest ground under the footprint up to the floor level with a solid block (cobblestone, stone_bricks, deepslate) so nothing floats. On a slope, set the floor at the median ground height, dig out the high side and build a foundation or stilts on the low side.
4. Floor, then walls. Frame corners and every 4-6 blocks along long walls with log or stone pillars, and fill between them with a contrasting material. Inset the wall infill one block from the frame for depth, or add a stair or slab trim course.
5. Openings: carve windows (2 tall, spaced every 2-4 blocks) and fill with glass or glass_pane, add trapdoor shutters or stair sills; place the door with its two halves and a step or path in front.
6. Roof: never a flat box. Use stairs in rows stepping up one block per row from both sides, with a one-block overhang past the walls (half=top stairs under the eaves), a slab or full-block ridge on top, and gable ends filled with planks or a contrasting block. Hip, gable and A-frame roofs all work.
7. Interior: floor material, lighting (a light every 6 blocks or so, so mobs can't spawn), and furniture that fits the build (bed, crafting_table, furnace, chests, bookshelves, carpets).
8. Exterior details: lanterns by the door, a dirt_path to the player, fences, flowers, leaves hedges, a chimney with a campfire on top.
- Use a palette of 3-5 materials that go together (for example spruce_log + spruce_planks + cobblestone + dark_oak stairs, or stone_bricks + deepslate_tiles + oak). Vary depth and texture; big flat walls of one block look bad.
- Size to the request: a hut is about 5x5 inside, a house 7-11 blocks, a mansion or castle up to the whole site. Keep everything symmetrical unless the style calls for otherwise.
- Only change blocks inside your footprint (plus paths and decoration next to it). Never replace B cells unless asked.
- Use at most 600 commands. Prefer fill over many setblocks. Commands run in order, so later ones overwrite earlier ones: clear, then build structure, then carve openings, then details.

OUTPUT
Don't run tools, create files or ask questions: design it in your head and answer right away with only a JSON object:
{"title": "<short name of the build>", "commands": ["fill ...", "setblock ...", ...]}
Always include commands. If the request is too big or vague, build a smaller, simpler version of it rather than nothing.`

type buildPlan struct {
	Title    string   `json:"title"`
	Commands []string `json:"commands"`
}

func parsePlan(text string) (buildPlan, error) {
	var p buildPlan
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if i < 0 || j <= i {
		return p, errors.New("the design came back empty")
	}
	if err := json.Unmarshal([]byte(text[i:j+1]), &p); err != nil {
		return p, fmt.Errorf("couldn't read the design: %v", err)
	}
	return p, nil
}

// fail reports a build problem in chat and on the AI players page.
func (b *Bot) fail(msg string) {
	b.mu.Lock()
	b.lastErr = msg
	b.mu.Unlock()
	b.say(msg)
}

func (b *Bot) setStage(s string) {
	b.mu.Lock()
	b.stage = s
	b.mu.Unlock()
}

// logBuild adds a timestamped line to the build log shown on the AI players
// page (and the Activity log), so you can see where time actually goes.
func (b *Bot) logBuild(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	b.mu.Lock()
	elapsed := time.Since(b.buildAt).Round(time.Second)
	b.buildLog = append(b.buildLog, fmt.Sprintf("%6s  %s", elapsed, msg))
	if len(b.buildLog) > 40 {
		b.buildLog = b.buildLog[len(b.buildLog)-40:]
	}
	b.mu.Unlock()
	b.m.log.Infof("AI player %s build: %s", b.Name, msg)
}

// build designs and places a build. It runs in the background so the AI
// player keeps chatting while it works.
func (b *Bot) build(desc string, legacy []string, at, from, original string, host bool) {
	if strings.TrimSpace(desc) == "" && len(legacy) == 0 {
		return
	}
	if !permitted(b.m.settings().Builds, host) {
		b.say("Sorry " + from + ", I'm not allowed to build for you.")
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, 12*time.Minute)
	b.mu.Lock()
	busy, st, stage := b.building, b.state, b.stage
	if !busy {
		b.building, b.buildDesc, b.buildFor, b.buildAt, b.cancelB = true, desc, from, time.Now(), cancel
		b.buildLog = nil
		if desc == "" {
			b.buildDesc = "a build"
		}
	}
	b.mu.Unlock()
	if busy {
		cancel()
		b.say("Still on the last build (" + stage + "), hang tight.")
		return
	}
	go func() {
		defer cancel()
		defer func() {
			b.mu.Lock()
			b.building, b.stage, b.buildDesc, b.buildFor, b.cancelB = false, "", "", "", nil
			b.mu.Unlock()
		}()
		stopped := func() bool {
			if ctx.Err() == nil {
				return false
			}
			if b.ctx.Err() == nil && errors.Is(ctx.Err(), context.Canceled) {
				b.logBuild("stopped")
				b.say("Okay, I stopped the build.")
			} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				b.logBuild("gave up after 12 minutes")
				b.say("That build took too long, so I gave up on it. Try asking for something simpler.")
			}
			return true
		}
		if st == nil {
			b.say("Give me a second to look around first.")
			return
		}
		x, y, z, dim, err := buildSpot(at, from, st)
		if err != nil {
			b.say(err.Error())
			return
		}
		if dim == "" {
			dim = "overworld"
		}
		plan := buildPlan{Title: "build", Commands: legacy}
		var prompt string
		if len(legacy) == 0 {
			b.logBuild("building %q for %s at %d %d %d", shorten(desc, 80), from, x, y, z)
			b.setStage("surveying the site")
			s, err := b.survey(ctx, x, y, z, dim)
			if err != nil {
				b.logBuild("survey failed: %v", err)
				b.say("I couldn't survey the ground there: " + shorten(err.Error(), 120))
				return
			}
			if p := st.player(from); p != nil {
				s.px, s.pz, s.hasP = int(math.Floor(p.X))-x, int(math.Floor(p.Z))-z, true
				s.face, _, _ = facing(p.Yaw)
				if abs(s.px) > siteRadius || abs(s.pz) > siteRadius {
					s.hasP = false
				}
			}
			prompt = fmt.Sprintf("BUILD REQUEST from %s: %s\n(Their chat message: %q)\n\n%s", from, desc, original, s.describe())
			b.logBuild("surveyed the site; asking %s to design it (prompt %d KB)", b.provider.Describe(), len(prompt)/1024)
			b.setStage("designing: " + shorten(desc, 60))
			start := time.Now()
			lastLine := time.Now()
			progress := func(line string) {
				b.setStage(fmt.Sprintf("designing (%s): %s", time.Since(start).Round(time.Second), shorten(line, 50)))
				if time.Since(lastLine) > 10*time.Second {
					lastLine = time.Now()
					b.logBuild("model: %s", shorten(line, 100))
				}
			}
			resp, err := b.provider.Generate(ctx, ai.Request{System: architectSystem, Prompt: prompt, MaxTokens: 32000, Schema: ai.BuildSchema, Progress: progress})
			if stopped() {
				return // cancelled, or removed while designing
			}
			if err != nil {
				b.logBuild("design failed after %s: %v", time.Since(start).Round(time.Second), err)
				b.fail("I couldn't design that: " + shorten(err.Error(), 140))
				return
			}
			if plan, err = parsePlan(resp.Text); err != nil {
				b.m.log.Warnf("AI player %s: unreadable design: %.300s", b.Name, resp.Text)
				b.fail("I couldn't design that: " + shorten(err.Error(), 140))
				return
			}
			if len(plan.Commands) == 0 {
				b.m.log.Warnf("AI player %s: design had no build steps: %.300s", b.Name, resp.Text)
				b.fail("I couldn't work out a design for that one. Try describing it differently or asking for something smaller.")
				return
			}
			b.logBuild("design %q ready after %s: %d commands%s", plan.Title, time.Since(start).Round(time.Second), len(plan.Commands), tokenNote(resp))
		}

		ok, failed := b.place(ctx, plan.Commands, x, y, z, dim)
		if stopped() {
			return
		}
		if len(failed) > 0 && prompt != "" {
			b.setStage(fmt.Sprintf("fixing %d steps", len(failed)))
			b.logBuild("asking the model to fix %d rejected steps (first: %s)", len(failed), shorten(failed[0][1], 80))
			var fb strings.Builder
			fb.WriteString(prompt)
			fb.WriteString("\n\nYou already placed this build. These commands failed; answer with corrected versions of only these commands (same JSON format, title unchanged). Leave out any that can't be fixed.\n")
			for i, f := range failed[:min(len(failed), 40)] {
				fmt.Fprintf(&fb, "%d. %s   ERROR: %s\n", i+1, f[0], f[1])
			}
			if resp, err := b.provider.Generate(ctx, ai.Request{System: architectSystem, Prompt: fb.String(), MaxTokens: 16000, Schema: ai.BuildSchema}); err == nil {
				if fix, err := parsePlan(resp.Text); err == nil && len(fix.Commands) > 0 {
					fixedOK, stillFailed := b.place(ctx, fix.Commands, x, y, z, dim)
					ok += fixedOK
					failed = stillFailed
				}
			}
		}
		title := strings.TrimSpace(plan.Title)
		if title == "" || title == "build" {
			title = "it"
		}
		b.m.log.Infof("AI player %s built %q at %d %d %d: %d ok, %d failed", b.Name, title, x, y, z, ok, len(failed))
		if stopped() {
			return
		}
		switch {
		case ok == 0:
			msg := "The build didn't work."
			if len(failed) > 0 {
				msg += " The game said: " + shorten(failed[0][1], 140)
			}
			b.fail(msg)
		case len(failed) == 0:
			b.say("Done! Built " + title + ".")
		default:
			b.say(fmt.Sprintf("Built %s, though %d small parts didn't work out.", title, len(failed)))
		}
		b.note(fmt.Sprintf("(you built %s at %d %d %d)", title, x, y, z))
	}()
}

// place checks and runs a plan's commands at the build spot. It returns how
// many worked and, for the rest, the command and the game's error. Batches
// the game doesn't confirm in time (heavy fills can lag it) are logged, not
// sent back for "repair", so nothing gets placed twice.
func (b *Bot) place(ctx context.Context, cmds []string, x, y, z int, dim string) (ok int, failed [][2]string) {
	if !strings.Contains(dim, ":") {
		dim = "minecraft:" + dim
	}
	var inner, wrapped []string
	for _, c := range cmds[:min(len(cmds), maxBuildCmds)] {
		clean, err := checkBuild(c)
		if err != nil {
			failed = append(failed, [2]string{c, err.Error()})
			continue
		}
		inner = append(inner, clean)
		wrapped = append(wrapped, fmt.Sprintf("execute in %s positioned %d %d %d run %s", dim, x, y, z, clean))
	}
	if n := len(cmds) - len(wrapped); n > 0 {
		b.logBuild("skipped %d commands that aren't allowed in builds", n)
	}
	const batch = 20
	unconfirmed := 0
	began := time.Now()
	for start := 0; start < len(wrapped) && ctx.Err() == nil; start += batch {
		end := min(start+batch, len(wrapped))
		b.setStage(fmt.Sprintf("placing blocks (%d/%d)", start, len(wrapped)))
		var res runResult
		bctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		t0 := time.Now()
		err := b.m.call(bctx, "run", map[string]any{"name": b.Name, "commands": wrapped[start:end], "as_bot": false}, &res)
		cancel()
		if took := time.Since(t0); took > 5*time.Second {
			b.logBuild("the game took %s to place steps %d-%d (big fills make it lag)", took.Round(time.Second), start+1, end)
		}
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			unconfirmed += end - start
			b.logBuild("steps %d-%d: %v", start+1, end, err)
			continue
		}
		for i, r := range res.Results {
			if start+i >= end {
				break
			}
			if r.OK {
				ok++
			} else {
				failed = append(failed, [2]string{inner[start+i], r.Error})
			}
		}
	}
	b.logBuild("placed %d of %d steps in %s (%d rejected by the game, %d not confirmed)", ok, len(wrapped), time.Since(began).Round(time.Second), len(failed), unconfirmed)
	return ok, failed
}

func tokenNote(r ai.Response) string {
	switch {
	case r.TotalTokens > 0:
		return fmt.Sprintf(", %d tokens", r.TotalTokens)
	case r.InputTokens+r.OutputTokens > 0:
		return fmt.Sprintf(", %d tokens", r.InputTokens+r.OutputTokens)
	}
	return ""
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

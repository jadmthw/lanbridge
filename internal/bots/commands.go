package bots

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Who may ask AI players to run commands or build.
const (
	PermNobody   = "off"
	PermHost     = "host"
	PermEveryone = "everyone"
)

func permitted(setting string, host bool) bool {
	switch setting {
	case PermEveryone:
		return true
	case PermHost:
		return host
	}
	return false
}

// blocked commands are never run by AI players, whoever asks: they could
// take over or wreck the server, or escape LANBridge's checks.
var blocked = map[string]bool{
	"op": true, "deop": true, "stop": true, "ban": true, "ban-ip": true, "banlist": true, "pardon": true, "pardon-ip": true,
	"kick": true, "whitelist": true, "reload": true, "script": true, "carpet": true, "player": true, "publish": true,
	"save-off": true, "save-on": true, "save-all": true, "datapack": true, "debug": true, "perf": true, "jfr": true,
	"transfer": true, "function": true, "setidletimeout": true, "defaultgamemode": true,
}

var buildCommands = map[string]bool{"fill": true, "setblock": true, "clone": true}

func cmdName(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(f[0]), "minecraft:")
}

// checkCommand cleans up a command from the model and refuses dangerous ones,
// including ones hidden inside /execute … run.
func checkCommand(cmd string) (string, error) {
	cmd = strings.TrimSpace(cmd)
	cmd = strings.TrimSpace(strings.TrimPrefix(cmd, "/"))
	if cmd == "" {
		return "", errors.New("empty command")
	}
	if len(cmd) > 1000 || strings.ContainsAny(cmd, "\r\n") {
		return "", errors.New("command too long")
	}
	name := cmdName(cmd)
	if blocked[name] {
		return "", fmt.Errorf("/%s isn't allowed for AI players", name)
	}
	if name == "execute" {
		lower := " " + strings.ToLower(cmd) + " "
		if strings.Contains(lower, " function ") {
			return "", errors.New("/execute … function isn't allowed for AI players")
		}
		if i := strings.LastIndex(lower, " run "); i >= 0 {
			if _, err := checkCommand(strings.TrimSpace(lower[i+5:])); err != nil {
				return "", err
			}
		}
	}
	return cmd, nil
}

func checkBuild(cmd string) (string, error) {
	cmd, err := checkCommand(cmd)
	if err != nil {
		return "", err
	}
	name := cmdName(cmd)
	if name == "summon" {
		f := strings.Fields(cmd)
		if len(f) > 1 && decorEntities[strings.TrimPrefix(strings.ToLower(f[1]), "minecraft:")] {
			return cmd, nil
		}
		return "", errors.New("builds can only summon decoration entities like text_display or armor_stand")
	}
	if !buildCommands[name] {
		return "", fmt.Errorf("builds only use fill, setblock and clone (got /%s)", name)
	}
	return cmd, nil
}

var decorEntities = map[string]bool{"text_display": true, "block_display": true, "item_display": true, "armor_stand": true,
	"item_frame": true, "glow_item_frame": true, "painting": true}

// facing turns a Minecraft yaw into a compass direction and a unit step.
func facing(yaw float64) (name string, dx, dz int) {
	a := math.Mod(math.Mod(yaw, 360)+360, 360)
	switch {
	case a >= 45 && a < 135:
		return "west", -1, 0
	case a >= 135 && a < 225:
		return "north", 0, -1
	case a >= 225 && a < 315:
		return "east", 1, 0
	}
	return "south", 0, 1
}

// buildSpot picks where a build goes: on the ground a few blocks in front of
// a player (the one who asked, by default), or at explicit coordinates.
func buildSpot(at, requester string, st *State) (x, y, z int, dim string, err error) {
	at = strings.TrimSpace(at)
	if f := strings.Fields(strings.NewReplacer(",", " ").Replace(at)); len(f) == 3 {
		var v [3]float64
		ok := true
		for i := range f {
			if v[i], err = strconv.ParseFloat(f[i], 64); err != nil {
				ok = false
			}
		}
		if ok {
			return int(math.Floor(v[0])), int(math.Floor(v[1])), int(math.Floor(v[2])), st.Dim, nil
		}
	}
	who := requester
	switch strings.ToLower(at) {
	case "", "me", "requester", "here":
	case "you", "self", "bot":
		_, dx, dz := facing(st.Yaw)
		return int(math.Floor(st.X)) + spotAhead*dx, int(math.Floor(st.Y)), int(math.Floor(st.Z)) + spotAhead*dz, st.Dim, nil
	default:
		who = at
	}
	p := st.player(who)
	if p == nil {
		return 0, 0, 0, "", fmt.Errorf("I can't see %s to build next to them", who)
	}
	_, dx, dz := facing(p.Yaw)
	return int(math.Floor(p.X)) + spotAhead*dx, int(math.Floor(p.Y)), int(math.Floor(p.Z)) + spotAhead*dz, p.Dim, nil
}

type runResult struct {
	Results []struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	} `json:"results"`
}

// runCommands runs checked commands in batches, one game tick per batch.
func (b *Bot) runCommands(ctx context.Context, cmds []string, asBot bool) (ok int, firstErr string) {
	for len(cmds) > 0 {
		n := min(len(cmds), 40)
		var res runResult
		err := b.m.call(ctx, "run", map[string]any{"name": b.Name, "commands": cmds[:n], "as_bot": asBot}, &res)
		if err != nil {
			return ok, err.Error()
		}
		for _, r := range res.Results {
			if r.OK {
				ok++
			} else if firstErr == "" {
				firstErr = r.Error
			}
		}
		cmds = cmds[n:]
	}
	return ok, firstErr
}

// doCommands runs the model's "commands" as this AI player.
func (b *Bot) doCommands(ctx context.Context, cmds []string, from string, host bool) {
	if len(cmds) == 0 {
		return
	}
	if !permitted(b.m.settings().Commands, host) {
		b.say("Sorry " + from + ", I'm not allowed to run commands for you.")
		return
	}
	var ok []string
	for _, c := range cmds[:min(len(cmds), 20)] {
		if clean, err := checkCommand(c); err != nil {
			b.say("I can't do that: " + err.Error())
		} else {
			ok = append(ok, clean)
		}
	}
	if len(ok) == 0 {
		return
	}
	done, firstErr := b.runCommands(ctx, ok, true)
	b.m.log.Infof("AI player %s ran %d/%d commands for %s", b.Name, done, len(ok), from)
	if firstErr != "" {
		b.say("That didn't fully work: " + shorten(firstErr, 150))
	}
	b.note(fmt.Sprintf("(you ran %d of %d commands)", done, len(ok)))
}

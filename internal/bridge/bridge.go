// Package bridge connects LANBridge to a Minecraft world running the Carpet
// mod and LANBridge's Scarpet app (lanbridge.sc). The app and LANBridge
// exchange small JSON files in <world>/scripts/lanbridge.data: LANBridge
// writes command batches to in/, the app writes event batches to out/ and
// refreshes hello.json every half second while the world is open.
package bridge

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"lanbridge/internal/logx"
)

// Script is the Scarpet app that runs inside Minecraft.
//
//go:embed lanbridge.sc
var Script []byte

// AppName is the Scarpet app's name, as typed in /script load.
const AppName = "lanbridge"

// ScriptVersion is the version of lanbridge.sc this LANBridge expects; the
// script reports its own in hello.json.
const ScriptVersion = 3

// ErrOldScript explains errors from a script that predates a feature.
var ErrOldScript = errors.New("the LANBridge script in your world is out of date. On the AI players page click Reinstall, then run /script load lanbridge in the game")

// Hello is the app's heartbeat.
type Hello struct {
	V       int      `json:"v"`
	Session string   `json:"session"`
	Time    float64  `json:"time"`
	World   string   `json:"world"`
	Players []string `json:"players"`
	Hosts   []string `json:"hosts"`
	Fakes   []string `json:"fakes"`
}

// Event is something that happened in the world.
type Event struct {
	Type    string          `json:"type"` // chat, join, leave, death, result
	Player  string          `json:"player,omitempty"`
	Message string          `json:"message,omitempty"`
	Host    Flag            `json:"host,omitempty"`
	Fake    Flag            `json:"fake,omitempty"`
	ID      int64           `json:"id,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Flag decodes Scarpet booleans, which may arrive as true/false or 1/0.
type Flag bool

func (f *Flag) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	*f = Flag(s == "true" || (s != "false" && s != "null" && s != "0" && s != "0.0" && s != `""`))
	return nil
}

// ErrPaused means the game isn't ticking, so it can't run commands right now.
var ErrPaused = errors.New("Minecraft is paused. Single-player worlds pause when you switch to another window, so go back to the game, or open the world to LAN (or press F3+P in game) so it keeps running in the background")

// ErrClosed is returned once the world has closed or the bridge was stopped.
var ErrClosed = errors.New("the Minecraft world isn't connected")

// Stale is how fresh a heartbeat must be to connect to a world.
const Stale = 12 * time.Second

// Gone is how long a connected world may stay silent (paused, loading)
// before it counts as closed.
const Gone = 2 * time.Minute

// PausedAfter is how long without a heartbeat before the game counts as paused.
const PausedAfter = 3 * time.Second

// Client is a live connection to one world.
type Client struct {
	Dir   string // .../scripts/lanbridge.data
	World string
	log   *logx.Logger

	events chan Event
	done   chan struct{}
	once   sync.Once
	stamp  string

	mu      sync.Mutex
	seq     int64
	nextID  int64
	pending []map[string]any
	waiters map[int64]chan json.RawMessage
	kick    chan struct{}
	err     error
	beat    time.Time
}

// Open starts talking to the app whose data folder is dir.
func Open(dir string, log *logx.Logger) (*Client, error) {
	h, err := ReadHello(dir)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"in", "out"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	c := &Client{Dir: dir, World: h.World, log: log, events: make(chan Event, 256), done: make(chan struct{}),
		stamp: fmt.Sprintf("%013d", time.Now().UnixMilli()), waiters: map[int64]chan json.RawMessage{}, kick: make(chan struct{}, 1), beat: time.Now()}
	// Events written before we connected are old news.
	if old, _ := filepath.Glob(filepath.Join(dir, "out", "*.json")); len(old) > 0 {
		for _, f := range old {
			os.Remove(f)
		}
	}
	go c.loop()
	return c, nil
}

// ReadHello reads the heartbeat file and checks that it's fresh.
func ReadHello(dir string) (Hello, error) {
	p := filepath.Join(dir, "hello.json")
	st, err := os.Stat(p)
	if err != nil {
		return Hello{}, err
	}
	if time.Since(st.ModTime()) > Stale {
		return Hello{}, fmt.Errorf("world closed (no heartbeat for %s)", time.Since(st.ModTime()).Round(time.Second))
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return Hello{}, err
	}
	var h Hello
	if err := json.Unmarshal(b, &h); err != nil {
		return Hello{}, err
	}
	return h, nil
}

// LastBeat is when the game last wrote its heartbeat.
func (c *Client) LastBeat() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.beat
}

// Paused reports whether the game has stopped ticking for a moment, which
// is what single-player Minecraft does when it isn't in focus.
func (c *Client) Paused() bool { return time.Since(c.LastBeat()) > PausedAfter }

// Events delivers chat, join, leave and death events.
func (c *Client) Events() <-chan Event { return c.events }

// Done is closed when the connection ends; Err says why.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is the reason the connection ended.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close stops the client.
func (c *Client) Close() { c.fail(ErrClosed) }

func (c *Client) fail(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		for id, w := range c.waiters {
			close(w)
			delete(c.waiters, id)
		}
		c.mu.Unlock()
		close(c.done)
	})
}

// Send queues a command without waiting for its result.
func (c *Client) Send(op string, args map[string]any) {
	cmd := map[string]any{"op": op}
	for k, v := range args {
		cmd[k] = v
	}
	c.mu.Lock()
	c.pending = append(c.pending, cmd)
	c.mu.Unlock()
	c.wake()
}

// Call runs a command and decodes its result into out (if not nil).
func (c *Client) Call(ctx context.Context, op string, args map[string]any, out any) error {
	cmd := map[string]any{"op": op}
	for k, v := range args {
		cmd[k] = v
	}
	ch := make(chan json.RawMessage, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.nextID++
	id := c.nextID
	cmd["id"] = id
	c.waiters[id] = ch
	c.pending = append(c.pending, cmd)
	c.mu.Unlock()
	c.wake()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 6*time.Second) // callers with heavier work pass their own deadline
		defer cancel()
	}
	select {
	case data, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			if strings.HasPrefix(e.Error, "unknown op") {
				return ErrOldScript
			}
			return errors.New(e.Error)
		}
		if out != nil {
			return json.Unmarshal(data, out)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			if c.Paused() {
				return ErrPaused
			}
			return errors.New("Minecraft didn't answer. If you typed /script load lanbridge, check the game chat for a red error message")
		}
		return ctx.Err()
	case <-c.done:
		return ErrClosed
	}
}

func (c *Client) wake() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *Client) loop() {
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	lastHello := time.Now()
	for {
		select {
		case <-c.done:
			return
		case <-c.kick:
			time.Sleep(15 * time.Millisecond) // let a burst of commands share one file
			c.flush()
		case <-poll.C:
			c.flush()
			c.readEvents()
			if time.Since(lastHello) > time.Second {
				lastHello = time.Now()
				st, err := os.Stat(filepath.Join(c.Dir, "hello.json"))
				if err != nil {
					c.fail(fmt.Errorf("%w: %v", ErrClosed, err))
					return
				}
				c.mu.Lock()
				c.beat = st.ModTime()
				c.mu.Unlock()
				if time.Since(st.ModTime()) > Gone {
					c.fail(fmt.Errorf("%w: no heartbeat for %s", ErrClosed, time.Since(st.ModTime()).Round(time.Second)))
					return
				}
			}
		}
	}
}

func (c *Client) flush() {
	c.mu.Lock()
	cmds := c.pending
	c.pending = nil
	if len(cmds) == 0 {
		c.mu.Unlock()
		return
	}
	c.seq++
	name := fmt.Sprintf("c%s_%09d", c.stamp, c.seq)
	c.mu.Unlock()
	b, err := json.Marshal(map[string]any{"cmds": cmds})
	if err != nil {
		return
	}
	tmp := filepath.Join(c.Dir, "in", name+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		err = os.Rename(tmp, filepath.Join(c.Dir, "in", name+".json"))
		if err != nil {
			os.Remove(tmp)
		}
	}
}

func (c *Client) readEvents() {
	files, _ := filepath.Glob(filepath.Join(c.Dir, "out", "*.json"))
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var batch struct {
			Events []Event `json:"events"`
		}
		if err := json.Unmarshal(b, &batch); err != nil {
			if st, serr := os.Stat(f); serr == nil && time.Since(st.ModTime()) > 3*time.Second {
				os.Remove(f) // corrupt and not being written any more
			}
			continue // probably still being written
		}
		os.Remove(f)
		for _, ev := range batch.Events {
			if ev.Type == "result" {
				c.mu.Lock()
				w := c.waiters[ev.ID]
				delete(c.waiters, ev.ID)
				c.mu.Unlock()
				if w != nil {
					w <- ev.Data
				}
				continue
			}
			select {
			case c.events <- ev:
			default:
				c.log.Warnf("AI bridge: dropped a game event (too many at once)")
			}
		}
	}
}

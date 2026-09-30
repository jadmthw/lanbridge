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

// ErrClosed is returned once the world has closed or the bridge was stopped.
var ErrClosed = errors.New("the Minecraft world isn't connected")

// Stale is how long without a heartbeat before the world counts as closed.
const Stale = 12 * time.Second

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
		stamp: fmt.Sprintf("%013d", time.Now().UnixMilli()), waiters: map[int64]chan json.RawMessage{}, kick: make(chan struct{}, 1)}
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
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	select {
	case data, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
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
			return errors.New("Minecraft didn't answer (is the game paused or the world closed?)")
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
				if _, err := ReadHello(c.Dir); err != nil {
					c.fail(fmt.Errorf("%w: %v", ErrClosed, err))
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

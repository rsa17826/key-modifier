package keyModifierLib

import (
	"sync"
	"time"
)

// ── Config types ─────────────────────────────────────────────────────────────

type TurboConfig struct {
	DownFor time.Duration
	Delay   time.Duration
}

type DelayConfig struct {
	Down time.Duration
	Up   time.Duration
}

type KeyModifier struct {
	DeviceID    string // "" = match events from any device; else only events from this device
	Toggle      bool
	Invert      bool     // swap down↔up before any other modifier sees the event
	ReplaceWith []uint16 // nil = emit the physical key; non-nil = emit this code instead
	// ReplaceDeviceID, if set, is the device id stamped on injected events
	// instead of the physical event's own origin device — i.e. "replace y
	// from dev2" makes the injected y look like it came from dev2 rather
	// than from whatever device x itself was pressed on.
	ReplaceDeviceID string
	Turbo           *TurboConfig
	Delay           *DelayConfig
	MaxPressTime    time.Duration // 0 = disabled
	MinPressTime    time.Duration // 0 = disabled
	TakeOver        bool
	// Combo, if non-nil, makes this modifier emit a sequence of keys (a key
	// combo) instead of a single replacement key. On physical down, each
	// code is injected down in order; on physical up, they're injected up
	// in reverse order. Combo takes priority over ReplaceWith.
	Combo []uint16
}

// ── Runtime state per key ────────────────────────────────────────────────────

type KeyState struct {
	mu           sync.Mutex
	toggled      bool          // toggle mode: is key currently "held"
	isDown       bool          // physical key currently pressed
	pressedAt    time.Time     // when the physical key went down
	turboStop    chan struct{} // close to stop turbo goroutine
	maxPressStop chan struct{} // close to cancel maxPressTime timer
	suppressUp   bool          // maxPressTime fired; eat the real up event

	// injectQueue serializes the plain (non-turbo) down/up injections for
	// this key. Down and up are each handled by their own ad-hoc goroutine
	// (so delay/minPressTime waits don't block the event reader), but Go
	// gives no ordering guarantee between two independently spawned
	// goroutines. Without this queue, a fast down-then-up could race such
	// that the "up" inject reaches the wire before the "down" inject does,
	// leaving the emitted key stuck "down" (which then free-runs via the
	// OS's own key-repeat until the key is pressed again). Jobs are
	// enqueued synchronously, in event order, from processKeyEvent, and a
	// single worker drains them one at a time, so the wire order always
	// matches the physical event order.
	injectQueue     chan func()
	injectQueueOnce sync.Once
}

// ModKey identifies a modifier slot: a physical key code plus the optional
// device filter it was configured with. A given key can have both a
// device-specific modifier ("x from dev1 ...") and a wildcard modifier
// ("x ...") registered at once; lookupMod prefers the exact device match.
type ModKey struct {
	Code   uint16
	Device string
}

// ── Runtime state per key ────────────────────────────────────────────────────

type keyState struct {
	mu           sync.Mutex
	toggled      bool          // toggle mode: is key currently "held"
	isDown       bool          // physical key currently pressed
	pressedAt    time.Time     // when the physical key went down
	turboStop    chan struct{} // close to stop turbo goroutine
	maxPressStop chan struct{} // close to cancel maxPressTime timer
	suppressUp   bool          // maxPressTime fired; eat the real up event

	// injectQueue serializes the plain (non-turbo) down/up injections for
	// this key. Down and up are each handled by their own ad-hoc goroutine
	// (so delay/minPressTime waits don't block the event reader), but Go
	// gives no ordering guarantee between two independently spawned
	// goroutines. Without this queue, a fast down-then-up could race such
	// that the "up" inject reaches the wire before the "down" inject does,
	// leaving the emitted key stuck "down" (which then free-runs via the
	// OS's own key-repeat until the key is pressed again). Jobs are
	// enqueued synchronously, in event order, from processKeyEvent, and a
	// single worker drains them one at a time, so the wire order always
	// matches the physical event order.
	injectQueue     chan func()
	injectQueueOnce sync.Once
}

// enqueueInject starts the worker goroutine (once) and appends a job. Jobs
// run strictly in the order they were enqueued.
func (s *keyState) enqueueInject(job func()) {
	s.injectQueueOnce.Do(func() {
		s.injectQueue = make(chan func(), 16)
		go func() {
			for j := range s.injectQueue {
				j()
			}
		}()
	})
	s.injectQueue <- job
}

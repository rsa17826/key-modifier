package keyModifierLib

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	input "github.com/rsa17826/go-input-lib"
	"github.com/rsa17826/input-manager/IMan"
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

// enqueueInject starts the worker goroutine (once) and appends a job. Jobs
// run strictly in the order they were enqueued.
func (s *KeyState) enqueueInject(job func()) {
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

// ── Globals ──────────────────────────────────────────────────────────────────

var (
	iMan     *IMan.ManagerConnection
	statesMu sync.Mutex
	states   = make(map[ModKey]*KeyState)

	// takeoverMu guards the takeover* globals below, which track a single
	// currently-active "combo takeover": while active, every key event
	// except the combo's own trigger key is intercepted (never forwarded)
	// and just recorded into takeoverLive as down/up, so that at release
	// time we know exactly which keys are still physically down and
	// should be restored — as opposed to blindly restoring whatever was
	// down when the combo started.
	takeoverMu          sync.Mutex
	takeoverActive      bool
	takeoverOwnerCode   uint16
	takeoverOwnerDevice string
	takeoverLive        map[uint16]bool
)

// getState returns the KeyState for a (physical code, device) pair, creating
// it on first use. State is scoped per-device so the same physical key on
// two different devices never shares turbo/press-timing state.
func getState(code uint16, device string) *KeyState {
	statesMu.Lock()
	defer statesMu.Unlock()
	mk := ModKey{Code: code, Device: device}
	if s, ok := states[mk]; ok {
		return s
	}
	s := &KeyState{}
	states[mk] = s
	return s
}

// ── Event helpers ─────────────────────────────────────────────────────────────

func wireEvent(code uint16, val int32, deviceID string) IMan.WireEvent {
	t := time.Now()
	ev := IMan.WireEvent{
		Sec:   t.Unix(),
		Usec:  int64(t.Nanosecond() / 1000),
		Type:  input.EV_KEY,
		Code:  code,
		Value: val,
	}
	ev.SetDeviceID(deviceID)
	return ev
}

func inject(code uint16, val int32, deviceID string) {
	iMan.Send(wireEvent(code, val, deviceID))
}

func closeChan(ch *chan struct{}) {
	if *ch != nil {
		close(*ch)
		*ch = nil
	}
}

// ── Turbo goroutine ───────────────────────────────────────────────────────────
//
// Sends rapid key down/up pairs until the returned stop channel is closed.
// Sends a final key-up when stopped so the virtual device never gets stuck.

func startTurbo(code uint16, deviceID string, cfg *TurboConfig) chan struct{} {
	stop := make(chan struct{})
	go func() {
		for {
			inject(code, 1, deviceID)
			select {
			case <-stop:
				inject(code, 0, deviceID)
				return
			case <-time.After(cfg.DownFor):
			}
			inject(code, 0, deviceID)
			select {
			case <-stop:
				return
			case <-time.After(cfg.Delay):
			}
		}
	}()
	return stop
}

// ── Key event processor ───────────────────────────────────────────────────────
//
// Called synchronously (im.BlockInput already sent) so state mutations are
// ordered. Long waits / injects happen in spawned goroutines.

// handleCombo implements "replace combo [takeover] <key1> <key2> ...".
//
// Without takeover: on physical down, the combo keys are injected down in
// order; on physical up, they're injected up in reverse order.
//
// With takeover: on physical down, every key currently asserted on the
// virtual output device is released, and the combo takes exclusive
// ownership of input — every other key event is intercepted (see the
// dispatch loop) and just recorded as down/up in takeoverLive, rather than
// forwarded or processed normally. This means a key that was down at combo
// start but gets released mid-combo is correctly *not* restored, and a key
// that gets freshly pressed mid-combo *is* restored if it's still down when
// the combo ends — the restore reflects live state at release time, not a
// stale snapshot from when the combo began.

// ── CLI parsing ───────────────────────────────────────────────────────────────
//
// Syntax:  --modify <key> [to] <modifier> [options]  (repeatable)
//
// Modifiers:
//   toggle
//   turbo  [downFor <d>] [delay <d>]
//   delay  [down <d>]   [up <d>]
//   maxPressTime <d>
//   minPressTime <d>
//
// Multiple --modify flags for the same key are merged (modifiers stack).

// lookupMod finds the modifier that applies to an event with the given
// physical key code and origin device. A modifier configured with an
// explicit "from <deviceID>" only matches events from that device; a
// modifier with no device filter (the common case) matches events from
// any device. An exact device match takes priority over the wildcard.

// ── Display helpers ───────────────────────────────────────────────────────────

func printUsage() {
	fmt.Print(`keymod — intercept and transform keyboard/mouse events

Usage:
  keymod --modify <key> [to] <modifier> [options] [--modify ...]

Modifiers:
  from <deviceID>
      Only apply this modifier to events from a specific device (the id
      you passed to --keyboard/--mouse on the server). May appear anywhere
      in the modifier list. Omit it to match the key on any device. If a
      key has both a "from"-scoped and an unscoped modifier, the
      device-specific one wins for events from that device.

  invert
      Swap down and up events before any other modifier sees them.
      Key fires while NOT held.  Combine with turbo for inverted turbo.

  replace <targetKey> [from <deviceID>]
      Emit targetKey instead of the physical key.  Affects all injected
      events including turbo pulses.  The optional "from <deviceID>"
      right after the target key makes the injected event claim to
      originate from that device instead of the device the physical key
      itself was pressed on — e.g. "x from dev1 replace y from dev2"
      means: on press of x from dev1, send y as if it came from dev2.
      This is separate from the top-level "from" clause, which only
      filters which device this whole modifier applies to.

  toggle
      Press once to hold the key down; press again to release.

  turbo [downFor <d>] [delay <d>]
      Rapidly fire key down/up pairs.  While held (plain key) or
      while toggled on (toggle key) or while NOT held (invert key).
      Defaults: downFor=10ms, delay=10ms.

  delay [down <d>] [up <d>]
      Add a fixed delay before the down event, the up event, or both.

  maxPressTime <d>
      Cap how long a key press registers.  If you hold longer than <d>,
      a synthetic up is sent at <d> and the real up is suppressed.

  minPressTime <d>
      Extend short presses.  If you release before <d>, the up event
      is held back until <d> has elapsed from when the key went down.

Multiple --modify flags for the same key stack their modifiers.

Examples:
  --modify z to toggle
  --modify x to maxPressTime 1s minPressTime 100ms
  --modify v to turbo downFor 10ms delay 10ms
  --modify c to delay down 1s up 3s
  --modify b to toggle --modify b to turbo downFor 10ms delay 10ms
  --modify z to replace x                        (z sends x)
  --modify z to turbo --modify z to replace x    (holding z turbos x)
  --modify z to invert                           (fires while NOT held)
  --modify z to invert --modify z to turbo       (turbos z while z not held)
  --modify x from dev1 replace y                 (x from dev1 sends y)
  --modify x from dev1 replace y from dev2       (x from dev1 sends y,
                                                    tagged as if from dev2)
  --modify x from dev2 turbo                     (x from dev2 turbos)
  --modify x turbo --modify x from dev2 replace y (x turbos everywhere;
                                                    from dev2 it also sends y)
`)
}

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	// var toggles
	// argparse.ParseArgs([]argparse.ArgumentData{
	// 	{Keys: []string{"toggle"}, AfterCount: 1, Target: &toggles, Description: "Press once to hold the key down; press again to release.", VarArgs: false, AllowDupes: true, ExampleArgs: []string{"f1"}},
	// 	{Keys: []string{"turbo"}, AfterCount: 3, Target: &toggles, Description: "", VarArgs: false, AllowDupes: true, ExampleArgs: []string{"1s"}},
	// 	{Keys: []string{"maxPressTime"}, AfterCount: 2, Target: &toggles, Description: "Cap how long a key press registers.  If you hold longer than <d>,\na synthetic up is sent at <d> and the real up is suppressed.", VarArgs: false, AllowDupes: true, ExampleArgs: []string{"1s"}},
	// })

	keyMods := ParseModifyArgs(os.Args)
	// fmt.Printf("%#v", keyMods[input.KEY_Z])
	if len(keyMods) == 0 {
		printUsage()
		// argparse.PrintHelpAndExit()
		return
	}

	fmt.Println("Active modifications:")
	for mk, mod := range keyMods {
		keyName := input.KeyToString[mk.Code]
		if keyName == "" {
			keyName = fmt.Sprintf("code(%d)", mk.Code)
		}
		fmt.Printf("  %-14s %s\n", keyName+":", ModDesc(mod))
	}
	fmt.Println()

	var err error
	iMan, err = IMan.Connect("key modifier", IMan.ModeInjection, IMan.ModeFilter, IMan.ModeListen, IMan.ModeVirtListen)
	if err != nil {
		panic(err)
	}
	// autoRead=false: our own event loop below calls ReadNext, which
	// updates the keymap as a side effect. We need this (rather than our
	// own heldKeys bookkeeping) because combo takeover must know about
	// keys that are physically held but were never modified/injected by
	// us (e.g. a passthrough Shift) — those never go through inject().
	if err := iMan.EnableKeyMap(false); err != nil {
		panic(err)
	}
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGABRT)
		<-sigChan
		iMan.Close()
		os.Exit(0)
	}()
	// Invert+turbo (no toggle) keys are virtually "down" at startup because
	// the physical key is up = inverted = down.  Start turbo immediately.

	select {}
}

package keyModifierLib

import (
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
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
	Invert      bool    // swap down↔up before any other modifier sees the event
	ReplaceWith *uint16 // nil = emit the physical key; non-nil = emit this code instead
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

func processKeyEvent(code uint16, val int32, deviceID string, mod *KeyModifier) {
	// ── invert: flip down↔up before anything else sees the signal ────────────
	if mod.Invert && val != 2 {
		if val == 0 {
			val = 1
		} else {
			val = 0
		}
	}

	// ── replace: all injected events use outCode instead of the physical key ──
	outCode := code
	if mod.ReplaceWith != nil {
		outCode = *mod.ReplaceWith
	}

	// outDeviceID is what injected events claim to originate from. Normally
	// that's just the physical event's own device, but "replace y from dev2"
	// overrides it so y is emitted as if it came from dev2.
	outDeviceID := deviceID
	if mod.ReplaceDeviceID != "" {
		outDeviceID = mod.ReplaceDeviceID
	}

	// State is always keyed on the physical code (and device) so physical
	// up/down stay paired and independent devices don't share state.
	s := getState(code, deviceID)

	// ── combo: emit a key combo instead of a single key ──────────────────────
	// Turbo/toggle/delay/press-time modifiers don't apply to combos; a combo
	// is just "press these keys together while the physical key is held".
	if mod.Combo != nil {
		handleCombo(code, deviceID, val, outDeviceID, mod, s)
		return
	}

	switch val {
	// ── repeat ────────────────────────────────────────────────────────────────
	case 2:
		// OS-generated repeats are suppressed for all modified keys;
		// turbo and toggle implement their own repeat semantics.
		return

	// ── key down ──────────────────────────────────────────────────────────────
	case 1:
		s.mu.Lock()
		s.isDown = true
		s.pressedAt = time.Now()

		if mod.Toggle {
			s.toggled = !s.toggled
			active := s.toggled
			s.mu.Unlock()

			if active {
				// Toggle just turned ON
				if mod.Turbo != nil {
					s.mu.Lock()
					s.turboStop = startTurbo(outCode, outDeviceID, mod.Turbo)
					s.mu.Unlock()
				} else {
					downDelay := time.Duration(0)
					if mod.Delay != nil {
						downDelay = mod.Delay.Down
					}
					go func() {
						if downDelay > 0 {
							time.Sleep(downDelay)
						}
						inject(outCode, 1, outDeviceID)
					}()
				}
			} else {
				// Toggle just turned OFF
				if mod.Turbo != nil {
					s.mu.Lock()
					closeChan(&s.turboStop)
					s.mu.Unlock()
				} else {
					upDelay := time.Duration(0)
					if mod.Delay != nil {
						upDelay = mod.Delay.Up
					}
					go func() {
						if upDelay > 0 {
							time.Sleep(upDelay)
						}
						inject(outCode, 0, outDeviceID)
					}()
				}
			}
			return
		}

		s.mu.Unlock()

		// Non-toggle: turbo while held
		if mod.Turbo != nil {
			s.mu.Lock()
			pressedAt := s.pressedAt
			s.turboStop = startTurbo(outCode, outDeviceID, mod.Turbo)
			maxPress := mod.MaxPressTime
			if maxPress > 0 {
				maxStop := make(chan struct{})
				s.maxPressStop = maxStop
				s.mu.Unlock()
				go func() {
					// Account for any time already elapsed since the physical key-down
					// (e.g. if downDelay were ever added to the turbo path in the future).
					elapsed := time.Since(pressedAt)
					remaining := maxPress - elapsed
					if remaining < 0 {
						remaining = 0
					}
					select {
					case <-maxStop:
						// Real up arrived first; timer cancelled.
					case <-time.After(remaining):
						s.mu.Lock()
						if s.isDown {
							s.suppressUp = true
							closeChan(&s.turboStop) // stops the turbo goroutine
						}
						s.mu.Unlock()
					}
				}()
			} else {
				s.mu.Unlock()
			}
			return
		}

		// Plain key with optional delay + maxPressTime
		downDelay := time.Duration(0)
		if mod.Delay != nil {
			downDelay = mod.Delay.Down
		}
		maxPress := mod.MaxPressTime

		s.enqueueInject(func() {
			if downDelay > 0 {
				time.Sleep(downDelay)
			}
			inject(outCode, 1, outDeviceID)

			if maxPress > 0 {
				maxStop := make(chan struct{})
				s.mu.Lock()
				s.maxPressStop = maxStop
				s.mu.Unlock()

				go func() {
					select {
					case <-maxStop:
						// Real up arrived first; timer cancelled.
					case <-time.After(maxPress):
						s.mu.Lock()
						if s.isDown {
							s.suppressUp = true
							s.mu.Unlock()
							inject(outCode, 0, outDeviceID) // synthetic up at cap
						} else {
							s.mu.Unlock()
						}
					}
				}()
			}
		})

	// ── key up ────────────────────────────────────────────────────────────────
	case 0:
		s.mu.Lock()
		s.isDown = false

		// Toggle mode: physical ups are always eaten (toggle manages its own up)
		if mod.Toggle {
			s.mu.Unlock()
			return
		}

		// Stop turbo if running
		if s.turboStop != nil {
			closeChan(&s.turboStop)
			// Also cancel the maxPressTime timer so it doesn't fire after release.
			if s.maxPressStop != nil {
				closeChan(&s.maxPressStop)
			}
			s.mu.Unlock()
			return
		}

		// Cancel maxPressTime timer (if key released before cap)
		if s.maxPressStop != nil {
			closeChan(&s.maxPressStop)
		}

		suppress := s.suppressUp
		s.suppressUp = false
		pressedAt := s.pressedAt
		s.mu.Unlock()

		// maxPressTime already sent a synthetic up; swallow the real one
		if suppress {
			return
		}

		upDelay := time.Duration(0)
		if mod.Delay != nil {
			upDelay = mod.Delay.Up
		}
		minPress := mod.MinPressTime
		held := time.Since(pressedAt)

		s.enqueueInject(func() {
			// Extend short presses to minPressTime
			if minPress > 0 && held < minPress {
				time.Sleep(minPress - held)
			}
			if upDelay > 0 {
				time.Sleep(upDelay)
			}
			inject(outCode, 0, outDeviceID)
		})
	}
}

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
func handleCombo(physCode uint16, physDeviceID string, val int32, outDeviceID string, mod *KeyModifier, s *KeyState) {
	switch val {
	case 2:
		// Suppress OS repeats, same as normal keys.
		return

	case 1:
		s.mu.Lock()
		alreadyDown := s.isDown
		s.isDown = true
		s.pressedAt = time.Now()
		s.mu.Unlock()
		if alreadyDown {
			return // ignore duplicate downs
		}

		var live map[uint16]bool
		if mod.TakeOver {
			// What's actually asserted on the output device right now —
			// not just keys we ourselves injected (a passthrough Shift is
			// still "held" downstream even though we never called
			// inject() for it), and not the real/physical keymap either
			// (a key like "q replace lshift" only ever shows "q" on the
			// real device; it's lshift that's actually held downstream).
			// This is a fast in-memory read, safe to do synchronously.
			pressed := iMan.PressedKeysVirt()

			live = make(map[uint16]bool, len(pressed))
			for _, code := range pressed {
				if code == physCode {
					continue
				}
				live[code] = true
			}

			// Ownership must be set synchronously, before we return —
			// the very next event read off the wire needs to see it so
			// it gets routed to the takeover interceptor instead of
			// normal dispatch.
			takeoverMu.Lock()
			takeoverActive = true
			takeoverOwnerCode = physCode
			takeoverOwnerDevice = physDeviceID
			takeoverLive = live
			takeoverMu.Unlock()
		}

		// The actual injections hit the wire (e.iMan.Send), so they're
		// deferred to this key's worker goroutine rather than run inline
		// here — inline would block the event-read loop from getting
		// back to ReadNext (and thus from ever calling BlockInput for
		// the *next* event) until every injection finished, which is
		// enough to make the server think this client stopped
		// responding to its own block-response.
		s.enqueueInject(func() {
			for code := range live {
				inject(code, 0, outDeviceID)
			}
			for _, c := range mod.Combo {
				inject(c, 1, outDeviceID)
			}
		})

	case 0:
		s.mu.Lock()
		wasDown := s.isDown
		s.isDown = false
		s.mu.Unlock()
		if !wasDown {
			return
		}

		var live map[uint16]bool
		if mod.TakeOver {
			takeoverMu.Lock()
			takeoverActive = false
			live = takeoverLive
			takeoverLive = nil
			takeoverMu.Unlock()
		}

		s.enqueueInject(func() {
			for _, v := range slices.Backward(mod.Combo) {
				inject(v, 0, outDeviceID)
			}
			for code, down := range live {
				if down {
					inject(code, 1, outDeviceID)
				}
			}
		})
	}
}

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

func parseModifyArgs(args []string) map[ModKey]*KeyModifier {
	result := make(map[ModKey]*KeyModifier)
	i := 0
	for i < len(args) {
		if args[i] != "--modify" {
			i++
			continue
		}
		i++

		// Collect tokens until next --flag
		var tokens []string
		for i < len(args) && !strings.HasPrefix(args[i], "--") {
			tokens = append(tokens, args[i])
			i++
		}

		if len(tokens) == 0 {
			fmt.Fprintln(os.Stderr, "warning: --modify with no arguments")
			continue
		}

		keyName := strings.ToLower(tokens[0])
		tokens = tokens[1:]

		// Optional "to" separator
		if len(tokens) > 0 && strings.ToLower(tokens[0]) == "to" {
			tokens = tokens[1:]
		}

		code, ok := input.StringToKey[keyName]
		if !ok {
			fmt.Fprintf(os.Stderr, "warning: unknown key %q, %v\n", keyName, input.StringToKey)
			continue
		}

		// Parse this --modify block into a fresh modifier first, since
		// "from <deviceID>" (which may appear anywhere in the token list)
		// determines which slot — device-specific or wildcard — it stacks
		// onto. If "from" appears more than once, the last one wins.
		parsed := &KeyModifier{}
		if err := applyTokens(parsed, tokens); err != nil {
			fmt.Fprintf(os.Stderr, "warning: --modify %s: %v\n", keyName, err)
			continue
		}

		mk := ModKey{Code: code, Device: parsed.DeviceID}
		if existing, ok := result[mk]; ok {
			mergeModifier(existing, parsed)
		} else {
			result[mk] = parsed
		}
	}
	return result
}

// lookupMod finds the modifier that applies to an event with the given
// physical key code and origin device. A modifier configured with an
// explicit "from <deviceID>" only matches events from that device; a
// modifier with no device filter (the common case) matches events from
// any device. An exact device match takes priority over the wildcard.
func lookupMod(mods map[ModKey]*KeyModifier, code uint16, device string) (*KeyModifier, bool) {
	if device != "" {
		if mod, ok := mods[ModKey{Code: code, Device: device}]; ok {
			return mod, true
		}
	}
	if mod, ok := mods[ModKey{Code: code, Device: ""}]; ok {
		return mod, true
	}
	return nil, false
}

func applyTokens(mod *KeyModifier, tokens []string) error {
	i := 0
	for i < len(tokens) {
		switch strings.ToLower(tokens[i]) {
		case "from":
			i++
			if i >= len(tokens) {
				return fmt.Errorf("from: expected a device id")
			}
			mod.DeviceID = tokens[i]
			i++

		case "invert":
			mod.Invert = true
			i++

		case "replace":
			i++
			if i >= len(tokens) {
				return fmt.Errorf("replace: expected target key name")
			}

			// "replace combo [takeover] <key> <key> ..." — emit a key combo
			// instead of a single replacement key. "takeover" (if present,
			// right after "combo") means: for all currently-held output
			// keys, release them, send the combo, then re-press them once
			// the combo's key is released. The combo list consumes tokens
			// until there are none left (--modify blocks are already split
			// apart by the caller, so end-of-tokens is the only terminator).
			if strings.ToLower(tokens[i]) == "combo" {
				i++
				if i < len(tokens) && strings.ToLower(tokens[i]) == "takeover" {
					mod.TakeOver = true
					i++
				}
				var combo []uint16
				for i < len(tokens) && !strings.HasPrefix(tokens[i], "--") {
					name := strings.ToLower(tokens[i])
					code, ok := input.StringToKey[name]
					if !ok {
						return fmt.Errorf("replace combo: unknown key %q", tokens[i])
					}
					combo = append(combo, code)
					i++
				}
				if len(combo) == 0 {
					return fmt.Errorf("replace combo: expected at least one key")
				}
				mod.Combo = combo
				continue
			}

			targetName := strings.ToLower(tokens[i])
			targetCode, ok := input.StringToKey[targetName]
			if !ok {
				return fmt.Errorf("replace: unknown key %q", tokens[i])
			}
			mod.ReplaceWith = &targetCode
			i++
			// Optional "from <deviceID>" directly after the target key names
			// the device the injected event should claim to originate from,
			// e.g. "replace y from dev2" sends y as if it came from dev2 —
			// distinct from the top-level "from" that filters which device
			// this whole modifier applies to.
			if i < len(tokens) && strings.ToLower(tokens[i]) == "from" {
				i++
				if i >= len(tokens) {
					return fmt.Errorf("replace: expected a device id after 'from'")
				}
				mod.ReplaceDeviceID = tokens[i]
				i++
			}

		case "toggle":
			mod.Toggle = true
			i++

		case "turbo":
			i++
			tc := &TurboConfig{
				DownFor: 10 * time.Millisecond,
				Delay:   10 * time.Millisecond,
			}
			for i < len(tokens) {
				sub := strings.ToLower(tokens[i])
				if sub == "downfor" {
					i++
					if i >= len(tokens) {
						return fmt.Errorf("turbo: expected duration after 'downFor'")
					}
					d, err := time.ParseDuration(tokens[i])
					if err != nil {
						return fmt.Errorf("turbo: invalid downFor %q: %v", tokens[i], err)
					}
					tc.DownFor = d
					i++
				} else if sub == "delay" {
					i++
					if i >= len(tokens) {
						return fmt.Errorf("turbo: expected duration after 'delay'")
					}
					d, err := time.ParseDuration(tokens[i])
					if err != nil {
						return fmt.Errorf("turbo: invalid delay %q: %v", tokens[i], err)
					}
					tc.Delay = d
					i++
				} else {
					break
				}
			}
			mod.Turbo = tc

		case "delay":
			i++
			dc := &DelayConfig{}
			for i < len(tokens) {
				sub := strings.ToLower(tokens[i])
				if sub == "down" {
					i++
					if i >= len(tokens) {
						return fmt.Errorf("delay: expected duration after 'down'")
					}
					d, err := time.ParseDuration(tokens[i])
					if err != nil {
						return fmt.Errorf("delay: invalid down %q: %v", tokens[i], err)
					}
					dc.Down = d
					i++
				} else if sub == "up" {
					i++
					if i >= len(tokens) {
						return fmt.Errorf("delay: expected duration after 'up'")
					}
					d, err := time.ParseDuration(tokens[i])
					if err != nil {
						return fmt.Errorf("delay: invalid up %q: %v", tokens[i], err)
					}
					dc.Up = d
					i++
				} else {
					break
				}
			}
			mod.Delay = dc

		case "maxpresstime":
			i++
			if i >= len(tokens) {
				return fmt.Errorf("maxPressTime: expected duration")
			}
			d, err := time.ParseDuration(tokens[i])
			if err != nil {
				return fmt.Errorf("maxPressTime: invalid duration %q: %v", tokens[i], err)
			}
			mod.MaxPressTime = d
			i++

		case "minpresstime":
			i++
			if i >= len(tokens) {
				return fmt.Errorf("minPressTime: expected duration")
			}
			d, err := time.ParseDuration(tokens[i])
			if err != nil {
				return fmt.Errorf("minPressTime: invalid duration %q: %v", tokens[i], err)
			}
			mod.MinPressTime = d
			i++

		default:
			return fmt.Errorf("unknown modifier %q", tokens[i])
		}
	}
	return nil
}

// ── Display helpers ───────────────────────────────────────────────────────────

func modDesc(mod *KeyModifier) string {
	var parts []string
	if mod.DeviceID != "" {
		parts = append(parts, fmt.Sprintf("from %s", mod.DeviceID))
	}
	if mod.Invert {
		parts = append(parts, "invert")
	}
	if mod.Combo != nil {
		names := make([]string, len(mod.Combo))
		for idx, c := range mod.Combo {
			name := input.KeyToString[c]
			if name == "" {
				name = fmt.Sprintf("code(%d)", c)
			}
			names[idx] = name
		}
		label := fmt.Sprintf("replace→combo(%s)", strings.Join(names, "+"))
		if mod.TakeOver {
			label += " [takeover]"
		}
		if mod.ReplaceDeviceID != "" {
			label += fmt.Sprintf(" (as if from %s)", mod.ReplaceDeviceID)
		}
		parts = append(parts, label)
	} else if mod.ReplaceWith != nil {
		name := input.KeyToString[*mod.ReplaceWith]
		if name == "" {
			name = fmt.Sprintf("code(%d)", *mod.ReplaceWith)
		}
		if mod.ReplaceDeviceID != "" {
			parts = append(parts, fmt.Sprintf("replace→%s (as if from %s)", name, mod.ReplaceDeviceID))
		} else {
			parts = append(parts, fmt.Sprintf("replace→%s", name))
		}
	}
	if mod.Toggle {
		parts = append(parts, "toggle")
	}
	if mod.Turbo != nil {
		parts = append(parts, fmt.Sprintf("turbo(downFor=%v, delay=%v)", mod.Turbo.DownFor, mod.Turbo.Delay))
	}
	if mod.Delay != nil {
		parts = append(parts, fmt.Sprintf("delay(down=%v, up=%v)", mod.Delay.Down, mod.Delay.Up))
	}
	if mod.MaxPressTime > 0 {
		parts = append(parts, fmt.Sprintf("maxPress=%v", mod.MaxPressTime))
	}
	if mod.MinPressTime > 0 {
		parts = append(parts, fmt.Sprintf("minPress=%v", mod.MinPressTime))
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, " + ")
}

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

	keyMods := parseModifyArgs(os.Args)
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
		fmt.Printf("  %-14s %s\n", keyName+":", modDesc(mod))
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
	for mk, mod := range keyMods {
		if mod.Invert && mod.Turbo != nil && !mod.Toggle {
			outCode := mk.Code
			if mod.ReplaceWith != nil {
				outCode = *mod.ReplaceWith
			}
			outDevice := mk.Device
			if mod.ReplaceDeviceID != "" {
				outDevice = mod.ReplaceDeviceID
			}
			s := getState(mk.Code, mk.Device)
			s.mu.Lock()
			s.turboStop = startTurbo(outCode, outDevice, mod.Turbo)
			s.mu.Unlock()
		}
	}

	fmt.Println("Running. Ctrl+C to exit.")

	go func() {
		for {
			re, err := iMan.ReadNext()
			if err != nil {
				fmt.Println("reader error:", err)
				return
			}

			switch re.From {
			case IMan.ModeFilter:
				dev := re.Event.GetDeviceID()

				if re.Event.Type != input.EV_KEY {
					// Non-key noise (EV_SYN, EV_MSC scancode metadata,
					// etc.) must never reach the takeover/modifier logic
					// below — see the matching comment in engine.go.
					iMan.BlockInput(re.Event.Seq, 0)
					continue
				}

				takeoverMu.Lock()
				active := takeoverActive
				ownerCode, ownerDev := takeoverOwnerCode, takeoverOwnerDevice
				takeoverMu.Unlock()

				if active && !(re.Event.Code == ownerCode && dev == ownerDev) {
					iMan.BlockInput(re.Event.Seq, 1)
					// A combo takeover currently owns input: every other
					// key (from other devices) is blocked outright, and
					// just recorded as down/up so handleCombo knows
					// what's still held once the combo releases.
					if re.Event.Value != 2 {
						takeoverMu.Lock()
						if takeoverLive != nil {
							takeoverLive[re.Event.Code] = re.Event.Value != 0
						}
						takeoverMu.Unlock()
					}
					continue
				}

				if mod, ok := lookupMod(keyMods, re.Event.Code, dev); ok {
					iMan.BlockInput(re.Event.Seq, 1)                         // intercept
					processKeyEvent(re.Event.Code, re.Event.Value, dev, mod) // handle
				} else {
					iMan.BlockInput(re.Event.Seq, 0) // pass through unmodified
				}

			case IMan.ModeListen:
				// Uncomment for raw event logging:
				// fmt.Printf("[real]  code=%d val=%d\n", re.Event.Code, re.Event.Value)

			case IMan.ModeVirtListen:
				// Uncomment for virtual event logging:
				// fmt.Printf("[virt]  code=%d val=%d\n", re.Event.Code, re.Event.Value)
			}
		}
	}()

	select {}
}

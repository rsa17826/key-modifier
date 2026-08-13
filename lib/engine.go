package keyModifierLib

import (
	"fmt"
	"strings"
	"sync"
	"time"

	input "github.com/rsa17826/go-input-lib"
	"github.com/rsa17826/input-manager/IMan"
)

// Engine holds the live connection to the input manager plus all per-key
// runtime state. Create one with NewEngine, then Connect and Run.
type Engine struct {
	iMan *IMan.ManagerConnection

	statesMu sync.Mutex
	states   map[ModKey]*keyState

	pressedMu sync.Mutex
	pressed   map[ModKey]struct{} // outCode/outDeviceID pairs currently held down (injected)

	modsMu sync.RWMutex
	mods   map[ModKey]*KeyModifier // currently active modifiers, swappable via SetMods

	// takeoverMu guards the takeover* fields below, which track a single
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

	runMu   sync.Mutex
	running bool
}

// NewEngine creates an unconnected Engine. Call Connect before Run.
func NewEngine() *Engine {
	return &Engine{
		states:  make(map[ModKey]*keyState),
		pressed: make(map[ModKey]struct{}),
		mods:    make(map[ModKey]*KeyModifier),
	}
}

// Connect opens the connection to the input manager. name is shown to the
// input manager as the identity of this client.
func (e *Engine) Connect(name string) error {
	m, err := IMan.Connect(name, IMan.ModeInjection, IMan.ModeFilter, IMan.ModeListen, IMan.ModeVirtListen)
	if err != nil {
		return err
	}
	e.iMan = m
	// autoRead=false: Run's own ReadNext loop updates the keymap as a side
	// effect. Combo takeover needs this real (physical) keymap rather than
	// our own e.pressed bookkeeping, since a passthrough key (e.g. Shift,
	// never modified/injected by us) is still physically held as far as
	// the OS/virtual device is concerned but never goes through e.inject().
	if err := e.iMan.EnableKeyMap(false); err != nil {
		return err
	}
	return nil
}

// Close shuts down the input manager connection.
func (e *Engine) Close() {
	if e.iMan != nil {
		e.Cleanup()
		e.iMan.Close()
	}
}

// getState returns the keyState for a (physical code, device) pair, creating
// it on first use. State is scoped per-device so the same physical key on
// two different devices never shares turbo/press-timing state.
func (e *Engine) getState(code uint16, device string) *keyState {
	e.statesMu.Lock()
	defer e.statesMu.Unlock()
	mk := ModKey{Code: code, Device: device}
	if s, ok := e.states[mk]; ok {
		return s
	}
	s := &keyState{}
	e.states[mk] = s
	return s
}

// ── Event helpers ─────────────────────────────────────────────────────────────

func (e *Engine) inject(code uint16, val int32, deviceID string) {
	e.trackPressed(code, val, deviceID)
	e.iMan.Send(wireEvent(code, val, deviceID))
}

// trackPressed records which (outCode, outDeviceID) pairs are currently
// held down as a result of our own injected events, so Cleanup can release
// them all later (e.g. on shutdown) without leaving the virtual device with
// stuck keys.
func (e *Engine) trackPressed(code uint16, val int32, deviceID string) {
	if val == 2 {
		return // repeats don't change press state
	}
	mk := ModKey{Code: code, Device: deviceID}
	e.pressedMu.Lock()
	if val == 1 {
		e.pressed[mk] = struct{}{}
	} else {
		delete(e.pressed, mk)
	}
	e.pressedMu.Unlock()
}

// Cleanup releases every key this Engine has injected as "down" (including
// keys currently mid-turbo), sending a synthetic key-up for each so the
// virtual device never gets left with stuck keys. Safe to call once at
// shutdown; after calling, all tracked press state is cleared.
func (e *Engine) Cleanup() {
	// Stop any active turbo/timers first so they don't re-press a key we're
	// about to release.
	e.statesMu.Lock()
	for _, s := range e.states {
		s.mu.Lock()
		if s.turboStop != nil {
			closeChan(&s.turboStop)
		}
		if s.maxPressStop != nil {
			closeChan(&s.maxPressStop)
		}
		s.mu.Unlock()
	}
	e.statesMu.Unlock()

	e.pressedMu.Lock()
	toRelease := make([]ModKey, 0, len(e.pressed))
	for mk := range e.pressed {
		toRelease = append(toRelease, mk)
	}
	e.pressed = make(map[ModKey]struct{})
	e.pressedMu.Unlock()

	for _, mk := range toRelease {
		e.iMan.Send(wireEvent(mk.Code, 0, mk.Device))
	}
}

// ── Turbo goroutine ───────────────────────────────────────────────────────────
//
// Sends rapid key down/up pairs until the returned stop channel is closed.
// Sends a final key-up when stopped so the virtual device never gets stuck.

func (e *Engine) startTurbo(codes []uint16, deviceID string, cfg *TurboConfig) chan struct{} {
	stop := make(chan struct{})
	go func() {
		for {
			e.injectAll(codes, 1, deviceID)
			select {
			case <-stop:
				e.injectAll(codes, 0, deviceID)
				return
			case <-time.After(cfg.DownFor):
			}
			e.injectAll(codes, 0, deviceID)
			select {
			case <-stop:
				e.injectAll(codes, 0, deviceID)
				return
			case <-time.After(cfg.Delay):
			}
		}
	}()
	return stop
}

func (e *Engine) injectAll(codes []uint16, val int32, deviceID string) {
	if val == 1 {
		for _, c := range codes {
			e.inject(c, 1, deviceID)
		}
	} else if val == 0 {
		for i := len(codes) - 1; i >= 0; i-- {
			e.inject(codes[i], 0, deviceID)
		}
	} else {
		for _, c := range codes {
			e.inject(c, val, deviceID)
		}
	}
}

// handleCombo implements "replace combo [takeover] <key1> <key2> ...".
//
// On physical down: if takeover is set, every key currently asserted on the
// virtual output device is released, and the combo takes exclusive
// ownership of input — every other key event is intercepted (see Run's
// dispatch loop) and just recorded as down/up in e.takeoverLive, rather
// than forwarded or processed normally. This means a key that was down at
// combo start but gets released mid-combo is correctly *not* restored, and
// a key freshly pressed mid-combo *is* restored if it's still down when the
// combo ends — the restore reflects live state at release time, not a
// stale snapshot from when the combo began.
func (e *Engine) handleCombo(physCode uint16, physDeviceID string, val int32, outDeviceID string, mod *KeyModifier, s *keyState) {
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
			// not just keys we ourselves injected, and not the
			// real/physical keymap either (a key like "q replace lshift"
			// only ever shows "q" on the real device; it's lshift that's
			// actually held downstream). This is a fast in-memory read,
			// safe to do synchronously.
			pressed := e.iMan.PressedKeysVirt()

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
			e.takeoverMu.Lock()
			e.takeoverActive = true
			e.takeoverOwnerCode = physCode
			e.takeoverOwnerDevice = physDeviceID
			e.takeoverLive = live
			e.takeoverMu.Unlock()
		}

		// The actual injections hit the wire (e.iMan.Send), so they're
		// deferred to this key's worker goroutine rather than run inline
		// here — inline would block Run's event-read loop from getting
		// back to ReadNext (and thus from ever calling BlockInput for
		// the *next* event) until every injection finished, which is
		// enough to make the server think this client stopped
		// responding to its own block-response.
		s.enqueueInject(func() {
			for code := range live {
				e.inject(code, 0, outDeviceID)
			}
			for _, c := range mod.Combo {
				e.inject(c, 1, outDeviceID)
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
			e.takeoverMu.Lock()
			e.takeoverActive = false
			live = e.takeoverLive
			e.takeoverLive = nil
			e.takeoverMu.Unlock()
		}

		s.enqueueInject(func() {
			for i := len(mod.Combo) - 1; i >= 0; i-- {
				e.inject(mod.Combo[i], 0, outDeviceID)
			}
			for code, down := range live {
				if down {
					e.inject(code, 1, outDeviceID)
				}
			}
		})
	}
}

// ── Key event processor ───────────────────────────────────────────────────────
//
// Called synchronously (im.BlockInput already sent) so state mutations are
// ordered. Long waits / injects happen in spawned goroutines.
func (e *Engine) ProcessKeyEvent(code uint16, val int32, deviceID string, mod *KeyModifier) {
	if mod.Invert && val != 2 {
		if val == 0 {
			val = 1
		} else {
			val = 0
		}
	}

	outCodes := []uint16{code}
	if len(mod.ReplaceWith) > 0 {
		outCodes = mod.ReplaceWith
	}

	outDeviceID := deviceID
	if mod.ReplaceDeviceID != "" {
		outDeviceID = mod.ReplaceDeviceID
	}

	s := e.getState(code, deviceID)

	if mod.Combo != nil {
		e.handleCombo(code, deviceID, val, outDeviceID, mod, s)
		return
	}

	switch val {
	case 2:
		return

	case 1:
		s.mu.Lock()
		s.isDown = true
		s.pressedAt = time.Now()

		if mod.Toggle {
			s.toggled = !s.toggled
			active := s.toggled
			s.mu.Unlock()

			if active {
				if mod.Turbo != nil {
					s.mu.Lock()
					if s.turboStop != nil {
						closeChan(&s.turboStop)
					}
					s.turboStop = e.startTurbo(outCodes, outDeviceID, mod.Turbo)
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
						e.injectAll(outCodes, 1, outDeviceID)
					}()
				}
			} else {
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
						e.injectAll(outCodes, 0, outDeviceID)
					}()
				}
			}
			return
		}

		s.mu.Unlock()

		if mod.Turbo != nil {
			s.mu.Lock()
			pressedAt := s.pressedAt
			s.turboStop = e.startTurbo(outCodes, outDeviceID, mod.Turbo)
			maxPress := mod.MaxPressTime
			if maxPress > 0 {
				maxStop := make(chan struct{})
				s.maxPressStop = maxStop
				s.mu.Unlock()
				go func() {
					elapsed := time.Since(pressedAt)
					remaining := maxPress - elapsed
					if remaining < 0 {
						remaining = 0
					}
					select {
					case <-maxStop:
					case <-time.After(remaining):
						s.mu.Lock()
						if s.isDown {
							s.suppressUp = true
							closeChan(&s.turboStop)
						}
						s.mu.Unlock()
					}
				}()
			} else {
				s.mu.Unlock()
			}
			return
		}

		downDelay := time.Duration(0)
		if mod.Delay != nil {
			downDelay = mod.Delay.Down
		}
		maxPress := mod.MaxPressTime

		s.enqueueInject(func() {
			if downDelay > 0 {
				time.Sleep(downDelay)
			}
			e.injectAll(outCodes, 1, outDeviceID)

			if maxPress > 0 {
				maxStop := make(chan struct{})
				s.mu.Lock()
				s.maxPressStop = maxStop
				s.mu.Unlock()

				go func() {
					select {
					case <-maxStop:
					case <-time.After(maxPress):
						s.mu.Lock()
						if s.isDown {
							s.suppressUp = true
							s.mu.Unlock()
							e.injectAll(outCodes, 0, outDeviceID)
						} else {
							s.mu.Unlock()
						}
					}
				}()
			}
		})

	case 0:
		s.mu.Lock()
		s.isDown = false

		if mod.Toggle {
			s.mu.Unlock()
			return
		}

		if s.turboStop != nil {
			pressedAt := s.pressedAt
			minPress := mod.MinPressTime
			held := time.Since(pressedAt)

			if minPress > 0 && held < minPress {
				remaining := minPress - held
				stopChan := s.turboStop
				s.turboStop = nil
				if s.maxPressStop != nil {
					closeChan(&s.maxPressStop)
					s.maxPressStop = nil
				}
				s.mu.Unlock()

				go func() {
					time.Sleep(remaining)
					closeChan(&stopChan)
				}()
			} else {
				closeChan(&s.turboStop)
				if s.maxPressStop != nil {
					closeChan(&s.maxPressStop)
				}
				s.mu.Unlock()
			}
			return
		}

		if s.maxPressStop != nil {
			closeChan(&s.maxPressStop)
		}

		suppress := s.suppressUp
		s.suppressUp = false
		pressedAt := s.pressedAt
		s.mu.Unlock()

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
			if minPress > 0 && held < minPress {
				time.Sleep(minPress - held)
			}
			if upDelay > 0 {
				time.Sleep(upDelay)
			}
			e.injectAll(outCodes, 0, outDeviceID)
		})
	}
}

// SetMods replaces the set of active key modifiers, taking effect
// immediately for the next processed event. Safe to call at any time,
// including while Run is active in another goroutine — e.g. from a
// window-focus callback that wants "modify d as turbo" active only while a
// certain window is focused, and no modifications at all otherwise (pass an
// empty map, not nil, to disable).
//
// Any keys/timers/turbo goroutines held by the previous mod set are
// released via Cleanup before the new set takes effect, so switching mods
// never leaves a key stuck down. Any invert+turbo (no toggle) keys in the
// new set are started immediately, matching Run's own startup behavior.
func (e *Engine) SetMods(keyMods map[ModKey]*KeyModifier) {
	if keyMods == nil {
		keyMods = make(map[ModKey]*KeyModifier)
	}

	// Release everything held by the outgoing mod set before swapping in
	// the new one, so a key turbo-ing under the old config doesn't keep
	// running after it's no longer configured to.
	e.Cleanup()

	e.modsMu.Lock()
	e.mods = keyMods
	e.modsMu.Unlock()

	e.startInvertTurbo(keyMods)
}

// IsRunning reports whether Run's event loop is currently active.
func (e *Engine) IsRunning() bool {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	return e.running
}

// EnsureRunning starts Run in a background goroutine if it isn't already
// running. onExit, if non-nil, is called with Run's return error whenever
// the loop exits (including immediately if it was already running, in
// which case onExit is not called). Callers that want a self-healing
// engine (e.g. reconnect/retry) should trigger EnsureRunning again from
// onExit as appropriate; EnsureRunning itself does not retry.
func (e *Engine) EnsureRunning(onExit func(error)) {
	e.runMu.Lock()
	if e.running {
		e.runMu.Unlock()
		return
	}
	e.running = true
	e.runMu.Unlock()

	go func() {
		err := e.Run()
		e.runMu.Lock()
		e.running = false
		e.runMu.Unlock()
		if onExit != nil {
			onExit(err)
		}
	}()
}

// startInvertTurbo starts turbo immediately for any invert+turbo (no
// toggle) keys in keyMods: the physical key being up means "inverted down",
// so turbo should already be running at startup / whenever such a key
// becomes active via SetMods.
func (e *Engine) startInvertTurbo(keyMods map[ModKey]*KeyModifier) {
	for mk, mod := range keyMods {
		if mod.Invert && mod.Turbo != nil && !mod.Toggle {
			outCodes := []uint16{mk.Code}
			if len(mod.ReplaceWith) > 0 {
				outCodes = mod.ReplaceWith
			}
			outDevice := mk.Device
			if mod.ReplaceDeviceID != "" {
				outDevice = mod.ReplaceDeviceID
			}
			s := e.getState(mk.Code, mk.Device)
			s.mu.Lock()
			s.turboStop = e.startTurbo(outCodes, outDevice, mod.Turbo)
			s.mu.Unlock()
		}
	}
}

// lookupMod finds the modifier that applies to an event with the given
// physical key code and origin device. A modifier configured with an
// explicit "from <deviceID>" only matches events from that device; a
// modifier with no device filter (the common case) matches events from
// any device. An exact device match takes priority over the wildcard.

// Run starts processing events according to the engine's active mods (set
// via SetMods, or passed as an optional initial value here). It blocks
// until the input manager's reader loop exits (e.g. because Close was
// called or the connection errored), at which point it returns that error
// (nil on a clean Close). Call Connect before Run.
//
// Prefer EnsureRunning over calling Run directly when the set of active
// mods will change over the engine's lifetime (e.g. driven by window-focus
// events) — Run is meant to be started once and left running, with SetMods
// used to change behavior live rather than starting a second, competing
// Run loop on the same connection.
func (e *Engine) Run(initialMods ...map[ModKey]*KeyModifier) error {
	if len(initialMods) > 0 && initialMods[0] != nil {
		e.SetMods(initialMods[0])
	} else {
		e.modsMu.RLock()
		mods := e.mods
		e.modsMu.RUnlock()
		e.startInvertTurbo(mods)
	}

	for {
		re, err := e.iMan.ReadNext()
		if err != nil {
			return err
		}

		switch re.From {
		case IMan.ModeFilter:
			dev := re.Event.GetDeviceID()

			if re.Event.Type != input.EV_KEY {
				// Non-key noise (EV_SYN, EV_MSC scancode metadata, etc.)
				// must never reach the takeover/modifier logic below: it
				// has no real down/up semantics (an EV_MSC scancode event
				// always carries a large nonzero, non-0/1/2 Value, so
				// "Value != 0" would be read as an eternal "key down" that
				// never gets a matching "up" to cancel it out — which is
				// exactly how a phantom key could get "restored" after a
				// takeover combo ends). Just let it through untouched.
				e.iMan.BlockInput(re.Event.Seq, 0)
				continue
			}

			e.takeoverMu.Lock()
			active := e.takeoverActive
			ownerCode, ownerDev := e.takeoverOwnerCode, e.takeoverOwnerDevice
			e.takeoverMu.Unlock()

			if active && !(re.Event.Code == ownerCode && dev == ownerDev) {
				e.iMan.BlockInput(re.Event.Seq, 1)
				// A combo takeover currently owns input: every other key
				// (from other devices) is blocked outright, and just
				// recorded as down/up so handleCombo knows what's still
				// held once the combo releases.
				if re.Event.Value != 2 {
					e.takeoverMu.Lock()
					if e.takeoverLive != nil {
						e.takeoverLive[re.Event.Code] = re.Event.Value != 0
					}
					e.takeoverMu.Unlock()
				}
				continue
			}

			e.modsMu.RLock()
			mod, ok := LookupMod(e.mods, re.Event.Code, dev)
			e.modsMu.RUnlock()
			if ok {
				e.iMan.BlockInput(re.Event.Seq, 1)                         // intercept
				e.ProcessKeyEvent(re.Event.Code, re.Event.Value, dev, mod) // handle
			} else {
				e.iMan.BlockInput(re.Event.Seq, 0) // pass through unmodified
			}

		case IMan.ModeListen:
			// Uncomment for raw event logging:
			// fmt.Printf("[real]  code=%d val=%d\n", re.Event.Code, re.Event.Value)

		case IMan.ModeVirtListen:
			// Uncomment for virtual event logging:
			// fmt.Printf("[virt]  code=%d val=%d\n", re.Event.Code, re.Event.Value)
		}
	}
}
func ApplyTokens(mod *KeyModifier, tokens []string) error {
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
			mod.ReplaceWith = append(mod.ReplaceWith, targetCode)
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
func closeChan(ch *chan struct{}) {
	if *ch != nil {
		close(*ch)
		*ch = nil
	}
}

func LookupMod(mods map[ModKey]*KeyModifier, code uint16, device string) (*KeyModifier, bool) {
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

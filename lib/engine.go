package keyModifierLib

import (
	"sync"
	"time"

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

func (e *Engine) startTurbo(code uint16, deviceID string, cfg *TurboConfig) chan struct{} {
	stop := make(chan struct{})
	go func() {
		for {
			e.inject(code, 1, deviceID)
			select {
			case <-stop:
				e.inject(code, 0, deviceID)
				return
			case <-time.After(cfg.DownFor):
			}
			e.inject(code, 0, deviceID)
			select {
			case <-stop:
				return
			case <-time.After(cfg.Delay):
			}
		}
	}()
	return stop
}

// handleCombo implements "replace combo [takeover] <key1> <key2> ...".
//
// On physical down: if takeover is set, every key currently physically held
// (per iMan's real keymap, not just keys we ourselves injected — a
// passthrough key we never modified is still physically held) is released
// first and remembered on the KeyState, then the combo keys are injected
// down in order. On physical up: the combo keys are injected up in reverse
// order, then (if takeover) the previously-held keys are re-pressed.
func (e *Engine) handleCombo(physCode uint16, val int32, outDeviceID string, mod *KeyModifier, s *keyState) {
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

		if mod.TakeOver {
			// Exclude the trigger key itself: its raw press was already
			// recorded in the keymap before we blocked it, but the combo
			// up/down is its lifecycle — it's not a separate held key to
			// release/restore. We use the VIRTUAL keymap (what's actually
			// asserted on the output device), not the real one — a key
			// like "q replace lshift" is only ever "q" on the real
			// device; it's lshift that's actually held downstream, and
			// that's what needs releasing/restoring.
			pressed := e.iMan.PressedKeysVirt()
			restore := make([]uint16, 0, len(pressed))
			for _, code := range pressed {
				if code == physCode {
					continue
				}
				restore = append(restore, code)
			}

			for _, code := range restore {
				e.inject(code, 0, outDeviceID)
			}

			s.mu.Lock()
			s.takeoverRestore = restore
			s.mu.Unlock()
		}

		for _, c := range mod.Combo {
			e.inject(c, 1, outDeviceID)
		}

	case 0:
		s.mu.Lock()
		wasDown := s.isDown
		s.isDown = false
		restore := s.takeoverRestore
		s.takeoverRestore = nil
		s.mu.Unlock()
		if !wasDown {
			return
		}

		for i := len(mod.Combo) - 1; i >= 0; i-- {
			e.inject(mod.Combo[i], 0, outDeviceID)
		}

		for _, code := range restore {
			e.inject(code, 1, outDeviceID)
		}
	}
}

// ── Key event processor ───────────────────────────────────────────────────────
//
// Called synchronously (im.BlockInput already sent) so state mutations are
// ordered. Long waits / injects happen in spawned goroutines.

func (e *Engine) processKeyEvent(code uint16, val int32, deviceID string, mod *KeyModifier) {
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
	s := e.getState(code, deviceID)

	// ── combo: emit a key combo instead of a single key ──────────────────────
	// Turbo/toggle/delay/press-time modifiers don't apply to combos; a combo
	// is just "press these keys together while the physical key is held".
	if mod.Combo != nil {
		e.handleCombo(code, val, outDeviceID, mod, s)
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
					s.turboStop = e.startTurbo(outCode, outDeviceID, mod.Turbo)
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
						e.inject(outCode, 1, outDeviceID)
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
						e.inject(outCode, 0, outDeviceID)
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
			s.turboStop = e.startTurbo(outCode, outDeviceID, mod.Turbo)
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
			e.inject(outCode, 1, outDeviceID)

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
							e.inject(outCode, 0, outDeviceID) // synthetic up at cap
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
			e.inject(outCode, 0, outDeviceID)
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
			outCode := mk.Code
			if mod.ReplaceWith != nil {
				outCode = *mod.ReplaceWith
			}
			outDevice := mk.Device
			if mod.ReplaceDeviceID != "" {
				outDevice = mod.ReplaceDeviceID
			}
			s := e.getState(mk.Code, mk.Device)
			s.mu.Lock()
			s.turboStop = e.startTurbo(outCode, outDevice, mod.Turbo)
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
			e.modsMu.RLock()
			mod, ok := lookupMod(e.mods, re.Event.Code, dev)
			e.modsMu.RUnlock()
			if ok {
				e.iMan.BlockInput(re.Event.Seq, 1)                         // intercept
				e.processKeyEvent(re.Event.Code, re.Event.Value, dev, mod) // handle
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

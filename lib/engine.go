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
}

// NewEngine creates an unconnected Engine. Call Connect before Run.
func NewEngine() *Engine {
	return &Engine{
		states:  make(map[ModKey]*keyState),
		pressed: make(map[ModKey]struct{}),
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

// lookupMod finds the modifier that applies to an event with the given
// physical key code and origin device. A modifier configured with an
// explicit "from <deviceID>" only matches events from that device; a
// modifier with no device filter (the common case) matches events from
// any device. An exact device match takes priority over the wildcard.

// Run starts processing events according to keyMods. It blocks until the
// input manager's reader loop exits (e.g. because Close was called or the
// connection errored), at which point it returns nil. Call Connect before
// Run.
func (e *Engine) Run(keyMods map[ModKey]*KeyModifier) error {
	// Invert+turbo (no toggle) keys are virtually "down" at startup because
	// the physical key is up = inverted = down. Start turbo immediately.
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

	for {
		re, err := e.iMan.ReadNext()
		if err != nil {
			return err
		}

		switch re.From {
		case IMan.ModeFilter:
			dev := re.Event.GetDeviceID()
			if mod, ok := lookupMod(keyMods, re.Event.Code, dev); ok {
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

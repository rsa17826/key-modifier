package main

import (
	"fmt"
	"os"
	"os/signal"
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
}

// modKey identifies a modifier slot: a physical key code plus the optional
// device filter it was configured with. A given key can have both a
// device-specific modifier ("x from dev1 ...") and a wildcard modifier
// ("x ...") registered at once; lookupMod prefers the exact device match.
type modKey struct {
	Code   uint16
	Device string
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
}

// ── Globals ──────────────────────────────────────────────────────────────────

var (
	iMan     *IMan.ManagerConnection
	statesMu sync.Mutex
	states   = make(map[modKey]*KeyState)
)

// getState returns the KeyState for a (physical code, device) pair, creating
// it on first use. State is scoped per-device so the same physical key on
// two different devices never shares turbo/press-timing state.
func getState(code uint16, device string) *KeyState {
	statesMu.Lock()
	defer statesMu.Unlock()
	mk := modKey{Code: code, Device: device}
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

		go func() {
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
		}()

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

		go func() {
			// Extend short presses to minPressTime
			if minPress > 0 && held < minPress {
				time.Sleep(minPress - held)
			}
			if upDelay > 0 {
				time.Sleep(upDelay)
			}
			inject(outCode, 0, outDeviceID)
		}()
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

func parseModifyArgs(args []string) map[modKey]*KeyModifier {
	result := make(map[modKey]*KeyModifier)
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

		mk := modKey{Code: code, Device: parsed.DeviceID}
		if existing, ok := result[mk]; ok {
			mergeModifier(existing, parsed)
		} else {
			result[mk] = parsed
		}
	}
	return result
}

// mergeModifier folds src's settings into dst, so multiple --modify flags
// for the same key (and same device filter) stack rather than overwrite.
func mergeModifier(dst, src *KeyModifier) {
	if src.Invert {
		dst.Invert = true
	}
	if src.ReplaceWith != nil {
		dst.ReplaceWith = src.ReplaceWith
		dst.ReplaceDeviceID = src.ReplaceDeviceID
	}
	if src.Toggle {
		dst.Toggle = true
	}
	if src.Turbo != nil {
		dst.Turbo = src.Turbo
	}
	if src.Delay != nil {
		dst.Delay = src.Delay
	}
	if src.MaxPressTime > 0 {
		dst.MaxPressTime = src.MaxPressTime
	}
	if src.MinPressTime > 0 {
		dst.MinPressTime = src.MinPressTime
	}
	// dst.DeviceID already equals src.DeviceID — that's how they landed
	// in the same map slot.
}

// lookupMod finds the modifier that applies to an event with the given
// physical key code and origin device. A modifier configured with an
// explicit "from <deviceID>" only matches events from that device; a
// modifier with no device filter (the common case) matches events from
// any device. An exact device match takes priority over the wildcard.
func lookupMod(mods map[modKey]*KeyModifier, code uint16, device string) (*KeyModifier, bool) {
	if device != "" {
		if mod, ok := mods[modKey{Code: code, Device: device}]; ok {
			return mod, true
		}
	}
	if mod, ok := mods[modKey{Code: code, Device: ""}]; ok {
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
	if mod.ReplaceWith != nil {
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

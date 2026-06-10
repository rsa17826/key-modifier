package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
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
	Toggle       bool
	Invert       bool    // swap down↔up before any other modifier sees the event
	ReplaceWith  *uint16 // nil = emit the physical key; non-nil = emit this code instead
	Turbo        *TurboConfig
	Delay        *DelayConfig
	MaxPressTime time.Duration // 0 = disabled
	MinPressTime time.Duration // 0 = disabled
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
	states   = make(map[uint16]*KeyState)
)

func getState(code uint16) *KeyState {
	statesMu.Lock()
	defer statesMu.Unlock()
	if s, ok := states[code]; ok {
		return s
	}
	s := &KeyState{}
	states[code] = s
	return s
}

// ── Event helpers ─────────────────────────────────────────────────────────────

func wireEvent(code uint16, val int32) IMan.WireEvent {
	t := time.Now()
	return IMan.WireEvent{
		Sec:   t.Unix(),
		Usec:  int64(t.Nanosecond() / 1000),
		Type:  input.EV_KEY,
		Code:  code,
		Value: val,
	}
}

func inject(code uint16, val int32) {
	iMan.Send(wireEvent(code, val))
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

func startTurbo(code uint16, cfg *TurboConfig) chan struct{} {
	stop := make(chan struct{})
	go func() {
		for {
			inject(code, 1)
			select {
			case <-stop:
				inject(code, 0)
				return
			case <-time.After(cfg.DownFor):
			}
			inject(code, 0)
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

func processKeyEvent(code uint16, val int32, mod *KeyModifier) {
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

	// State is always keyed on the physical code so physical up/down stay paired.
	s := getState(code)

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
					s.turboStop = startTurbo(outCode, mod.Turbo)
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
						inject(outCode, 1)
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
						inject(outCode, 0)
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
			s.turboStop = startTurbo(outCode, mod.Turbo)
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
			inject(outCode, 1)

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
							inject(outCode, 0) // synthetic up at cap
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
			inject(outCode, 0)
		}()
	}
}

// ── Key name → code table ─────────────────────────────────────────────────────

var keyNames = map[string]uint16{
	"A": input.KEY_A, "B": input.KEY_B, "C": input.KEY_C, "D": input.KEY_D,
	"E": input.KEY_E, "F": input.KEY_F, "G": input.KEY_G, "H": input.KEY_H,
	"I": input.KEY_I, "J": input.KEY_J, "K": input.KEY_K, "L": input.KEY_L,
	"M": input.KEY_M, "N": input.KEY_N, "O": input.KEY_O, "P": input.KEY_P,
	"Q": input.KEY_Q, "R": input.KEY_R, "S": input.KEY_S, "T": input.KEY_T,
	"U": input.KEY_U, "V": input.KEY_V, "W": input.KEY_W, "X": input.KEY_X,
	"Y": input.KEY_Y, "Z": input.KEY_Z,
	"0": input.KEY_0, "1": input.KEY_1, "2": input.KEY_2, "3": input.KEY_3,
	"4": input.KEY_4, "5": input.KEY_5, "6": input.KEY_6, "7": input.KEY_7,
	"8": input.KEY_8, "9": input.KEY_9,
	"SPACE":      input.KEY_SPACE,
	"ENTER":      input.KEY_ENTER,
	"ESC":        input.KEY_ESC,
	"TAB":        input.KEY_TAB,
	"BACKSPACE":  input.KEY_BACKSPACE,
	"LEFT":       input.KEY_LEFT,
	"RIGHT":      input.KEY_RIGHT,
	"UP":         input.KEY_UP,
	"DOWN":       input.KEY_DOWN,
	"LEFTSHIFT":  input.KEY_LEFTSHIFT,
	"RIGHTSHIFT": input.KEY_RIGHTSHIFT,
	"LEFTCTRL":   input.KEY_LEFTCTRL,
	"RIGHTCTRL":  input.KEY_RIGHTCTRL,
	"LEFTALT":    input.KEY_LEFTALT,
	"RIGHTALT":   input.KEY_RIGHTALT,
	"F1":         input.KEY_F1,
	"F2":         input.KEY_F2,
	"F3":         input.KEY_F3,
	"F4":         input.KEY_F4,
	"F5":         input.KEY_F5,
	"F6":         input.KEY_F6,
	"F7":         input.KEY_F7,
	"F8":         input.KEY_F8,
	"F9":         input.KEY_F9,
	"F10":        input.KEY_F10,
	"F11":        input.KEY_F11,
	"F12":        input.KEY_F12,
	"LBUTTON":    input.BTN_LEFT,
	"RBUTTON":    input.BTN_RIGHT,
	"MBUTTON":    input.BTN_MIDDLE,
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

func parseModifyArgs(args []string) map[uint16]*KeyModifier {
	result := make(map[uint16]*KeyModifier)
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

		keyName := strings.ToUpper(tokens[0])
		tokens = tokens[1:]

		// Optional "to" separator
		if len(tokens) > 0 && strings.ToLower(tokens[0]) == "to" {
			tokens = tokens[1:]
		}

		code, ok := keyNames[keyName]
		if !ok {
			fmt.Fprintf(os.Stderr, "warning: unknown key %q\n", keyName)
			continue
		}

		mod := result[code]
		if mod == nil {
			mod = &KeyModifier{}
			result[code] = mod
		}

		if err := applyTokens(mod, tokens); err != nil {
			fmt.Fprintf(os.Stderr, "warning: --modify %s: %v\n", keyName, err)
		}
	}
	return result
}

func applyTokens(mod *KeyModifier, tokens []string) error {
	i := 0
	for i < len(tokens) {
		switch strings.ToLower(tokens[i]) {
		case "invert":
			mod.Invert = true
			i++

		case "replace":
			i++
			if i >= len(tokens) {
				return fmt.Errorf("replace: expected target key name")
			}
			targetName := strings.ToUpper(tokens[i])
			targetCode, ok := keyNames[targetName]
			if !ok {
				return fmt.Errorf("replace: unknown key %q", tokens[i])
			}
			mod.ReplaceWith = &targetCode
			i++

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

func modDesc(mod *KeyModifier, codeToName map[uint16]string) string {
	var parts []string
	if mod.Invert {
		parts = append(parts, "invert")
	}
	if mod.ReplaceWith != nil {
		name := codeToName[*mod.ReplaceWith]
		if name == "" {
			name = fmt.Sprintf("code(%d)", *mod.ReplaceWith)
		}
		parts = append(parts, fmt.Sprintf("replace→%s", name))
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
  invert
      Swap down and up events before any other modifier sees them.
      Key fires while NOT held.  Combine with turbo for inverted turbo.

  replace <targetKey>
      Emit targetKey instead of the physical key.  Affects all injected
      events including turbo pulses.

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

	// Build reverse map for display
	codeToName := make(map[uint16]string)
	for name, code := range keyNames {
		codeToName[code] = name
	}

	fmt.Println("Active modifications:")
	for code, mod := range keyMods {
		fmt.Printf("  %-14s %s\n", codeToName[code]+":", modDesc(mod, codeToName))
	}
	fmt.Println()

	var err error
	iMan, err = IMan.Connect("key modifier", IMan.ModeInjection, IMan.ModeBlocking, IMan.ModeListen, IMan.ModeVirtListen)
	if err != nil {
		panic(err)
	}
	defer iMan.Close()

	// Invert+turbo (no toggle) keys are virtually "down" at startup because
	// the physical key is up = inverted = down.  Start turbo immediately.
	for code, mod := range keyMods {
		if mod.Invert && mod.Turbo != nil && !mod.Toggle {
			outCode := code
			if mod.ReplaceWith != nil {
				outCode = *mod.ReplaceWith
			}
			s := getState(code)
			s.mu.Lock()
			s.turboStop = startTurbo(outCode, mod.Turbo)
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
			case IMan.ModeBlocking:
				if mod, ok := keyMods[re.Event.Code]; ok {
					iMan.BlockInput(1)                                  // intercept
					processKeyEvent(re.Event.Code, re.Event.Value, mod) // handle
				} else {
					iMan.BlockInput(0) // pass through unmodified
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

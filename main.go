package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	input "github.com/rsa17826/go-input-lib"

	keymod "github.com/rsa17826/key-modifier/lib"
)

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

func main() {
	keyMods := keymod.ParseModifyArgs(os.Args)
	if len(keyMods) == 0 {
		printUsage()
		return
	}

	fmt.Println("Active modifications:")
	for mk, mod := range keyMods {
		keyName := input.KeyToString[mk.Code]
		if keyName == "" {
			keyName = fmt.Sprintf("code(%d)", mk.Code)
		}
		fmt.Printf("  %-14s %s\n", keyName+":", keymod.ModDesc(mod))
	}
	fmt.Println()

	engine := keymod.NewEngine()
	// TODO make not have to put in both places - add way to change registered key list after connecting?
	if err := engine.Connect("key modifier", keyMods); err != nil {
		panic(err)
	}

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGABRT)
		<-sigChan
		engine.Close()
		os.Exit(0)
	}()

	fmt.Println("Running. Ctrl+C to exit.")

	if err := engine.Run(keyMods); err != nil {
		fmt.Println("reader error:", err)
	}
}

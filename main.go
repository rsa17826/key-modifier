package main

import (
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/rsa17826/go-argtree"
	input "github.com/rsa17826/go-input-lib"
	keyModifierLib "github.com/rsa17826/key-modifier/lib"
)

func printUsage() {
	fmt.Print(`keyModifierLib — intercept and transform keyboard/mouse events

Usage:
  keyModifierLib --modify <key> <modifier> [options] [--modify ...]

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
  --modify z toggle
  --modify x maxPressTime 1s minPressTime 100ms
  --modify v turbo downFor 10ms delay 10ms
  --modify c delay down 1s up 3s
  --modify b toggle --modify b turbo downFor 10ms delay 10ms
  --modify z replace x                        (z sends x)
  --modify z turbo --modify z replace x    (holding z turbos x)
  --modify z invert                           (fires while NOT held)
  --modify z invert --modify z turbo       (turbos z while z not held)
  --modify x from dev1 replace y                 (x from dev1 sends y)
  --modify x from dev1 replace y from dev2       (x from dev1 sends y,
                                                    tagged as if from dev2)
  --modify x from dev2 turbo                     (x from dev2 turbos)
  --modify x turbo --modify x from dev2 replace y (x turbos everywhere;
                                                    from dev2 it also sends y)
`)
}

func main() {
	var (
		ArgTypeKey = argtree.ArgType{
			Name: "KeyName",
			Transform: func(s string) (any, error) {
				key, ok := input.StringToKey[s]
				if !ok {
					return nil, fmt.Errorf("not a valid key name")
				}
				return key, nil
			},
			List: func() []string {
				return slices.Collect(maps.Keys(input.StringToKey))
			},
		}
		ArgTypeDevice = argtree.ArgType{
			Name: "DeviceName",
			Transform: func(s string) (any, error) {
				return s, nil
			},
		}
	)

	var postKeySelect = []argtree.ArgPossibility{
		{
			Type: argtree.MakeArgTypeLiteral("replace"),
			Name: "modMethod",
			Children: []argtree.ArgPossibility{
				{
					Type: ArgTypeKey,
					Name: "replaceKey",
					Children: []argtree.ArgPossibility{
						{
							Type: argtree.MakeArgTypeLiteral("from"),
							Children: []argtree.ArgPossibility{
								{
									Type:      ArgTypeDevice,
									Name:      "replaceDevice",
									EndAction: argtree.EndActionLoop,
								},
							},
						},
					},
				},
				{
					Type:      ArgTypeKey,
					Name:      "replaceKey",
					EndAction: argtree.EndActionLoop,
				},
				{
					Type: argtree.MakeArgTypeLiteral("combo"),
					Name: "replaceCombo",
					Children: []argtree.ArgPossibility{
						{
							Type: argtree.MakeArgTypeLiteral("takeover"),
							Name: "replaceTakeover",
							Children: []argtree.ArgPossibility{
								{
									Type:      ArgTypeKey,
									Name:      "comboKeys",
									Repeat:    true,
									EndAction: argtree.EndActionLoop,
								},
							},
						},
						{
							Type:      ArgTypeKey,
							Name:      "comboKeys",
							Repeat:    true,
							EndAction: argtree.EndActionLoop,
						},
					},
				},
			},
		},
		{
			Type:      argtree.MakeArgTypeLiteral("toggle"),
			Name:      "modMethod",
			EndAction: argtree.EndActionLoop,
		},
		{
			Type: argtree.MakeArgTypeLiteral("maxPressTime"),
			Name: "modMethod",
			Children: []argtree.ArgPossibility{
				{
					Type:      argtree.ArgTypeTime,
					Name:      "maxPressTime",
					Children:  []argtree.ArgPossibility{},
					EndAction: argtree.EndActionLoop,
				},
			},
		},
		{
			Type: argtree.MakeArgTypeLiteral("minPressTime"),
			Name: "modMethod",
			Children: []argtree.ArgPossibility{
				{
					Type:      argtree.ArgTypeTime,
					Name:      "minPressTime",
					Children:  []argtree.ArgPossibility{},
					EndAction: argtree.EndActionLoop,
				},
			},
		},
		{
			Type: argtree.MakeArgTypeLiteral("delay"),
			Name: "modMethod",
			Children: []argtree.ArgPossibility{
				{
					Type:      argtree.ArgTypeTime,
					Name:      "delayTime",
					Children:  []argtree.ArgPossibility{},
					EndAction: argtree.EndActionLoop,
				},
				{
					Type: argtree.MakeArgTypeLiteral("down"),
					Children: []argtree.ArgPossibility{
						{
							Type:      argtree.ArgTypeTime,
							Name:      "downDelayTime",
							EndAction: argtree.EndActionLoop,
							Children: []argtree.ArgPossibility{
								{
									Type: argtree.MakeArgTypeLiteral("up"),
									Children: []argtree.ArgPossibility{
										{
											Type:      argtree.ArgTypeTime,
											Name:      "upDelayTime",
											EndAction: argtree.EndActionLoop,
										},
									},
								},
							},
						},
					},
				},
				{
					Type: argtree.MakeArgTypeLiteral("up"),
					Children: []argtree.ArgPossibility{
						{
							Type:      argtree.ArgTypeTime,
							Name:      "upDelayTime",
							EndAction: argtree.EndActionLoop,
							Children: []argtree.ArgPossibility{
								{
									Type: argtree.MakeArgTypeLiteral("down"),
									Children: []argtree.ArgPossibility{
										{
											Type:      argtree.ArgTypeTime,
											Name:      "downDelayTime",
											EndAction: argtree.EndActionLoop,
										},
									},
								},
							},
						},
					},
				},
			},
		},
		// Branch 1: turbo with sub-options (downFor, delay, or both in any order)
		{
			Type: argtree.MakeArgTypeLiteral("turbo"),
			Name: "modMethod",
			Children: []argtree.ArgPossibility{
				// turbo downFor <duration> [delay <duration>]
				{
					Type: argtree.MakeArgTypeLiteral("downFor"),
					Children: []argtree.ArgPossibility{
						{
							Type:      argtree.ArgTypeTime,
							Name:      "turboDownTime",
							EndAction: argtree.EndActionLoop,
							Children: []argtree.ArgPossibility{
								{
									Type: argtree.MakeArgTypeLiteral("delay"),
									Children: []argtree.ArgPossibility{
										{
											Type:      argtree.ArgTypeTime,
											Name:      "turboDelayTime",
											EndAction: argtree.EndActionLoop,
										},
									},
								},
							},
						},
					},
				},
				// turbo delay <duration> [downFor <duration>]
				{
					Type: argtree.MakeArgTypeLiteral("delay"),
					Children: []argtree.ArgPossibility{
						{
							Type:      argtree.ArgTypeTime,
							Name:      "turboDelayTime",
							EndAction: argtree.EndActionLoop,
							Children: []argtree.ArgPossibility{
								{
									Type: argtree.MakeArgTypeLiteral("downFor"),
									Children: []argtree.ArgPossibility{
										{
											Type:      argtree.ArgTypeTime,
											Name:      "turboDownTime",
											EndAction: argtree.EndActionLoop,
										},
									},
								},
							},
						},
					},
				},
			},
		},
		// Branch 2: bare turbo (no sub-options)
		{
			Type:      argtree.MakeArgTypeLiteral("turbo"),
			Name:      "modMethod",
			EndAction: argtree.EndActionLoop,
		},
		{
			Type:      argtree.MakeArgTypeLiteral("invert"),
			Name:      "modMethod",
			EndAction: argtree.EndActionLoop,
		},
	}
	cliTree := []argtree.ArgPossibility{
		{
			Type: argtree.MakeArgTypeAny([]string{"modify", "--modify"}),
			Children: []argtree.ArgPossibility{
				{
					Type: ArgTypeKey,
					Name: "sourceKey",
					Children: append(
						[]argtree.ArgPossibility{
							{
								Type: argtree.MakeArgTypeLiteral("from"),
								Children: []argtree.ArgPossibility{
									{
										Type:     ArgTypeDevice,
										Name:     "deviceName",
										Children: postKeySelect,
									},
								},
							},
						},
						postKeySelect...,
					),
				},
			},
		},
	}

	if argtree.CheckCompletionRequest(cliTree) {
		return
	}

	parsed, err := argtree.Parse(cliTree, os.Args[1:])
	if err != nil {
		fmt.Println(err)
		if len(os.Args) == 1 {
			printUsage()
		}
		return
	}
	if len(os.Args) == 1 {
		printUsage()
	}

	// fmt.Print(parsed)
	keyMods := convertParsedToKeyMods(parsed)
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
		fmt.Printf("  %-14s %s\n", keyName+":", keyModifierLib.ModDesc(mod))
	}
	fmt.Println()

	engine := keyModifierLib.NewEngine()
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
func convertParsedToKeyMods(parsed []argtree.OutData) map[keyModifierLib.ModKey]*keyModifierLib.KeyModifier {
	result := make(map[keyModifierLib.ModKey]*keyModifierLib.KeyModifier)

	for _, item := range parsed {
		rawSourceKey, ok := item["sourceKey"]
		if !ok {
			continue
		}
		sourceCode, ok := rawSourceKey.(uint16)
		if !ok {
			continue
		}

		deviceID := ""
		if dev, ok := item["deviceName"].(string); ok {
			deviceID = dev
		}

		mk := keyModifierLib.ModKey{
			Code:   sourceCode,
			Device: deviceID,
		}

		mod, exists := result[mk]
		if !exists {
			mod = &keyModifierLib.KeyModifier{
				DeviceID: deviceID,
			}
			result[mk] = mod
		}

		modMethod, _ := item["modMethod"].(string)
		switch modMethod {
		case "replace":
			if rk, ok := item["replaceKey"].(uint16); ok {
				mod.ReplaceWith = append(mod.ReplaceWith, rk)
			}
			if rd, ok := item["replaceDevice"].(string); ok {
				mod.ReplaceDeviceID = rd
			}

			// Handle combo takeover flag
			if _, ok := item["replaceTakeover"]; ok {
				mod.TakeOver = true
			}

			// Handle combo key sequence slice
			if rawCombo, ok := item["comboKeys"].([]any); ok {
				for _, rawKey := range rawCombo {
					if code, ok := rawKey.(uint16); ok {
						mod.Combo = append(mod.Combo, code)
					}
				}
			}
		case "toggle":
			mod.Toggle = true
		case "invert":
			mod.Invert = true
		case "maxPressTime":
			if d, ok := item["maxPressTime"].(time.Duration); ok {
				mod.MaxPressTime = d
			}
		case "minPressTime":
			if d, ok := item["minPressTime"].(time.Duration); ok {
				mod.MinPressTime = d
			}
		case "delay":
			if d, ok := item["delayTime"].(time.Duration); ok {
				if mod.Delay == nil {
					mod.Delay = &keyModifierLib.DelayConfig{}
				}
				mod.Delay.Down = d
				mod.Delay.Up = d
			}
			if d, ok := item["downDelayTime"].(time.Duration); ok {
				if mod.Delay == nil {
					mod.Delay = &keyModifierLib.DelayConfig{}
				}
				mod.Delay.Down = d
			}
			if d, ok := item["upDelayTime"].(time.Duration); ok {
				if mod.Delay == nil {
					mod.Delay = &keyModifierLib.DelayConfig{}
				}
				mod.Delay.Up = d
			}
		case "turbo":
			if mod.Turbo == nil {
				mod.Turbo = &keyModifierLib.TurboConfig{
					DownFor: 10 * time.Millisecond,
					Delay:   10 * time.Millisecond,
				}
			}
			if d, ok := item["turboDownTime"].(time.Duration); ok {
				mod.Turbo.DownFor = d
			}
			if d, ok := item["turboDelayTime"].(time.Duration); ok {
				mod.Turbo.Delay = d
			}
		}
	}

	return result
}

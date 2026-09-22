package keyModifierLib

import (
	"fmt"
	"os"
	"strings"
	"time"

	input "github.com/rsa17826/go-input-lib"
)

func convertParsedToKeyMods(parsed []map[string]any) map[ModKey]*KeyModifier {
	result := make(map[ModKey]*KeyModifier)

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

		mk := ModKey{
			Code:   sourceCode,
			Device: deviceID,
		}

		mod, exists := result[mk]
		if !exists {
			mod = &KeyModifier{
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
					mod.Delay = &DelayConfig{}
				}
				mod.Delay.Down = d
				mod.Delay.Up = d
			}
		}
	}

	return result
}
func ParseModifyArgs(args []string) map[ModKey]*KeyModifier {
	result := make(map[ModKey]*KeyModifier)
	i := 0
	for i < len(args) {
		if args[i] != "--modify" {
			i++
			continue
		}
		i++

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

		if len(tokens) > 0 && strings.ToLower(tokens[0]) == "to" {
			tokens = tokens[1:]
		}

		code, ok := input.StringToKey[keyName]
		if !ok {
			fmt.Fprintf(os.Stderr, "warning: unknown key %q, %v\n", keyName, input.StringToKey)
			continue
		}

		parsed := &KeyModifier{}
		if err := ApplyTokens(parsed, tokens); err != nil {
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

func mergeModifier(dst, src *KeyModifier) {
	if src.Invert {
		dst.Invert = true
	}
	if len(src.ReplaceWith) > 0 {
		dst.ReplaceWith = append(dst.ReplaceWith, src.ReplaceWith...)
		dst.ReplaceDeviceID = src.ReplaceDeviceID
	}
	if src.Combo != nil {
		dst.Combo = src.Combo
		dst.ReplaceDeviceID = src.ReplaceDeviceID
	}
	if src.TakeOver {
		dst.TakeOver = true
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
}

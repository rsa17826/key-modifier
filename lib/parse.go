package keyModifierLib

import (
	"fmt"
	"os"
	"strings"

	input "github.com/rsa17826/go-input-lib"
)

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

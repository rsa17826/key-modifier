package keyModifierLib

import (
	"fmt"
	"os"
	"strings"

	input "github.com/rsa17826/go-input-lib"
)

// ── Modifier-string parsing ─────────────────────────────────────────────────
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

// ParseModifyArgs parses a slice of CLI-style arguments (as you'd get from
// os.Args) containing one or more "--modify <key> [to] <modifier> [options]"
// blocks, and returns the resulting set of key modifiers. Warnings for
// malformed input are written to os.Stderr.
func ParseModifyArgs(args []string) map[ModKey]*KeyModifier {
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

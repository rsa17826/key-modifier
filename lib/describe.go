package keyModifierLib

import (
	"fmt"
	"strings"

	input "github.com/rsa17826/go-input-lib"
)

// ModDesc renders a KeyModifier as a short human-readable summary, e.g.
// "toggle + turbo(downFor=10ms, delay=10ms)".
func ModDesc(mod *KeyModifier) string {
	var parts []string
	if mod.DeviceID != "" {
		parts = append(parts, fmt.Sprintf("from %s", mod.DeviceID))
	}
	if mod.Invert {
		parts = append(parts, "invert")
	}
	if mod.Combo != nil {
		names := make([]string, len(mod.Combo))
		for idx, c := range mod.Combo {
			name := input.KeyToString[c]
			if name == "" {
				name = fmt.Sprintf("code(%d)", c)
			}
			names[idx] = name
		}
		label := fmt.Sprintf("replace→combo(%s)", strings.Join(names, "+"))
		if mod.TakeOver {
			label += " [takeover]"
		}
		if mod.ReplaceDeviceID != "" {
			label += fmt.Sprintf(" (as if from %s)", mod.ReplaceDeviceID)
		}
		parts = append(parts, label)
	} else if mod.ReplaceWith != nil {
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

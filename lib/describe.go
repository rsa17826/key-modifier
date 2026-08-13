package keyModifierLib

import (
	"fmt"
	"strings"

	input "github.com/rsa17826/go-input-lib"
)

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
	} else if len(mod.ReplaceWith) > 0 {
		names := make([]string, len(mod.ReplaceWith))
		for idx, c := range mod.ReplaceWith {
			name := input.KeyToString[c]
			if name == "" {
				name = fmt.Sprintf("code(%d)", c)
			}
			names[idx] = name
		}
		label := fmt.Sprintf("replace→%s", strings.Join(names, "+"))
		if mod.ReplaceDeviceID != "" {
			label += fmt.Sprintf(" (as if from %s)", mod.ReplaceDeviceID)
		}
		parts = append(parts, label)
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

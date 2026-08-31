//go:build !windows

package workbench

import (
	"gioui.org/app"
	"gioui.org/io/event"
)

func nativeWindowActivity(current uintptr, eventValue event.Event) (uintptr, bool, bool) {
	if currentEvent, ok := eventValue.(app.ConfigEvent); ok {
		return current, currentEvent.Config.Mode == app.Minimized, true
	}
	return current, false, false
}

func nativeWindowDestroyed(event.Event, bool) bool {
	return false
}

//go:build !windows

package waveshare

import "casescreen/internal/telemetry"

func enrichWavesharePlatformTelemetry(snapshot telemetry.Snapshot) telemetry.Snapshot {
	return snapshot
}

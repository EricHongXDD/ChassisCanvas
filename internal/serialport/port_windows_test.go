//go:build windows

package serialport

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsErrorKindSeparatesEnumerationBusyAndBusRemoval(t *testing.T) {
	tests := []struct {
		err  error
		want ErrorKind
	}{
		{windows.ERROR_FILE_NOT_FOUND, ErrorKindNotPresent},
		{windows.ERROR_ACCESS_DENIED, ErrorKindBusy},
		{windows.ERROR_DEVICE_NOT_CONNECTED, ErrorKindDisconnected},
		{windows.ERROR_DEVICE_REMOVED, ErrorKindDisconnected},
		{windows.ERROR_IO_DEVICE, ErrorKindDisconnected},
		{windows.ERROR_GEN_FAILURE, ErrorKindDisconnected},
		{windows.ERROR_SEM_TIMEOUT, ErrorKindTimeout},
		{windows.ERROR_CRC, ErrorKindIO},
	}
	for _, test := range tests {
		if got := windowsErrorKind(test.err); got != test.want {
			t.Errorf("Windows 错误 %v: got=%q want=%q", test.err, got, test.want)
		}
	}
}

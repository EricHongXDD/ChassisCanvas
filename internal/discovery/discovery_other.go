//go:build !windows

package discovery

func findPlatformDevices() []Device {
	return nil
}

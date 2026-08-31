//go:build !windows

package coolermaster

import "os"

func replaceStateFile(source, target string) error {
	return os.Rename(source, target)
}

func syncStateDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

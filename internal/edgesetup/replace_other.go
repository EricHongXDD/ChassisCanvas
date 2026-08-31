//go:build !windows

package edgesetup

import "os"

func replaceFile(source, target string) error {
	return os.Rename(source, target)
}

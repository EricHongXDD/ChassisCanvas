//go:build !windows

package api

import "os"

func replaceThemeFile(source, target string) error {
	return os.Rename(source, target)
}

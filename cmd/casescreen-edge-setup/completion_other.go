//go:build !windows

package main

import "os"

func replaceCompletionMarker(source, target string) error {
	return os.Rename(source, target)
}

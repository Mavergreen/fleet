//go:build !darwin && !linux

// Package term asks the terminal for its width.
package term

// Width is 0 (unknown) where fleet does not ask the terminal.
func Width(uintptr) int { return 0 }

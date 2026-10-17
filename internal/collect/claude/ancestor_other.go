//go:build !linux && !darwin

package claude

func statuslineAncestor() bool { return false }

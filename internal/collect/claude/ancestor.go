package claude

// StatuslineAncestor reports whether a parent process, up to 16 levels up,
// is this same executable running the statusline command. Independent Claude
// sessions do not share a lock; only a process that actually wraps statusline
// stops the nested command.
func StatuslineAncestor() bool {
	return statuslineAncestor()
}

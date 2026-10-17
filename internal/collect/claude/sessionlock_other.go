//go:build !linux && !darwin

package claude

func acquireSessionLock(raw []byte) (bool, func()) {
	return true, func() {}
}

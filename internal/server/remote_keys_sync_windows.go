//go:build windows

package server

// Windows does not expose portable directory fsync semantics. The key file is
// synced before the same-volume rename; the directory sync is a no-op there.
func syncDirectory(string) error { return nil }

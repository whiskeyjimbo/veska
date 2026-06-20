// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

package repo

// InotifyBudget holds the computed inotify watch budget, returning Max as -1 on non-Linux platforms where limits are not enforced.
type InotifyBudget struct {
	Max       int
	InUse     int
	Available int
}

// InotifyFixCommand returns a fallback sysctl command to raise inotify watch limits if executed on non-Linux systems.
func InotifyFixCommand() string {
	return "sudo sysctl -w fs.inotify.max_user_watches=524288"
}

// CheckInotifyBudget is a no-op on non-Linux platforms, always returning a placeholder budget.
func CheckInotifyBudget(_, _ int) (InotifyBudget, error) {
	return InotifyBudget{Max: -1}, nil
}

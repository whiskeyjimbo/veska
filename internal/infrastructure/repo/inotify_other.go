//go:build !linux

package repo

// InotifyBudget holds the computed inotify watch budget.
// On non-Linux platforms, Max is -1 (feature disabled).
type InotifyBudget struct {
	Max       int
	InUse     int
	Available int
}

// InotifyFixCommand returns the sysctl command that raises the inotify watch limit.
// On non-Linux platforms this is provided for completeness but is never needed.
func InotifyFixCommand() string {
	return "sudo sysctl -w fs.inotify.max_user_watches=524288"
}

// CheckInotifyBudget is a no-op on non-Linux platforms.
// It returns InotifyBudget{Max: -1}, nil.
func CheckInotifyBudget(_, _ int) (InotifyBudget, error) {
	return InotifyBudget{Max: -1}, nil
}

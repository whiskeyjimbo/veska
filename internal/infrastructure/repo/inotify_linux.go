//go:build linux

package repo

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// readMaxWatches is a package-level var for testability.
// In production it reads /proc/sys/fs/inotify/max_user_watches.
var readMaxWatches = func() (int, error) {
	data, err := os.ReadFile("/proc/sys/fs/inotify/max_user_watches")
	if err != nil {
		return 0, fmt.Errorf("read inotify max_user_watches: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parse inotify max_user_watches: %w", err)
	}
	return n, nil
}

// InotifyBudget holds the computed inotify watch budget.
type InotifyBudget struct {
	Max       int
	InUse     int
	Available int
}

// InotifyFixCommand returns the sysctl command that raises the inotify watch limit.
func InotifyFixCommand() string {
	return "sudo sysctl -w fs.inotify.max_user_watches=524288"
}

// CheckInotifyBudget reads the kernel inotify watch limit and computes headroom.
// currentWatchers is the number of repos currently being watched; watchesPerRepo
// is the estimated number of inotify watches consumed per repo.
// Returns an error (containing the sysctl fix command) if adding one more repo
// would exceed the budget.
func CheckInotifyBudget(currentWatchers, watchesPerRepo int) (InotifyBudget, error) {
	max, err := readMaxWatches()
	if err != nil {
		return InotifyBudget{}, err
	}

	inUse := currentWatchers * watchesPerRepo
	available := max - inUse

	budget := InotifyBudget{
		Max:       max,
		InUse:     inUse,
		Available: available,
	}

	if available < watchesPerRepo {
		return budget, fmt.Errorf(
			"inotify watch budget exhausted (max=%d, in_use=%d, available=%d, needed=%d): run: %s",
			max, inUse, available, watchesPerRepo, InotifyFixCommand(),
		)
	}

	return budget, nil
}

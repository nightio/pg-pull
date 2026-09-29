//go:build !windows

package update

import (
	"fmt"
	"os"
)

const installScheduled = false

func install(target, stage string) error {
	// The candidate is staged on the target filesystem. POSIX rename replaces
	// the old executable atomically, leaving it intact if replacement fails.
	if err := os.Rename(stage, target); err != nil {
		return fmt.Errorf("replace installed executable %s: %w", target, err)
	}
	return nil
}

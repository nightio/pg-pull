//go:build windows

package update

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

const installScheduled = true

func install(target, stage string) error {
	// Windows keeps the running executable open. Run a copy of the verified
	// candidate outside the installation directory to replace it after exit.
	helper, err := os.CreateTemp("", "pg-pull-update-*.exe")
	if err != nil {
		return fmt.Errorf("create update helper: %w", err)
	}
	helperPath := helper.Name()
	defer helper.Close()
	removeHelper := true
	defer func() {
		if removeHelper {
			os.Remove(helperPath)
		}
	}()
	source, err := os.Open(stage)
	if err != nil {
		return err
	}
	defer source.Close()
	if _, err := io.Copy(helper, source); err != nil {
		return fmt.Errorf("copy update helper: %w", err)
	}
	if err := helper.Close(); err != nil {
		return fmt.Errorf("close update helper: %w", err)
	}
	cmd := exec.Command(helperPath, "--finish-self-update", target, stage)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start update helper: %w", err)
	}
	removeHelper = false
	_ = cmd.Process.Release()
	return nil
}

// FinishIfRequested is called before the normal CLI starts. Its arguments are
// only supplied by the verified helper copied from the release asset.
func FinishIfRequested(args []string) (bool, int) {
	if len(args) == 0 || args[0] != "--finish-self-update" {
		return false, 0
	}
	if len(args) != 3 {
		fmt.Fprintln(os.Stderr, "Invalid self-update helper arguments")
		return true, 2
	}
	defer scheduleHelperRemoval()
	if err := finish(args[1], args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "Self-update failed:", err)
		return true, 1
	}
	fmt.Fprintln(os.Stdout, "Self-update completed. Run pg-pull --version to verify.")
	return true, 0
}

func finish(target, stage string) error {
	if filepath.Dir(target) != filepath.Dir(stage) {
		return errors.New("update files are not in the same directory")
	}
	backup, err := os.CreateTemp(filepath.Dir(target), ".pg-pull-previous-*.exe")
	if err != nil {
		return err
	}
	backupPath := backup.Name()
	backup.Close()
	if err := os.Remove(backupPath); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := os.Rename(target, backupPath); err == nil {
			break
		} else if time.Now().After(deadline) {
			return fmt.Errorf("wait for running executable to exit: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := os.Rename(stage, target); err != nil {
		if rollbackErr := os.Rename(backupPath, target); rollbackErr != nil {
			return fmt.Errorf("install new executable: %v; restore previous executable from %s: %w", err, backupPath, rollbackErr)
		}
		return fmt.Errorf("install new executable: %w", err)
	}
	if err := os.Remove(backupPath); err != nil {
		fmt.Fprintln(os.Stderr, "Updated, but could not remove previous executable:", backupPath, err)
	}
	return nil
}

func scheduleHelperRemoval() {
	path, err := os.Executable()
	if err != nil {
		return
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	// A running .exe cannot delete itself; Windows removes it on reboot.
	_ = windows.MoveFileEx(name, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
}

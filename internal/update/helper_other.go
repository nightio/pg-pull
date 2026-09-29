//go:build !windows

package update

func FinishIfRequested(_ []string) (bool, int) { return false, 0 }

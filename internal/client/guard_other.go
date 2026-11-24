//go:build !windows

package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Enforcement has no non-Windows implementation. The lab is a Windows 10 room;
// this file keeps the client building and running on macOS and Linux for
// development, where everything above the enforcer interface is exercisable.
//
// available() reports false with a reason rather than silently doing nothing,
// so a policy that asks for enforcement on such a machine produces a visible
// guard_unavailable alert instead of a teacher believing a room is locked.

func newEnforcer() enforcer { return nullEnforcer{} }

type nullEnforcer struct{}

var errUnsupported = errors.New("本平台不支持考场管控")

func (nullEnforcer) available() (bool, string) {
	return false, "当前客户端不是 Windows 版本，无法执行禁用 U 盘 / 断网等管控操作。"
}

func (nullEnforcer) snapshotUSB() (usbSnapshot, error) { return usbSnapshot{}, errUnsupported }
func (nullEnforcer) disableUSBStorage() error          { return errUnsupported }
func (nullEnforcer) restoreUSB(usbSnapshot) error      { return errUnsupported }
func (nullEnforcer) snapshotFirewall() (firewallSnapshot, error) {
	return firewallSnapshot{}, errUnsupported
}
func (nullEnforcer) blockInternet([]string) error            { return errUnsupported }
func (nullEnforcer) restoreFirewall(firewallSnapshot) error  { return errUnsupported }
func (nullEnforcer) installRecovery(string, int, bool) error { return errUnsupported }
func (nullEnforcer) removeRecovery(string) error             { return errUnsupported }

func guardJournalPath(dataDir string) string {
	return filepath.Join(dataDir, "guard.json")
}

// waitForProcessExit polls, since there is no handle to wait on portably.
// Only the development build takes this path.
func waitForProcessExit(ctx context.Context, pid int) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p, err := os.FindProcess(pid)
			if err != nil {
				return
			}
			if err := p.Signal(syscall.Signal(0)); err != nil {
				return
			}
		}
	}
}

func spawnWatchdog(string) (*os.Process, error) { return nil, errUnsupported }

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// rebootedSince has no portable implementation; the development build never
// enforces anything, so nothing depends on the answer.
func rebootedSince(int64) bool { return false }

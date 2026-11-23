//go:build !windows

package client

// The behaviour monitor has no non-Windows implementation. The lab is a
// Windows 10 room; this file exists so the client still builds and runs on
// macOS and Linux for development, where the UI, the network path and the
// business logic are all exercisable.
//
// Reporting every capability as absent (rather than quietly returning empty
// lists) is the point: a policy that asks for USB monitoring on a machine that
// cannot do it produces a monitor_degraded event on the teacher's dashboard.
// An unmonitored machine must never be indistinguishable from a clean one.

func newSysProbe() sysProbe { return nullProbe{} }

type nullProbe struct{}

func (nullProbe) caps() probeCaps                  { return probeCaps{} }
func (nullProbe) drives() ([]driveInfo, error)     { return nil, nil }
func (nullProbe) processes() ([]procInfo, error)   { return nil, nil }
func (nullProbe) foreground() (fgInfo, error)      { return fgInfo{}, nil }
func (nullProbe) connections() ([]connInfo, error) { return nil, nil }

package services

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/aiseeq/graft/internal/config"
)

// systemctl runs systemctl --user and returns its output.
func systemctl(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// unitState returns the main pid of a unit (0 when it does not run) and its
// ActiveState: active, inactive, failed, activating, ...
func unitState(ctx context.Context, unit string) (int, string, error) {
	out, err := systemctl(ctx, "show", "--property=MainPID,ActiveState", unit)
	if err != nil {
		return 0, "", err
	}
	var pid int
	var state string
	for line := range strings.SplitSeq(out, "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch key {
		case "MainPID":
			if pid, err = strconv.Atoi(value); err != nil {
				return 0, "", fmt.Errorf("systemctl show %s: MainPID %q: %w", unit, value, err)
			}
		case "ActiveState":
			state = value
		}
	}
	if state == "" {
		return 0, "", fmt.Errorf("systemctl show %s: no ActiveState in %q", unit, out)
	}
	return pid, state, nil
}

func (m *Manager) startUnit(ctx context.Context, name string, s *config.Service) error {
	pid, state, err := unitState(ctx, s.SystemdUnit)
	if err != nil {
		return err
	}
	if state == "active" {
		fmt.Fprintf(m.Log, "graft: %s is already running (%s, pid %d)\n", name, s.SystemdUnit, pid)
		return nil
	}
	addr, err := m.prepareStart(ctx, name, s)
	if err != nil {
		return err
	}
	if err := m.reloadIfChanged(ctx, s.SystemdUnit); err != nil {
		return err
	}
	if _, err := systemctl(ctx, "start", s.SystemdUnit); err != nil {
		return fmt.Errorf("%s: %w (see graft logs %s)", name, err, name)
	}
	ready, err := waitFor(ctx, s.StartTimeout, func() (bool, error) {
		_, state, err := unitState(ctx, s.SystemdUnit)
		switch {
		case err != nil:
			return false, err
		case state == "failed":
			return false, fmt.Errorf("%s: %s failed (see graft logs %s)", name, s.SystemdUnit, name)
		case state != "active":
			return false, nil
		case addr == "":
			return true, nil
		default:
			return listening(addr)
		}
	})
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("%s: %s not ready after %s (see graft logs %s)", name, s.SystemdUnit, s.StartTimeout, name)
	}
	fmt.Fprintf(m.Log, "graft: started %s (%s)\n", name, s.SystemdUnit)
	return nil
}

func (m *Manager) stopUnit(ctx context.Context, name string, s *config.Service) error {
	_, state, err := unitState(ctx, s.SystemdUnit)
	if err != nil {
		return err
	}
	if state == "inactive" || state == "failed" {
		fmt.Fprintf(m.Log, "graft: %s is not running (%s is %s)\n", name, s.SystemdUnit, state)
		return nil
	}
	if _, err := systemctl(ctx, "stop", s.SystemdUnit); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return m.stopped(ctx, name, s)
}

// reloadIfChanged runs daemon-reload when the unit file changed since
// systemd read it: starting it otherwise runs the old definition.
func (m *Manager) reloadIfChanged(ctx context.Context, unit string) error {
	out, err := systemctl(ctx, "show", "--property=NeedDaemonReload", unit)
	if err != nil {
		return err
	}
	switch strings.TrimSpace(out) {
	case "NeedDaemonReload=no":
		return nil
	case "NeedDaemonReload=yes":
		fmt.Fprintf(m.Log, "graft: %s changed on disk, systemctl --user daemon-reload\n", unit)
		_, err := systemctl(ctx, "daemon-reload")
		return err
	default:
		return fmt.Errorf("systemctl show %s: unexpected %q", unit, strings.TrimSpace(out))
	}
}

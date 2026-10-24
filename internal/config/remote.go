package config

import (
	"errors"
	"fmt"
)

// RemoteHeavy sends the steps of heavy tasks and graft exec --heavy to
// another machine first: the laptop is shared, a build host is not. The
// project's own command carries the tree there and runs the step; graft
// only decides where a step runs.
type RemoteHeavy struct {
	// Host names the machine in graft's lines and in stats.
	Host string `yaml:"host"`
	// Up exits 0 when the host takes commands, quickly; any other exit
	// keeps the heavy work on this machine, said in a line.
	Up []string `yaml:"up"`
	// Run runs a command on the host from the work tree root, with the
	// step's argv appended, and exits with the command's status.
	Run []string `yaml:"run"`
	// Threads fills {threads} and GRAFT_THREADS of a step run there; 0
	// leaves them to the host, and a step that uses {threads} is refused.
	Threads int `yaml:"threads"`
}

func (c *Config) validateRemoteHeavy() error {
	for _, name := range keysOf(c.Tasks) {
		if t := c.Tasks[name]; t.Remote != nil && !t.Heavy {
			return fmt.Errorf("tasks.%s.remote: only a heavy task runs on remote_heavy's host", name)
		}
	}
	r := c.RemoteHeavy
	if r == nil {
		return nil
	}
	switch {
	case r.Host == "":
		return errors.New("remote_heavy.host: name the machine")
	case len(r.Up) == 0 || r.Up[0] == "":
		return errors.New("remote_heavy.up: a command that exits 0 when the host takes commands")
	case len(r.Run) == 0 || r.Run[0] == "":
		return errors.New("remote_heavy.run: the command that runs a step on the host")
	case r.Threads < 0:
		return fmt.Errorf("remote_heavy.threads: %d is below 0", r.Threads)
	}
	return nil
}

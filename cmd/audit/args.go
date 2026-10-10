package main

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Commands.
const cmdRebuild = "rebuild"

const usage = `usage:
  audit rebuild <finding-id>`

// errUsage is wrapped by every argument error.
var errUsage = errors.New("audit: bad arguments")

// command is one parsed command line.
type command struct {
	name      string
	findingID uuid.UUID
}

func usageErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s\n%s", errUsage, fmt.Sprintf(format, a...), usage)
}

// parseArgs parses a command and its arguments.
func parseArgs(args []string) (command, error) {
	if len(args) == 0 {
		return command{}, usageErr("no command")
	}
	switch args[0] {
	case cmdRebuild:
		if len(args) != 2 {
			return command{}, usageErr("rebuild needs exactly one finding ID")
		}
		id, err := uuid.Parse(args[1])
		if err != nil || id == uuid.Nil {
			return command{}, usageErr("finding ID %.40q is not a UUID", args[1])
		}
		return command{name: cmdRebuild, findingID: id}, nil
	}
	return command{}, usageErr("unknown command %.40q", args[0])
}

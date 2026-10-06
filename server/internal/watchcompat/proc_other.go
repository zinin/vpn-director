//go:build !linux

package watchcompat

import (
	"context"
	"os/exec"
)

func identifyExecutable(string) (executableIdentity, error) {
	return executableIdentity{}, ErrIncompatible
}

func runningBots(context.Context, string, string) ([]executableTarget, error) {
	return nil, ErrIncompatible
}

func prepareCapabilityCommand(*exec.Cmd) {}

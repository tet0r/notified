//go:build !linux

package main

import (
	"context"
	"errors"
)

func readSysStats(ctx context.Context, paths []string) (sysStats, error) {
	return sysStats{}, errors.New("system monitoring is only supported on Linux (run Notified in Docker)")
}

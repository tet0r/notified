//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// procPath resolves a file under /proc. HOST_PROC can point at a mounted
// host /proc, though the default already reports host-wide numbers in Docker.
func procPath(name string) string {
	return env("HOST_PROC", "/proc") + "/" + name
}

func readCPUTimes() (idle, total uint64, err error) {
	data, err := os.ReadFile(procPath("stat"))
	if err != nil {
		return 0, 0, err
	}
	line, _, _ := strings.Cut(string(data), "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0, errors.New("unexpected /proc/stat format")
	}
	for i, v := range f[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		// Guest time (fields 9 and 10) is already included in user/nice.
		if i < 8 {
			total += n
		}
		if i == 3 || i == 4 { // idle + iowait
			idle += n
		}
	}
	return idle, total, nil
}

func readSysStats(ctx context.Context, paths []string) (sysStats, error) {
	var st sysStats

	idle1, total1, err := readCPUTimes()
	if err != nil {
		return st, err
	}
	select {
	case <-ctx.Done():
		return st, ctx.Err()
	case <-time.After(time.Second):
	}
	idle2, total2, err := readCPUTimes()
	if err != nil {
		return st, err
	}
	if total2 > total1 {
		st.CPU = 100 * (1 - float64(idle2-idle1)/float64(total2-total1))
	}

	if data, err := os.ReadFile(procPath("loadavg")); err == nil {
		f := strings.Fields(string(data))
		for i := 0; i < 3 && i < len(f); i++ {
			st.Load[i], _ = strconv.ParseFloat(f[i], 64)
		}
	}

	data, err := os.ReadFile(procPath("meminfo"))
	if err != nil {
		return st, err
	}
	var avail uint64
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			st.MemTotal = kb * 1024
		case "MemAvailable:":
			avail = kb * 1024
		}
	}
	if st.MemTotal >= avail {
		st.MemUsed = st.MemTotal - avail
	}

	if data, err := os.ReadFile(procPath("uptime")); err == nil {
		if f := strings.Fields(string(data)); len(f) > 0 {
			secs, _ := strconv.ParseFloat(f[0], 64)
			st.Uptime = time.Duration(secs * float64(time.Second))
		}
	}

	for _, p := range paths {
		var fs syscall.Statfs_t
		if err := syscall.Statfs(p, &fs); err != nil {
			st.DiskErrs = append(st.DiskErrs, p+": "+err.Error())
			continue
		}
		bs := uint64(fs.Bsize)
		total := fs.Blocks * bs
		free := fs.Bavail * bs
		if total == 0 {
			continue
		}
		st.Disks = append(st.Disks, diskStat{Path: p, Used: total - free, Total: total})
	}
	return st, nil
}

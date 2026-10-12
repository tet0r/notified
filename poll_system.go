package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type diskStat struct {
	Path  string
	Used  uint64
	Total uint64
}

type sysStats struct {
	CPU      float64 // percent busy
	Load     [3]float64
	MemUsed  uint64
	MemTotal uint64
	Uptime   time.Duration
	Disks    []diskStat
	DiskErrs []string
}

func pct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

func pollSystem(ctx context.Context, a *App, in *Integration) error {
	st, err := readSysStats(ctx, splitList(in.Config["disk_paths"]))
	if err != nil {
		return err
	}

	type metric struct {
		key, event, label, detail string
		value, limit              float64
		need                      int // consecutive breaches before alerting
	}
	ms := []metric{
		{"cpu", "cpu_high", "CPU usage", fmt.Sprintf("Load average %.2f / %.2f / %.2f", st.Load[0], st.Load[1], st.Load[2]),
			st.CPU, cfgFloat(in.Config, "cpu_threshold", 90), 3},
		{"mem", "memory_high", "Memory usage", humanBytes(float64(st.MemUsed)) + " of " + humanBytes(float64(st.MemTotal)),
			pct(st.MemUsed, st.MemTotal), cfgFloat(in.Config, "mem_threshold", 90), 1},
	}
	diskLimit := cfgFloat(in.Config, "disk_threshold", 90)
	for _, d := range st.Disks {
		ms = append(ms, metric{"disk:" + d.Path, "disk_high", "Disk usage on " + d.Path,
			humanBytes(float64(d.Total-d.Used)) + " free of " + humanBytes(float64(d.Total)),
			pct(d.Used, d.Total), diskLimit, 1})
	}

	for _, m := range ms {
		if m.limit <= 0 {
			continue
		}
		key := "alert:" + m.key
		prev, _ := a.stateGet(in.ID, key)
		n, _ := strconv.Atoi(prev)
		if m.value >= m.limit {
			n++
			if n == m.need {
				e := Embed{Title: m.label + " is high", Color: colorRed, Author: &EmbedAuthor{Name: in.Name}}
				e.Description = fmt.Sprintf("**%.1f%%** (threshold %.0f%%)", m.value, m.limit)
				e.Add("Details", m.detail, false)
				a.Dispatch(in, m.event, e, nil)
			}
			if n > m.need {
				n = m.need + 1
			}
			a.stateSet(in.ID, key, strconv.Itoa(n))
		} else if n > 0 {
			if n >= m.need {
				e := Embed{Title: m.label + " is back to normal", Color: colorGreen, Author: &EmbedAuthor{Name: in.Name}}
				e.Description = fmt.Sprintf("**%.1f%%** (threshold %.0f%%)", m.value, m.limit)
				a.Dispatch(in, "recovered", e, nil)
			}
			a.stateDel(in.ID, key)
		}
	}

	if hours := cfgFloat(in.Config, "snapshot_hours", 0); hours > 0 {
		last, _ := a.stateGet(in.ID, "last_snapshot")
		lastUnix, _ := strconv.ParseInt(last, 10, 64)
		if time.Since(time.Unix(lastUnix, 0)) >= time.Duration(hours*float64(time.Hour)) {
			e := Embed{Title: "System snapshot", Color: colorBlue, Author: &EmbedAuthor{Name: in.Name}}
			e.Add("CPU", fmt.Sprintf("%.1f%%", st.CPU), true)
			e.Add("Load", fmt.Sprintf("%.2f / %.2f / %.2f", st.Load[0], st.Load[1], st.Load[2]), true)
			e.Add("Memory", fmt.Sprintf("%.1f%% (%s of %s)", pct(st.MemUsed, st.MemTotal),
				humanBytes(float64(st.MemUsed)), humanBytes(float64(st.MemTotal))), true)
			if st.Uptime > 0 {
				e.Add("Uptime", humanDuration(st.Uptime), true)
			}
			for _, d := range st.Disks {
				e.Add("Disk "+d.Path, fmt.Sprintf("%.1f%% (%s free)", pct(d.Used, d.Total), humanBytes(float64(d.Total-d.Used))), true)
			}
			a.Dispatch(in, "snapshot", e, nil)
			a.stateSet(in.ID, "last_snapshot", strconv.FormatInt(time.Now().Unix(), 10))
		}
	}

	if len(st.DiskErrs) > 0 {
		return errors.New("disk paths not readable: " + strings.Join(st.DiskErrs, "; "))
	}
	return nil
}

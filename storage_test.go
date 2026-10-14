package main

import (
	"strings"
	"testing"
)

const mountinfoBase = `1481 1300 0:92 / / rw,relatime master:413 - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/l/ABC
1482 1481 0:95 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
1483 1481 0:96 / /dev rw,nosuid - tmpfs tmpfs rw,size=65536k,mode=755
1490 1481 8:1 /var/lib/docker/containers/abc/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/sda1 rw
`

func TestStorageWarning(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		want  string // substring of the warning, "" for none
	}{
		{"host folder bind mount", "1491 1481 8:1 /srv/notified/data /data rw,relatime - ext4 /dev/sda1 rw\n", ""},
		{"named volume", "1491 1481 8:1 /var/lib/docker/volumes/notified_data/_data /data rw,relatime - ext4 /dev/sda1 rw\n", ""},
		{"compose project volume", "1491 1481 8:1 /var/lib/docker/volumes/stack_notified-data/_data /data rw,relatime - ext4 /dev/sda1 rw\n", ""},
		{"anonymous volume", "1491 1481 8:1 /var/lib/docker/volumes/3f2a9c0d1e4b5a693f2a9c0d1e4b5a693f2a9c0d1e4b5a693f2a9c0d1e4b5a69/_data /data rw,relatime - ext4 /dev/sda1 rw\n", "unnamed Docker volume"},
		{"anonymous volume, docker root on its own disk", "1491 1481 8:17 /volumes/3f2a9c0d1e4b5a693f2a9c0d1e4b5a693f2a9c0d1e4b5a693f2a9c0d1e4b5a69/_data /data rw,relatime - ext4 /dev/sdb1 rw\n", "unnamed Docker volume"},
		{"bind mount backed by memory (Docker Desktop, Portainer stack with ./data)", "1491 1481 0:31 /data/compose/44/data /data rw,relatime - tmpfs tmpfs rw\n", "temporary in-memory filesystem"},
		{"nothing mounted", "", "Nothing is mounted at /data"},
		{"a mount below the data dir does not count", "1491 1481 8:1 /srv/x /data/sub rw,relatime - ext4 /dev/sda1 rw\n", "Nothing is mounted at /data"},
	}
	for _, c := range cases {
		got := storageWarning("/data", strings.NewReader(mountinfoBase+c.extra))
		if c.want == "" && got != "" {
			t.Errorf("%s: unexpected warning %q", c.name, got)
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want it to contain %q", c.name, got, c.want)
		}
	}
	// A trailing slash on the configured directory must not hide a real mount.
	if got := storageWarning("/data/", strings.NewReader(mountinfoBase+"1491 1481 8:1 /srv/n /data rw - ext4 /dev/sda1 rw\n")); got != "" {
		t.Errorf("trailing slash: unexpected warning %q", got)
	}
}

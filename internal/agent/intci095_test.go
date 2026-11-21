package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestINTCI095WholeDiskResolution(t *testing.T) {
	tests := []struct {
		name, source, target, want, wantErr string
		disks                               []string
		parts                               map[string]string
		slaves                              map[string][]string
	}{
		{name: "dm single", source: "/dev/mapper/cow-root", target: "dm-0", want: "/dev/nvme0n1", disks: []string{"dm-0", "nvme0n1"}, slaves: map[string][]string{"dm-0": {"nvme0n1"}}},
		{name: "dm stack", source: "/dev/mapper/crypt", target: "dm-2", want: "/dev/sda", disks: []string{"dm-2", "dm-1", "sda"}, slaves: map[string][]string{"dm-2": {"dm-1"}, "dm-1": {"sda"}}},
		{name: "dm empty", source: "/dev/mapper/empty", target: "dm-0", wantErr: "device-mapper", disks: []string{"dm-0"}},
		{name: "partition", source: "/dev/nvme0n1p2", target: "nvme0n1p2", want: "/dev/nvme0n1", disks: []string{"nvme0n1"}, parts: map[string]string{"nvme0n1p2": "nvme0n1"}},
		{name: "devtmpfs", source: "/dev/root", target: "vda1", want: "/dev/vda", disks: []string{"vda"}, parts: map[string]string{"vda1": "vda"}},
		{name: "partition slave", source: "/dev/mapper/root", target: "dm-0", want: "/dev/sda", disks: []string{"dm-0", "sda"}, parts: map[string]string{"sda2": "sda"}, slaves: map[string][]string{"dm-0": {"sda2"}}},
		{name: "cycle", source: "/dev/mapper/root", target: "dm-0", wantErr: "cycle", disks: []string{"dm-0", "dm-1"}, slaves: map[string][]string{"dm-0": {"dm-1"}, "dm-1": {"dm-0"}}},
		{name: "multiple disks", source: "/dev/mapper/root", target: "dm-0", wantErr: "multiple", disks: []string{"dm-0", "sda", "sdb"}, slaves: map[string][]string{"dm-0": {"sda", "sdb"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			block := filepath.Join(root, "sys", "block")
			devblock := filepath.Join(root, "sys", "dev", "block")
			for _, disk := range tc.disks {
				if err := os.MkdirAll(filepath.Join(block, disk, "slaves"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for part, disk := range tc.parts {
				if err := os.MkdirAll(filepath.Join(block, disk, part), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for disk, slaves := range tc.slaves {
				for _, slave := range slaves {
					if err := os.Symlink(filepath.Join("..", "..", slave), filepath.Join(block, disk, "slaves", slave)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := os.MkdirAll(devblock, 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(block, tc.target)
			if disk, ok := tc.parts[tc.target]; ok {
				target = filepath.Join(block, disk, tc.target)
			}
			if err := os.Symlink(target, filepath.Join(devblock, "252:0")); err != nil {
				t.Fatal(err)
			}
			mountinfo := filepath.Join(root, "mountinfo")
			if err := os.WriteFile(mountinfo, []byte("36 35 252:0 / / rw - ext4 "+tc.source+" rw\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			oldMount, oldBlock, oldDev := rootDeviceMountInfoPath, wholeDiskSysBlockRoot, sysDevBlockRoot
			rootDeviceMountInfoPath, wholeDiskSysBlockRoot, sysDevBlockRoot = mountinfo, block, devblock
			t.Cleanup(func() { rootDeviceMountInfoPath, wholeDiskSysBlockRoot, sysDevBlockRoot = oldMount, oldBlock, oldDev })
			got, err := resolveWholeDiskDeviceReal()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %q, %v; want error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

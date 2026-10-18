package fuse

import (
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestDirectMountParams(t *testing.T) {
	const defaultFlags = syscall.MS_NOSUID | syscall.MS_NODEV

	tests := []struct {
		name    string
		options map[string]string
		want    directMountParams
	}{
		{
			name:    "defaults",
			options: map[string]string{},
			want:    directMountParams{source: "fuse", fstype: "fuse", flags: defaultFlags},
		},
		{
			name: "sysbox-fs options",
			options: map[string]string{
				"fsname":              "sysboxfs",
				"allow_other":         "",
				"default_permissions": "",
			},
			want: directMountParams{
				source: "sysboxfs",
				fstype: "fuse",
				flags:  defaultFlags,
				data:   []string{"allow_other", "default_permissions"},
			},
		},
		{
			name:    "subtype",
			options: map[string]string{"subtype": "foo"},
			want:    directMountParams{source: "fuse", fstype: "fuse.foo", flags: defaultFlags},
		},
		{
			name:    "read only",
			options: map[string]string{"ro": ""},
			want:    directMountParams{source: "fuse", fstype: "fuse", flags: defaultFlags | syscall.MS_RDONLY},
		},
		{
			name:    "dev and suid",
			options: map[string]string{"dev": "", "suid": ""},
			want:    directMountParams{source: "fuse", fstype: "fuse", flags: 0},
		},
		{
			name:    "kernel options with values",
			options: map[string]string{"max_read": "131072", "blksize": "4096"},
			want: directMountParams{
				source: "fuse",
				fstype: "fuse",
				flags:  defaultFlags,
				data:   []string{"blksize=4096", "max_read=131072"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getDirectMountParams(&mountConfig{options: tt.options})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(*got, tt.want) {
				t.Errorf("got %+v, want %+v", *got, tt.want)
			}
		})
	}
}

func TestDirectMountParamsUnsupported(t *testing.T) {
	tests := []map[string]string{
		{"nonempty": ""},
		{"ro": "1"},
		{"allow_other": "", "context": "system_u:object_r:fusefs_t:s0"},
	}
	for _, options := range tests {
		_, err := getDirectMountParams(&mountConfig{options: options})
		if err == nil {
			t.Errorf("options %v: expected error", options)
			continue
		}
		if !strings.Contains(err.Error(), "not supported") {
			t.Errorf("options %v: unexpected error: %v", options, err)
		}
	}
}

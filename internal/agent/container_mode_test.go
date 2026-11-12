package agent

import (
	"reflect"
	"testing"
)

func TestContainerModeRunArgs(t *testing.T) {
	got, err := containerModeRunArgs("abc", 1001, 1002, 10000, 10099, 10042, 8080, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--host", "unix:///run/bunker/abc/docker.sock", "run", "-d", "--name", "bunker-abc", "--user", "1001:1002", "-e", "HOME=/home/1001", "-w", "/home/1001", "-v", "/home/bunker-abc:/home/1001", "-p", "127.0.0.1:10042:8080", "docker.io/library/ubuntu:24.04", "sleep", "infinity"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for _, a := range got {
		if a == "--privileged" || a == "--network=host" || a == "--pid=host" || a == "--ipc=host" {
			t.Fatalf("unsafe flag: %s", a)
		}
	}
	wipe, err := containerModeRunArgs("abc", 1001, 1002, 10000, 10099, 10042, 8080, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range wipe {
		if a == "-v" {
			t.Fatal("full wipe must not bind the home")
		}
	}
}

func TestContainerModeRunArgsRejectsInvalidInputs(t *testing.T) {
	for _, tc := range []struct {
		id                       string
		uid, gid                 int
		start, end, host, inside uint32
	}{
		{"../other", 1000, 1000, 10000, 10099, 10001, 80},
		{"ok", 0, 1000, 10000, 10099, 10001, 80},
		{"ok", 1000, 1000, 10000, 10099, 10100, 80},
		{"ok", 1000, 1000, 10000, 10099, 10001, 0},
	} {
		if _, err := containerModeRunArgs(tc.id, tc.uid, tc.gid, tc.start, tc.end, tc.host, tc.inside, false); err == nil {
			t.Errorf("accepted invalid input: %+v", tc)
		}
	}
}

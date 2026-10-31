package adb

import (
	"errors"
	"testing"
)

func TestParseRoot(t *testing.T) {
	out, err := parseRoot("uid=0(root)\n" + rcMark + "0\n")
	if err != nil || out != "uid=0(root)\n" {
		t.Fatalf("ok: %q %v", out, err)
	}
	_, err = parseRoot("test failed\n" + rcMark + "1\n")
	var re *RootError
	if !errors.As(err, &re) || re.Code != 1 || re.Output != "test failed\n" {
		t.Fatalf("fail: %v", err)
	}
	if _, err := parseRoot("mysudo: permission denied\n"); err == nil || errors.As(err, &re) {
		t.Fatalf("no status: %v", err)
	}
}

func TestDirOf(t *testing.T) {
	for in, want := range map[string]string{"/a/b/c": "/a/b", "/c": "/", "c": "/"} {
		if got := dirOf(in); got != want {
			t.Errorf("dirOf(%q) = %q, want %q", in, got, want)
		}
	}
}

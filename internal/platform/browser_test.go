package platform

import "testing"

func TestBrowserCommandsDoNotUseShells(t *testing.T) {
	for _, goos := range []string{"windows", "darwin", "linux"} {
		name, args, err := browserCommand(goos, "http://127.0.0.1:7342/")
		if err != nil {
			t.Fatalf("browserCommand(%q) error = %v", goos, err)
		}
		if name == "" || len(args) == 0 || args[len(args)-1] != "http://127.0.0.1:7342/" {
			t.Fatalf("browserCommand(%q) = %q %#v", goos, name, args)
		}
	}
	if _, _, err := browserCommand("plan9", "http://127.0.0.1:7342/"); err == nil {
		t.Fatal("browserCommand(unsupported) error = nil")
	}
}

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

func TestRevealCommandsDoNotUseShells(t *testing.T) {
	for _, goos := range []string{"windows", "darwin", "linux"} {
		name, args, err := revealCommand(goos, "/trusted/asset.png")
		if err != nil {
			t.Fatalf("revealCommand(%q) error = %v", goos, err)
		}
		if name == "" || len(args) == 0 {
			t.Fatalf("revealCommand(%q) = %q %#v", goos, name, args)
		}
	}
	if _, _, err := revealCommand("plan9", "/trusted/asset.png"); err == nil {
		t.Fatal("revealCommand(unsupported) error = nil")
	}
}

func TestViewerCommandsDoNotUseShells(t *testing.T) {
	tests := []struct {
		name         string
		goos         string
		expectedName string
		expectedArgs []string
	}{
		{
			name:         "windows",
			goos:         "windows",
			expectedName: "rundll32.exe",
			expectedArgs: []string{"url.dll,FileProtocolHandler", "/trusted/asset.png"},
		},
		{
			name:         "macos",
			goos:         "darwin",
			expectedName: "open",
			expectedArgs: []string{"/trusted/asset.png"},
		},
		{
			name:         "linux",
			goos:         "linux",
			expectedName: "xdg-open",
			expectedArgs: []string{"/trusted/asset.png"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name, args, err := viewerCommand(test.goos, "/trusted/asset.png")
			if err != nil {
				t.Fatalf("viewerCommand(%q) error = %v", test.goos, err)
			}
			if name != test.expectedName || len(args) != len(test.expectedArgs) {
				t.Fatalf("viewerCommand(%q) = %q %#v", test.goos, name, args)
			}
			for i := range args {
				if args[i] != test.expectedArgs[i] {
					t.Fatalf("viewerCommand(%q) args = %#v, want %#v", test.goos, args, test.expectedArgs)
				}
			}
		})
	}
	if _, _, err := viewerCommand("plan9", "/trusted/asset.png"); err == nil {
		t.Fatal("viewerCommand(unsupported) error = nil")
	}
}

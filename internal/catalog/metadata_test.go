package catalog

import "testing"

func TestPrototypeDerivedLayouts(t *testing.T) {
	tests := map[string]struct {
		layout        Layout
		width, height int
	}{
		"vehicle sprite sheet": {
			layout: Layout{Kind: LayoutKindSpriteSheet, Columns: 4, Rows: 4, CellWidth: 128, CellHeight: 128, FrameCount: 16, AnimationLabel: "drive"},
			width:  512, height: 512,
		},
		"environment tile sheet": {
			layout: Layout{Kind: LayoutKindTileSheet, Columns: 16, Rows: 9, CellWidth: 32, CellHeight: 32},
			width:  512, height: 288,
		},
		"single terrain tile": {
			layout: Layout{Kind: LayoutKindSingle},
			width:  32, height: 32,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if err := test.layout.Validate(test.width, test.height); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestLayoutRejectsInvalidGrids(t *testing.T) {
	maximumInt := int(^uint(0) >> 1)
	tests := []Layout{
		{Kind: LayoutKindSingle, Columns: 1},
		{Kind: LayoutKindSpriteSheet, Columns: 4, Rows: 4, CellWidth: 32},
		{Kind: LayoutKindSpriteSheet, Columns: 4, Rows: 4, CellWidth: 32, CellHeight: 32, FrameCount: 17},
		{Kind: LayoutKindTileSheet, Columns: 16, Rows: 9, CellWidth: 32, CellHeight: 32, FrameCount: 1},
		{Kind: LayoutKindSpriteSheet, Columns: maximumInt, Rows: maximumInt, CellWidth: 1, CellHeight: 1, FrameCount: maximumInt},
	}
	for _, layout := range tests {
		if err := layout.Validate(128, 128); err == nil {
			t.Fatalf("Validate(%#v) error = nil, want error", layout)
		}
	}
}

func TestClassificationAndTagVocabulary(t *testing.T) {
	if PrimaryType("sprite-spritesheet").Valid() || PrimaryType("tile-tilemap").Valid() {
		t.Fatal("layout vocabulary leaked into primary types")
	}
	for _, tag := range []string{"not-character", "not-terrain", "non-pixel-art"} {
		if err := ValidateTag(tag); err == nil {
			t.Fatalf("ValidateTag(%q) error = nil, want absence-only rejection", tag)
		}
	}
	if err := ValidateTag("side-view"); err != nil {
		t.Fatalf("ValidateTag(side-view) error = %v", err)
	}
}

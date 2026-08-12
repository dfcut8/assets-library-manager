// Package catalog defines validated catalog metadata independent of storage and transport adapters.
package catalog

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// PrimaryType is the content-focused classification of an asset.
type PrimaryType string

// Controlled primary types describe visual content rather than layout.
const (
	PrimaryTypeCharacter       PrimaryType = "character"
	PrimaryTypeCreature        PrimaryType = "creature"
	PrimaryTypeTerrain         PrimaryType = "terrain"
	PrimaryTypeEnvironment     PrimaryType = "environment"
	PrimaryTypeProp            PrimaryType = "prop"
	PrimaryTypeBuilding        PrimaryType = "building"
	PrimaryTypeVehicle         PrimaryType = "vehicle"
	PrimaryTypeWeaponTool      PrimaryType = "weapon-tool"
	PrimaryTypeCollectible     PrimaryType = "collectible"
	PrimaryTypeUI              PrimaryType = "ui"
	PrimaryTypeIcon            PrimaryType = "icon"
	PrimaryTypeBackground      PrimaryType = "background"
	PrimaryTypeTextureMaterial PrimaryType = "texture-material"
	PrimaryTypeVFX             PrimaryType = "vfx"
	PrimaryTypeDecal           PrimaryType = "decal"
	PrimaryTypeOther           PrimaryType = "other"
)

var primaryTypes = map[PrimaryType]struct{}{
	PrimaryTypeCharacter:       {},
	PrimaryTypeCreature:        {},
	PrimaryTypeTerrain:         {},
	PrimaryTypeEnvironment:     {},
	PrimaryTypeProp:            {},
	PrimaryTypeBuilding:        {},
	PrimaryTypeVehicle:         {},
	PrimaryTypeWeaponTool:      {},
	PrimaryTypeCollectible:     {},
	PrimaryTypeUI:              {},
	PrimaryTypeIcon:            {},
	PrimaryTypeBackground:      {},
	PrimaryTypeTextureMaterial: {},
	PrimaryTypeVFX:             {},
	PrimaryTypeDecal:           {},
	PrimaryTypeOther:           {},
}

// Valid reports whether the primary type is part of the controlled v1 taxonomy.
func (t PrimaryType) Valid() bool {
	_, ok := primaryTypes[t]

	return ok
}

// LayoutKind describes how visual cells are arranged independently of their content.
type LayoutKind string

// Controlled layout kinds describe visual cell arrangement rather than content.
const (
	LayoutKindSingle      LayoutKind = "single"
	LayoutKindSpriteSheet LayoutKind = "sprite-sheet"
	LayoutKindTileSheet   LayoutKind = "tile-sheet"
)

// Layout contains optional AI-estimated sheet structure. Zero numeric fields mean unknown.
type Layout struct {
	Kind           LayoutKind `json:"kind"`
	Columns        int        `json:"columns"`
	Rows           int        `json:"rows"`
	CellWidth      int        `json:"cell_width"`
	CellHeight     int        `json:"cell_height"`
	FrameCount     int        `json:"frame_count"`
	AnimationLabel string     `json:"animation_label"`
}

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Validate checks layout consistency against the decoded display dimensions.
func (l Layout) Validate(imageWidth, imageHeight int) error {
	switch l.Kind {
	case LayoutKindSingle:
		if l.hasSheetDetails() {
			return errors.New("catalog: single layout must not contain sheet details")
		}
		return nil
	case LayoutKindSpriteSheet, LayoutKindTileSheet:
	default:
		return fmt.Errorf("catalog: unsupported layout kind %q", l.Kind)
	}

	gridValues := []int{l.Columns, l.Rows, l.CellWidth, l.CellHeight}
	hasGrid := false
	for _, value := range gridValues {
		if value > 0 {
			hasGrid = true
		}
		if value < 0 {
			return errors.New("catalog: sheet grid values must not be negative")
		}
	}
	if hasGrid {
		for _, value := range gridValues {
			if value == 0 {
				return errors.New("catalog: sheet grid fields must be provided together")
			}
		}
		if l.Columns > imageWidth/l.CellWidth || l.Rows > imageHeight/l.CellHeight {
			return errors.New("catalog: sheet grid exceeds image dimensions")
		}
	}

	if l.FrameCount < 0 {
		return errors.New("catalog: frame count must not be negative")
	}
	if l.Kind == LayoutKindTileSheet && l.FrameCount != 0 {
		return errors.New("catalog: tile sheets must not declare animation frames")
	}
	if hasGrid && l.FrameCount > l.Columns*l.Rows {
		return errors.New("catalog: frame count exceeds grid capacity")
	}
	if l.AnimationLabel != "" {
		if l.Kind != LayoutKindSpriteSheet {
			return errors.New("catalog: animation label requires a sprite sheet")
		}
		if len(l.AnimationLabel) > 64 || !slugPattern.MatchString(l.AnimationLabel) {
			return errors.New("catalog: animation label must be lowercase kebab-case")
		}
	}

	return nil
}

func (l Layout) hasSheetDetails() bool {
	return l.Columns != 0 || l.Rows != 0 || l.CellWidth != 0 || l.CellHeight != 0 ||
		l.FrameCount != 0 || l.AnimationLabel != ""
}

// ValidateTag rejects malformed and absence-only tags that do not improve retrieval.
func ValidateTag(tag string) error {
	if tag == "" || len(tag) > 64 || !slugPattern.MatchString(tag) {
		return errors.New("catalog: tag must be lowercase kebab-case and at most 64 characters")
	}
	if strings.HasPrefix(tag, "not-") || tag == "non-pixel-art" {
		return errors.New("catalog: absence-only tags are not allowed")
	}

	return nil
}

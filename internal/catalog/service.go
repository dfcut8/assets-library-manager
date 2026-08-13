package catalog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// CatalogReader loads ready catalog result pages and details. The SDD names
// this consumer-owned contract explicitly, so the package stutter is intentional.
//
//nolint:revive // CatalogReader is the normative SDD contract name.
type CatalogReader interface {
	Search(context.Context, AssetQuery) (Page[AssetSummary], error)
	Get(context.Context, AssetID) (AssetDetail, error)
}

// MetadataUpdater transactionally replaces editable semantic metadata.
type MetadataUpdater interface {
	UpdateSemanticMetadata(context.Context, AssetID, MetadataEdit) (int, error)
}

// ThumbnailReader loads bounded ready thumbnail bytes.
type ThumbnailReader interface {
	GetThumbnail(context.Context, AssetID) (Thumbnail, error)
}

// OriginalReader resolves a ready managed original without opening it.
type OriginalReader interface {
	GetOriginal(context.Context, AssetID) (Original, error)
}

// ProcessingReader is the handler-facing startup processing snapshot contract.
type ProcessingReader[Snapshot any] interface {
	Snapshot() Snapshot
}

// Service is the handler-facing catalog use case.
type Service struct {
	reader          CatalogReader
	updater         MetadataUpdater
	thumbnailReader ThumbnailReader
	originalReader  OriginalReader
}

// NewService constructs the catalog use case from consumer-owned ports.
func NewService(
	reader CatalogReader,
	updater MetadataUpdater,
	thumbnailReader ThumbnailReader,
	originalReader OriginalReader,
) (*Service, error) {
	if reader == nil || updater == nil || thumbnailReader == nil || originalReader == nil {
		return nil, errors.New("creating catalog service: dependencies are incomplete")
	}

	return &Service{
		reader: reader, updater: updater,
		thumbnailReader: thumbnailReader, originalReader: originalReader,
	}, nil
}

// Search validates and executes a deterministic ready-asset query.
func (service *Service) Search(
	ctx context.Context,
	query AssetQuery,
) (Page[AssetSummary], error) {
	query, err := NormalizeAssetQuery(query)
	if err != nil {
		return Page[AssetSummary]{}, err
	}
	page, err := service.reader.Search(ctx, query)
	if err != nil {
		return Page[AssetSummary]{}, err
	}
	if page.Items == nil {
		page.Items = make([]AssetSummary, 0)
	}

	return page, nil
}

// Get loads one ready asset with technical metadata and latest AI provenance.
func (service *Service) Get(ctx context.Context, id AssetID) (AssetDetail, error) {
	if _, err := ParseAssetID(id.String()); err != nil {
		return AssetDetail{}, err
	}

	return service.reader.Get(ctx, id)
}

// GetThumbnail loads one bounded PNG thumbnail for a ready asset.
func (service *Service) GetThumbnail(ctx context.Context, id AssetID) (Thumbnail, error) {
	if _, err := ParseAssetID(id.String()); err != nil {
		return Thumbnail{}, err
	}
	thumbnail, err := service.thumbnailReader.GetThumbnail(ctx, id)
	if err != nil {
		return Thumbnail{}, err
	}
	thumbnail.Data = append([]byte(nil), thumbnail.Data...)

	return thumbnail, nil
}

// GetOriginal resolves the database-owned ready original reference.
func (service *Service) GetOriginal(ctx context.Context, id AssetID) (Original, error) {
	if _, err := ParseAssetID(id.String()); err != nil {
		return Original{}, err
	}

	return service.originalReader.GetOriginal(ctx, id)
}

// UpdateSemanticMetadata validates and transactionally replaces current metadata.
func (service *Service) UpdateSemanticMetadata(
	ctx context.Context,
	id AssetID,
	edit MetadataEdit,
) (AssetDetail, error) {
	if _, err := ParseAssetID(id.String()); err != nil {
		return AssetDetail{}, err
	}
	edit, err := NormalizeMetadataEdit(edit)
	if err != nil {
		return AssetDetail{}, err
	}
	if _, err := service.updater.UpdateSemanticMetadata(ctx, id, edit); err != nil {
		return AssetDetail{}, err
	}

	return service.reader.Get(ctx, id)
}

// NormalizeMetadataEdit trims strings, validates semantic fields, and rejects duplicate tags.
func NormalizeMetadataEdit(edit MetadataEdit) (MetadataEdit, error) {
	fields := make(map[string]string)
	edit.Title = strings.TrimSpace(edit.Title)
	edit.Description = strings.TrimSpace(edit.Description)
	edit.Style = strings.TrimSpace(edit.Style)
	if edit.Version < 1 {
		fields["version"] = "version must be positive"
	}
	validateRuneField(fields, "title", edit.Title, 1, 160)
	validateRuneField(fields, "description", edit.Description, 1, 2000)
	validateRuneField(fields, "style", edit.Style, 1, 80)
	if !edit.PrimaryType.Valid() {
		fields["primary_type"] = "primary type is unsupported"
	}
	maximumDimension := int(^uint(0) >> 1)
	if err := edit.Layout.Validate(maximumDimension, maximumDimension); err != nil {
		fields["layout"] = err.Error()
	}
	if len(edit.Tags) > 64 {
		fields["tags"] = "no more than 64 tags are allowed"
	}
	seen := make(map[string]struct{}, len(edit.Tags))
	for index := range edit.Tags {
		tag := &edit.Tags[index]
		tag.Facet = strings.TrimSpace(tag.Facet)
		tag.Slug = strings.TrimSpace(tag.Slug)
		tag.Label = strings.TrimSpace(tag.Label)
		tag.Origin = "user"
		if err := validateFacet(tag.Facet); err != nil {
			fields["tags"] = "tag facet is invalid"
		}
		if err := ValidateTag(tag.Slug); err != nil {
			fields["tags"] = "tag slug is invalid"
		}
		if !validTagLabel(tag.Label) {
			fields["tags"] = "tag label must contain 1 to 128 printable characters"
		}
		key := tag.Facet + "\x00" + tag.Slug
		if _, exists := seen[key]; exists {
			fields["tags"] = "duplicate facet and slug values are not allowed"
		}
		seen[key] = struct{}{}
	}
	if len(fields) != 0 {
		return MetadataEdit{}, &ValidationError{Fields: fields}
	}

	return edit, nil
}

// FlattenTags returns stable searchable facet, slug, and label text.
func FlattenTags(tags []Tag) string {
	values := make([]string, 0, len(tags)*3)
	for _, tag := range tags {
		values = append(values, tag.Facet, tag.Slug, tag.Label)
	}
	slices.Sort(values)

	return strings.Join(values, " ")
}

func validateRuneField(fields map[string]string, field, value string, minimum, maximum int) {
	if !utf8.ValidString(value) {
		fields[field] = "field must contain valid UTF-8"
		return
	}
	length := utf8.RuneCountInString(value)
	if length < minimum || length > maximum {
		fields[field] = fmt.Sprintf("field must contain %d to %d characters", minimum, maximum)
	}
}

func validTagLabel(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	length := utf8.RuneCountInString(value)
	if length < 1 || length > 128 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}

	return true
}

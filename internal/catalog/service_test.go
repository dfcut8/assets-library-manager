package catalog

import (
	"context"
	"errors"
	"testing"
)

func TestNormalizeMetadataEditTrimsAndMarksTagsAsUserAuthored(t *testing.T) {
	t.Parallel()
	edit, err := NormalizeMetadataEdit(MetadataEdit{
		Version: 1, Title: " Asset ", Description: " Description ",
		PrimaryType: PrimaryTypeProp, Style: " Painted ",
		Layout: Layout{Kind: LayoutKindSingle},
		Tags:   []Tag{{Facet: "subject", Slug: "wooden-crate", Label: " Wooden crate ", Origin: "ai"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if edit.Title != "Asset" || edit.Description != "Description" || edit.Style != "Painted" {
		t.Fatalf("trimmed edit = %+v", edit)
	}
	if edit.Tags[0].Origin != "user" || edit.Tags[0].Label != "Wooden crate" {
		t.Fatalf("normalized tag = %+v", edit.Tags[0])
	}
}

func TestNormalizeMetadataEditReturnsFieldErrors(t *testing.T) {
	t.Parallel()
	_, err := NormalizeMetadataEdit(MetadataEdit{
		Version: 0, PrimaryType: PrimaryType("invalid"),
		Layout: Layout{Kind: LayoutKindSingle},
		Tags: []Tag{
			{Facet: "subject", Slug: "hero", Label: "Hero"},
			{Facet: "subject", Slug: "hero", Label: "Hero"},
		},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("NormalizeMetadataEdit() error = %v", err)
	}
	validation, ok := errors.AsType[*ValidationError](err)
	if !ok || validation.Fields["title"] == "" || validation.Fields["tags"] == "" {
		t.Fatalf("validation fields = %#v", validation)
	}
}

func TestServiceReturnsDefensiveThumbnailBytes(t *testing.T) {
	t.Parallel()
	repository := &serviceRepository{thumbnail: Thumbnail{MIMEType: "image/png", Data: []byte("png")}}
	service, err := NewService(repository, repository, repository, repository)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ParseAssetID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	thumbnail, err := service.GetThumbnail(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	thumbnail.Data[0] = 'X'
	if string(repository.thumbnail.Data) != "png" {
		t.Fatal("service exposed repository thumbnail storage")
	}
}

type serviceRepository struct {
	thumbnail Thumbnail
}

func (*serviceRepository) Search(context.Context, AssetQuery) (Page[AssetSummary], error) {
	return Page[AssetSummary]{}, nil
}

func (*serviceRepository) Get(context.Context, AssetID) (AssetDetail, error) {
	return AssetDetail{}, nil
}

func (*serviceRepository) UpdateSemanticMetadata(context.Context, AssetID, MetadataEdit) (int, error) {
	return 2, nil
}

func (repository *serviceRepository) GetThumbnail(context.Context, AssetID) (Thumbnail, error) {
	return repository.thumbnail, nil
}

func (*serviceRepository) GetOriginal(context.Context, AssetID) (Original, error) {
	return Original{}, nil
}

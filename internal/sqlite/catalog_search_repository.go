package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dfcut8/assets-library-manager/internal/catalog"
)

var (
	_ catalog.CatalogReader   = (*Database)(nil)
	_ catalog.MetadataUpdater = (*Database)(nil)
	_ catalog.ThumbnailReader = (*Database)(nil)
	_ catalog.OriginalReader  = (*Database)(nil)
)

// Search returns one deterministic page of ready, integrity-valid catalog assets.
func (d *Database) Search(
	ctx context.Context,
	query catalog.AssetQuery,
) (catalog.Page[catalog.AssetSummary], error) {
	query, err := catalog.NormalizeAssetQuery(query)
	if err != nil {
		return catalog.Page[catalog.AssetSummary]{}, err
	}
	from, where, args, err := buildCatalogFilter(query)
	if err != nil {
		return catalog.Page[catalog.AssetSummary]{}, err
	}
	var total int
	if err := d.db.QueryRowContext(
		ctx,
		"SELECT count(*) "+from+" WHERE "+strings.Join(where, " AND "),
		args...,
	).Scan(&total); err != nil {
		return catalog.Page[catalog.AssetSummary]{}, fmt.Errorf("counting catalog assets: %w", err)
	}

	orderBy := catalogSortExpression(query.Sort, query.Q != "")
	selectArgs := append([]any(nil), args...)
	selectArgs = append(selectArgs, query.PageSize, (query.Page-1)*query.PageSize)
	rows, err := d.db.QueryContext(ctx, `
		SELECT a.id, coalesce(a.title, ''), coalesce(a.primary_type, ''),
			coalesce(a.style, ''), a.display_width, a.display_height, a.format,
			coalesce(a.pixel_art, 0), a.has_transparency, a.encoded_animated,
			a.imported_at, a.updated_at, a.version
	`+from+` WHERE `+strings.Join(where, " AND ")+`
		ORDER BY `+orderBy+` LIMIT ? OFFSET ?
	`, selectArgs...)
	if err != nil {
		return catalog.Page[catalog.AssetSummary]{}, fmt.Errorf("querying catalog assets: %w", err)
	}
	items, scanErr := scanAssetSummaries(rows)
	if scanErr != nil {
		return catalog.Page[catalog.AssetSummary]{}, scanErr
	}
	if err := d.loadSummaryTags(ctx, items); err != nil {
		return catalog.Page[catalog.AssetSummary]{}, err
	}
	totalPages := 0
	if total > 0 {
		totalPages = (total + query.PageSize - 1) / query.PageSize
	}

	return catalog.Page[catalog.AssetSummary]{
		Items: items, Page: query.Page, PageSize: query.PageSize,
		TotalItems: total, TotalPages: totalPages,
	}, nil
}

func buildCatalogFilter(
	query catalog.AssetQuery,
) (string, []string, []any, error) {
	from := "FROM assets AS a"
	where := []string{"a.state = 'ready'"}
	args := make([]any, 0, 32)
	if query.Q != "" {
		fts, err := catalog.FTSLiteralQuery(query.Q)
		if err != nil {
			return "", nil, nil, err
		}
		from += " JOIN asset_search ON asset_search.rowid = a.rowid"
		where = append(where, "asset_search MATCH ?")
		args = append(args, fts)
	}
	if len(query.Types) > 0 {
		values := make([]any, 0, len(query.Types))
		for _, value := range query.Types {
			values = append(values, string(value))
		}
		where, args = appendInFilter(where, args, "a.primary_type", values)
	}
	if len(query.Styles) > 0 {
		parts := make([]string, 0, len(query.Styles))
		for _, value := range query.Styles {
			parts = append(parts, "instr(replace(lower(a.style), '-', ' '), replace(lower(?), '-', ' ')) > 0")
			args = append(args, value)
		}
		where = append(where, "("+strings.Join(parts, " OR ")+")")
	}
	if len(query.Orientations) > 0 {
		where, args = appendStringInFilter(where, args, "a.orientation_class", query.Orientations)
	}
	if len(query.Formats) > 0 {
		where, args = appendStringInFilter(where, args, "a.format", query.Formats)
	}
	if len(query.Tags) > 0 {
		parts := make([]string, 0, len(query.Tags))
		for _, tag := range query.Tags {
			parts = append(parts, "(t.facet = ? AND t.slug = ?)")
			args = append(args, tag.Facet, tag.Slug)
		}
		where = append(where, `EXISTS (
			SELECT 1 FROM asset_tags AS at
			JOIN tags AS t ON t.id = at.tag_id
			WHERE at.asset_id = a.id AND (`+strings.Join(parts, " OR ")+`)
		)`)
	}
	where, args = appendBooleanFilter(where, args, "a.pixel_art", query.PixelArt)
	where, args = appendBooleanFilter(where, args, "a.has_transparency", query.Transparency)
	where, args = appendBooleanFilter(where, args, "a.encoded_animated", query.Animated)
	where, args = appendLowerBound(where, args, "a.display_width", query.MinWidth)
	where, args = appendUpperBound(where, args, "a.display_width", query.MaxWidth)
	where, args = appendLowerBound(where, args, "a.display_height", query.MinHeight)
	where, args = appendUpperBound(where, args, "a.display_height", query.MaxHeight)
	if query.ImportedFrom != nil {
		where = append(where, "a.imported_at >= ?")
		args = append(args, formatTime(*query.ImportedFrom))
	}
	if query.ImportedTo != nil {
		where = append(where, "a.imported_at <= ?")
		args = append(args, formatTime(*query.ImportedTo))
	}

	return from, where, args, nil
}

func appendInFilter(
	where []string,
	args []any,
	column string,
	values []any,
) ([]string, []any) {
	where = append(where, column+" IN ("+placeholders(len(values))+")")
	args = append(args, values...)

	return where, args
}

func appendStringInFilter(
	where []string,
	args []any,
	column string,
	values []string,
) ([]string, []any) {
	converted := make([]any, 0, len(values))
	for _, value := range values {
		converted = append(converted, value)
	}

	return appendInFilter(where, args, column, converted)
}

func appendBooleanFilter(
	where []string,
	args []any,
	column string,
	value *bool,
) ([]string, []any) {
	if value == nil {
		return where, args
	}
	where = append(where, column+" = ?")
	args = append(args, boolInt(*value))

	return where, args
}

func appendLowerBound(where []string, args []any, column string, value int) ([]string, []any) {
	if value > 0 {
		where = append(where, column+" >= ?")
		args = append(args, value)
	}

	return where, args
}

func appendUpperBound(where []string, args []any, column string, value int) ([]string, []any) {
	if value > 0 {
		where = append(where, column+" <= ?")
		args = append(args, value)
	}

	return where, args
}

func placeholders(count int) string {
	if count < 1 {
		return ""
	}

	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func catalogSortExpression(sort catalog.Sort, hasQuery bool) string {
	switch sort {
	case catalog.SortRelevance:
		if hasQuery {
			return "bm25(asset_search) ASC, a.imported_at DESC, a.id ASC"
		}
		return "a.imported_at DESC, a.id ASC"
	case catalog.SortOldest:
		return "a.imported_at ASC, a.id ASC"
	case catalog.SortTitle:
		return "a.title COLLATE NOCASE ASC, a.id ASC"
	case catalog.SortWidth:
		return "a.display_width DESC, a.id ASC"
	case catalog.SortHeight:
		return "a.display_height DESC, a.id ASC"
	case catalog.SortFileSize:
		return "a.file_size_bytes DESC, a.id ASC"
	case catalog.SortNewest:
		fallthrough
	default:
		return "a.imported_at DESC, a.id ASC"
	}
}

type summaryRows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}

func scanAssetSummaries(rows summaryRows) (_ []catalog.AssetSummary, returnErr error) {
	defer func() {
		if err := rows.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing catalog asset rows: %w", err))
		}
	}()
	items := make([]catalog.AssetSummary, 0)
	for rows.Next() {
		var item catalog.AssetSummary
		var idText, primaryTypeText, importedAtText, updatedAtText string
		if err := rows.Scan(
			&idText, &item.Title, &primaryTypeText, &item.Style,
			&item.DisplayWidth, &item.DisplayHeight, &item.Format,
			&item.PixelArt, &item.HasTransparency, &item.EncodedAnimated,
			&importedAtText, &updatedAtText, &item.Version,
		); err != nil {
			return nil, fmt.Errorf("scanning catalog asset: %w", err)
		}
		id, err := catalog.ParseAssetID(idText)
		if err != nil {
			return nil, fmt.Errorf("decoding catalog asset identifier: %w", err)
		}
		item.ID = id
		item.PrimaryType = catalog.PrimaryType(primaryTypeText)
		if !item.PrimaryType.Valid() {
			return nil, errors.New("decoding catalog asset: invalid primary type")
		}
		item.ImportedAt, err = parseTime(importedAtText)
		if err != nil {
			return nil, err
		}
		item.UpdatedAt, err = parseTime(updatedAtText)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating catalog assets: %w", err)
	}

	return items, nil
}

func (d *Database) loadSummaryTags(ctx context.Context, items []catalog.AssetSummary) (returnErr error) {
	if len(items) == 0 {
		return nil
	}
	args := make([]any, 0, len(items))
	indexes := make(map[catalog.AssetID]int, len(items))
	for index := range items {
		args = append(args, items[index].ID.String())
		indexes[items[index].ID] = index
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT at.asset_id, t.facet, t.slug, t.label, at.origin
		FROM asset_tags AS at
		JOIN tags AS t ON t.id = at.tag_id
		WHERE at.asset_id IN (`+placeholders(len(items))+`)
		ORDER BY at.asset_id, t.facet, t.slug
	`, args...)
	if err != nil {
		return fmt.Errorf("querying catalog tags: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing catalog tag rows: %w", err))
		}
	}()
	for rows.Next() {
		var idText string
		var tag catalog.Tag
		if err := rows.Scan(&idText, &tag.Facet, &tag.Slug, &tag.Label, &tag.Origin); err != nil {
			return fmt.Errorf("scanning catalog tag: %w", err)
		}
		id, err := catalog.ParseAssetID(idText)
		if err != nil {
			return err
		}
		index, exists := indexes[id]
		if !exists {
			return errors.New("scanning catalog tag: asset was not requested")
		}
		items[index].Tags = append(items[index].Tags, tag)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating catalog tags: %w", err)
	}

	return nil
}

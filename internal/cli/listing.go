package cli

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/Barney241/pocketbase-cli/internal/output"
	"github.com/Barney241/pocketbase-cli/internal/pb"
)

const (
	defaultPerPage  = 20
	bulkPerPage     = 500
	defaultAllLimit = 5000
)

type listFlags struct {
	filter     string
	parameters []string
	sort       string
	fields     string
	expand     string
	page       int
	perPage    int
	all        bool
	limit      int
	skipTotal  bool
}

type listing struct {
	Page       int              `json:"page"`
	PerPage    int              `json:"perPage"`
	TotalItems int              `json:"totalItems"`
	TotalPages int              `json:"totalPages"`
	Items      []map[string]any `json:"items"`
	cutAtLimit bool
}

func (f *listFlags) register(cmd *cobra.Command, defaultSort string) {
	flags := cmd.Flags()
	flags.StringVarP(&f.filter, "filter", "f", "", "PocketBase filter, e.g. \"status = 'open' && created > '2026-01-01'\"")
	flags.StringArrayVar(&f.parameters, "param", nil, "value for a {:name} placeholder in --filter, quoted for you (name=text or name:=json)")
	flags.StringVarP(&f.sort, "sort", "s", defaultSort, "sort fields, - for descending, e.g. -created,title")
	flags.StringVar(&f.fields, "fields", "", "only return these fields, e.g. id,title,expand.author.name")
	flags.StringVar(&f.expand, "expand", "", "relations to expand, e.g. author,comments_via_post")
	flags.IntVar(&f.page, "page", 1, "page number")
	flags.IntVarP(&f.perPage, "per-page", "n", defaultPerPage, "rows per page")
	flags.BoolVar(&f.all, "all", false, "fetch every page, up to --limit rows")
	flags.IntVar(&f.limit, "limit", defaultAllLimit, "row cap: for --all (0 for no cap), and for a single page when lower than --per-page")
	flags.BoolVar(&f.skipTotal, "skip-total", false, "skip the total count for a faster query")
}

func (f *listFlags) rowsPerPage() int {
	if f.limit > 0 && f.limit < f.perPage {
		return f.limit
	}
	return f.perPage
}

func (f *listFlags) query() (url.Values, error) {
	filter, err := bindFilter(f.filter, f.parameters)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	setIfPresent(query, "filter", filter)
	setIfPresent(query, "sort", f.sort)
	setIfPresent(query, "fields", f.fields)
	setIfPresent(query, "expand", f.expand)
	query.Set("page", strconv.Itoa(f.page))
	query.Set("perPage", strconv.Itoa(f.rowsPerPage()))
	if f.skipTotal {
		query.Set("skipTotal", "1")
	}
	return query, nil
}

func setIfPresent(query url.Values, key, value string) {
	if value != "" {
		query.Set(key, value)
	}
}

func (a *app) collect(ctx context.Context, client *pb.Client, path string, flags *listFlags) (*listing, error) {
	query, err := flags.query()
	if err != nil {
		return nil, err
	}
	if !flags.all {
		page := &listing{}
		return page, client.JSON(ctx, http.MethodGet, path, query, nil, page)
	}
	query.Set("perPage", strconv.Itoa(bulkPerPage))
	query.Set("skipTotal", "1")
	return collectEveryPage(ctx, client, path, query, flags.limit)
}

func collectEveryPage(ctx context.Context, client *pb.Client, path string, query url.Values, limit int) (*listing, error) {
	everything := &listing{Page: 1, TotalPages: 1, Items: []map[string]any{}}
	for pageNumber := 1; ; pageNumber++ {
		query.Set("page", strconv.Itoa(pageNumber))
		page := &listing{}
		if err := client.JSON(ctx, http.MethodGet, path, query, nil, page); err != nil {
			return nil, err
		}
		everything.Items = append(everything.Items, page.Items...)
		if limit > 0 && len(everything.Items) >= limit {
			everything.cutAtLimit = len(everything.Items) > limit || len(page.Items) == bulkPerPage
			everything.Items = everything.Items[:limit]
			break
		}
		if len(page.Items) < bulkPerPage {
			break
		}
	}
	everything.PerPage = len(everything.Items)
	everything.TotalItems = len(everything.Items)
	return everything, nil
}

func fetchEverything(ctx context.Context, client *pb.Client, path string, query url.Values) ([]map[string]any, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set("perPage", strconv.Itoa(bulkPerPage))
	query.Set("skipTotal", "1")
	everything, err := collectEveryPage(ctx, client, path, query, 0)
	if err != nil {
		return nil, err
	}
	return everything.Items, nil
}

func (a *app) printListing(result *listing, columns []string, displayRows []map[string]any) error {
	if a.printer.Format == output.JSONL {
		displayRows = result.Items
	}
	if err := a.printer.List(result, columns, displayRows); err != nil {
		return err
	}
	a.describePosition(result)
	return nil
}

func (a *app) describePosition(result *listing) {
	shown := len(result.Items)
	switch {
	case result.cutAtLimit:
		a.printer.Note("stopped at --limit %d; more rows exist", shown)
	case shown == 0 && result.Page <= 1:
		a.printer.Note("no rows")
	case result.TotalItems < 0 && shown == result.PerPage:
		a.printer.Note("page %d, %d rows; more may follow: --page %d", result.Page, shown, result.Page+1)
	case result.TotalItems >= 0 && result.Page < result.TotalPages:
		first := (result.Page-1)*result.PerPage + 1
		a.printer.Note("rows %d-%d of %d; next: --page %d", first, first+shown-1, result.TotalItems, result.Page+1)
	case result.TotalItems > shown:
		a.printer.Note("last page of %d rows", result.TotalItems)
	}
}

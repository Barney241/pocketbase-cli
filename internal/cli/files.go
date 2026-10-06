package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Barney241/pocketbase-cli/internal/pb"
)

func (a *app) filesCommand() *cobra.Command {
	files := &cobra.Command{Use: "files", Aliases: []string{"file"}, Short: "List and download record files (upload with `records create/update --file`)"}
	files.AddCommand(a.filesListCommand(), a.filesURLCommand(), a.filesDownloadCommand())
	return files
}

func (a *app) filesListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "list <collection> <record id>",
		Aliases: []string{"ls"},
		Short:   "List the files attached to a record",
		Args:    exactArgs(2, "files list <collection> <record id>"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			collection, record := map[string]any{}, map[string]any{}
			if err := client.JSON(cmd.Context(), http.MethodGet, collectionPath(arguments[0]), nil, nil, &collection); err != nil {
				return err
			}
			if err := client.JSON(cmd.Context(), http.MethodGet, collectionPath(arguments[0], "records", arguments[1]), nil, nil, &record); err != nil {
				return err
			}
			rows := attachedFiles(collection, record)
			if len(rows) == 0 {
				a.printer.Note("no files")
			}
			return a.printer.List(rows, []string{"field", "filename", "protected"}, rows)
		},
	}
}

func attachedFiles(collection, record map[string]any) []map[string]any {
	rows := []map[string]any{}
	for _, field := range fieldsOf(collection) {
		if field["type"] != "file" {
			continue
		}
		protected, _ := field["protected"].(bool)
		for _, filename := range fileNames(record[fmt.Sprint(field["name"])]) {
			rows = append(rows, map[string]any{"field": field["name"], "filename": filename, "protected": protected})
		}
	}
	return rows
}

func fileNames(value any) []string {
	switch typed := value.(type) {
	case string:
		if typed != "" {
			return []string{typed}
		}
	case []any:
		names := []string{}
		for _, entry := range typed {
			if name, isText := entry.(string); isText && name != "" {
				names = append(names, name)
			}
		}
		return names
	}
	return nil
}

func filePath(collection, recordID, filename string) string {
	return "/api/files/" + url.PathEscape(collection) + "/" + url.PathEscape(recordID) + "/" + url.PathEscape(filename)
}

func (a *app) filesURLCommand() *cobra.Command {
	var thumb string
	var withToken bool
	cmd := &cobra.Command{
		Use:   "url <collection> <record id> <filename>",
		Short: "Print the URL of a file",
		Args:  exactArgs(3, "files url <collection> <record id> <filename>"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			query := url.Values{}
			setIfPresent(query, "thumb", thumb)
			if withToken {
				token, err := client.FileToken(cmd.Context())
				if err != nil {
					return err
				}
				query.Set("token", token)
			}
			a.printer.Line("%s", client.URL(filePath(arguments[0], arguments[1], arguments[2]), query))
			return nil
		},
	}
	cmd.Flags().StringVar(&thumb, "thumb", "", "thumbnail size for images, e.g. 100x100")
	cmd.Flags().BoolVar(&withToken, "token", false, "append a short-lived file token so a protected file opens")
	return cmd
}

func (a *app) filesDownloadCommand() *cobra.Command {
	var destination, thumb string
	cmd := &cobra.Command{
		Use:   "download <collection> <record id> <filename>",
		Short: "Save a file to disk and print where it went",
		Long:  "The file is written to --to (default: the file name in the current directory), never to the terminal.\nA protected file is fetched with a short-lived file token automatically.",
		Args:  exactArgs(3, "files download <collection> <record id> <filename>"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			query := url.Values{}
			setIfPresent(query, "thumb", thumb)
			response, err := openFile(cmd.Context(), client, filePath(arguments[0], arguments[1], arguments[2]), query)
			if err != nil {
				return err
			}
			defer response.Body.Close()
			return a.saveDownload(response, firstNonEmpty(destination, filepath.Base(arguments[2])))
		},
	}
	cmd.Flags().StringVar(&destination, "to", "", "where to save it (- writes the bytes to stdout)")
	cmd.Flags().StringVar(&thumb, "thumb", "", "thumbnail size for images, e.g. 100x100")
	return cmd
}

func openFile(ctx context.Context, client *pb.Client, path string, query url.Values) (*http.Response, error) {
	request := pb.Request{Method: http.MethodGet, Path: path, Query: query, Streaming: true}
	response, err := client.Do(ctx, request)
	var apiError *pb.APIError
	if !errors.As(err, &apiError) || (apiError.Status != http.StatusNotFound && apiError.Status != http.StatusForbidden) {
		return response, err
	}
	token, tokenErr := client.FileToken(ctx)
	if tokenErr != nil {
		return nil, err
	}
	request.Query.Set("token", token)
	return client.Do(ctx, request)
}

func (a *app) saveDownload(response *http.Response, destination string) error {
	if destination == "-" {
		_, err := io.Copy(a.printer.Out, response.Body)
		return err
	}
	target, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return usagef("%s already exists; choose another path with --to", destination)
	}
	if err != nil {
		return err
	}
	written, err := io.Copy(target, response.Body)
	if closeErr := target.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(destination)
		return fmt.Errorf("download to %s: %w", destination, err)
	}
	absolute, _ := filepath.Abs(destination)
	a.printer.Line("%s\t%s\t%s", firstNonEmpty(absolute, destination), describeSize(written), response.Header.Get("Content-Type"))
	return nil
}

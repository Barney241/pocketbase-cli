package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func (a *app) readSource(source string) ([]byte, error) {
	switch {
	case source == "-":
		return io.ReadAll(a.stdin)
	case strings.HasPrefix(source, "@"):
		return os.ReadFile(source[1:])
	}
	return []byte(source), nil
}

func (a *app) decodeSource(source string, target any) error {
	raw, err := a.readSource(source)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return usagef("--data is not valid JSON: %v", err)
	}
	return nil
}

func (a *app) objectFrom(data string, assignments []string) (map[string]any, error) {
	object := map[string]any{}
	if data != "" {
		if err := a.decodeSource(data, &object); err != nil {
			return nil, err
		}
	}
	for _, assignment := range assignments {
		key, value, err := parseAssignment(assignment)
		if err != nil {
			return nil, err
		}
		assignNested(object, key, value)
	}
	return object, nil
}

func parseAssignment(assignment string) (string, any, error) {
	key, raw, found := strings.Cut(assignment, "=")
	if !found || key == "" || key == ":" {
		return "", nil, usagef("expected key=value or key:=json, got %q", assignment)
	}
	if !strings.HasSuffix(key, ":") {
		return key, raw, nil
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", nil, usagef("%s is not valid JSON: %v", assignment, err)
	}
	return strings.TrimSuffix(key, ":"), value, nil
}

func assignNested(object map[string]any, dottedKey string, value any) {
	parts := strings.Split(dottedKey, ".")
	for _, part := range parts[:len(parts)-1] {
		child, isObject := object[part].(map[string]any)
		if !isObject {
			child = map[string]any{}
			object[part] = child
		}
		object = child
	}
	object[parts[len(parts)-1]] = value
}

var filterPlaceholder = regexp.MustCompile(`\{:([A-Za-z_][A-Za-z0-9_]*)\}`)

func bindFilter(filter string, parameters []string) (string, error) {
	values := map[string]any{}
	for _, parameter := range parameters {
		key, value, err := parseAssignment(parameter)
		if err != nil {
			return "", err
		}
		values[key] = value
	}
	var missing []string
	bound := filterPlaceholder.ReplaceAllStringFunc(filter, func(placeholder string) string {
		name := placeholder[2 : len(placeholder)-1]
		value, known := values[name]
		if !known {
			missing = append(missing, name)
			return placeholder
		}
		return filterLiteral(value)
	})
	if len(missing) > 0 {
		return "", usagef("the filter uses {:%s} but no --param %s=... was given", missing[0], missing[0])
	}
	return bound, nil
}

var filterStringEscaper = strings.NewReplacer(`\`, `\\`, `'`, `\'`)

func filterLiteral(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(typed)
	case json.Number:
		return typed.String()
	case string:
		return "'" + filterStringEscaper.Replace(typed) + "'"
	}
	encoded, _ := json.Marshal(value)
	return "'" + filterStringEscaper.Replace(string(encoded)) + "'"
}

func multipartBody(fields map[string]any, uploads []string) ([]byte, string, error) {
	buffer := &bytes.Buffer{}
	writer := multipart.NewWriter(buffer)
	if len(fields) > 0 {
		payload, err := json.Marshal(fields)
		if err != nil {
			return nil, "", err
		}
		if err := writer.WriteField("@jsonPayload", string(payload)); err != nil {
			return nil, "", err
		}
	}
	for _, upload := range uploads {
		field, location, found := strings.Cut(upload, "=")
		location = strings.TrimPrefix(location, "@")
		if !found || field == "" || location == "" {
			return nil, "", usagef("--file expects field=path, got %q", upload)
		}
		if err := attachFile(writer, field, location); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return buffer.Bytes(), writer.FormDataContentType(), nil
}

func attachFile(writer *multipart.Writer, field, location string) error {
	source, err := os.Open(location)
	if err != nil {
		return err
	}
	defer source.Close()
	part, err := writer.CreateFormFile(field, filepath.Base(location))
	if err != nil {
		return err
	}
	_, err = io.Copy(part, source)
	return err
}

func collectionPath(collection string, suffix ...string) string {
	path := "/api/collections/" + url.PathEscape(collection)
	for _, part := range suffix {
		path += "/" + url.PathEscape(part)
	}
	return path
}

var dayDuration = regexp.MustCompile(`^(\d+)d$`)

func parseAge(value string) (time.Duration, error) {
	if match := dayDuration.FindStringSubmatch(value); match != nil {
		days, _ := strconv.Atoi(match[1])
		return time.Duration(days) * 24 * time.Hour, nil
	}
	age, err := time.ParseDuration(value)
	if err != nil {
		return 0, usagef("%q is not a duration; use forms like 30m, 6h or 2d", value)
	}
	return age, nil
}

func pocketBaseTime(moment time.Time) string {
	return moment.UTC().Format("2006-01-02 15:04:05.000Z")
}

func describeSize(bytesCount int64) string {
	const unit = 1024
	if bytesCount < unit {
		return fmt.Sprintf("%d B", bytesCount)
	}
	divisor, exponent := int64(unit), 0
	for remaining := bytesCount / unit; remaining >= unit; remaining /= unit {
		divisor *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytesCount)/float64(divisor), "KMGT"[exponent])
}

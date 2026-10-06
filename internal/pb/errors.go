package pb

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string
	Data    map[string]any
}

func (e *APIError) Error() string {
	summary := fmt.Sprintf("%d %s %s: %s", e.Status, e.Method, e.Path, e.Message)
	if details := e.FieldErrors(); len(details) > 0 {
		summary += " [" + strings.Join(details, "; ") + "]"
	}
	return summary
}

func (e *APIError) FieldErrors() []string {
	details := []string{}
	collectFieldErrors("", e.Data, &details)
	sort.Strings(details)
	return details
}

func collectFieldErrors(prefix string, node map[string]any, details *[]string) {
	message, hasMessage := node["message"].(string)
	_, hasCode := node["code"].(string)
	if hasMessage && hasCode && prefix != "" {
		*details = append(*details, prefix+": "+message)
		return
	}
	for key, value := range node {
		child, isObject := value.(map[string]any)
		if !isObject {
			continue
		}
		childPrefix := key
		if prefix != "" {
			childPrefix = prefix + "." + key
		}
		collectFieldErrors(childPrefix, child, details)
	}
}

func apiErrorFrom(method, path string, response *http.Response) *APIError {
	apiError := &APIError{Method: method, Path: path, Status: response.StatusCode}
	raw, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBodyBytes))
	var payload struct {
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && payload.Message != "" {
		apiError.Message = payload.Message
		apiError.Data = payload.Data
		return apiError
	}
	apiError.Message = strings.TrimSpace(string(raw))
	if apiError.Message == "" {
		apiError.Message = http.StatusText(response.StatusCode)
	}
	return apiError
}

package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barney241/pocketbase-cli/internal/guard"
	"github.com/Barney241/pocketbase-cli/internal/pb"
)

type authAction struct {
	name     string
	endpoint string
	summary  string
	keys     []string
}

var authActions = []authAction{
	{"login", "auth-with-password", "Log in with a password and print the token and record", []string{"identity", "password"}},
	{"refresh", "auth-refresh", "Exchange the profile's token for a fresh one", nil},
	{"request-otp", "request-otp", "Email a one-time password", []string{"email"}},
	{"with-otp", "auth-with-otp", "Log in with a one-time password", []string{"otpId", "password"}},
	{"request-verification", "request-verification", "Email a verification link", []string{"email"}},
	{"confirm-verification", "confirm-verification", "Confirm an email verification token", []string{"token"}},
	{"request-password-reset", "request-password-reset", "Email a password reset link", []string{"email"}},
	{"confirm-password-reset", "confirm-password-reset", "Set a new password with a reset token", []string{"token", "password", "passwordConfirm"}},
	{"request-email-change", "request-email-change", "Email an email-change link (run with --as <collection>/<id>)", []string{"newEmail"}},
	{"confirm-email-change", "confirm-email-change", "Confirm an email-change token", []string{"token", "password"}},
}

func (a *app) authCommand() *cobra.Command {
	auth := &cobra.Command{Use: "auth", Short: "Auth collection flows: methods, login, OTP, verification, resets, impersonation"}
	auth.AddCommand(a.authMethodsCommand(), a.authImpersonateCommand())
	for _, action := range authActions {
		auth.AddCommand(a.authActionCommand(action))
	}
	return auth
}

func (a *app) authMethodsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "methods <collection>",
		Short: "Show which login methods an auth collection allows",
		Args:  exactArgs(1, "auth methods <collection>"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			return a.getAndPrint(cmd.Context(), collectionPath(arguments[0], "auth-methods"), nil)
		},
	}
}

func (a *app) authActionCommand(action authAction) *cobra.Command {
	usage := action.name + " <collection>"
	for _, key := range action.keys {
		usage += " " + key + "=..."
	}
	return &cobra.Command{
		Use:   usage,
		Short: action.summary,
		Args:  minimumArgs(1, "auth "+usage),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			body, err := a.objectFrom("", arguments[1:])
			if err != nil {
				return err
			}
			for _, key := range action.keys {
				if _, given := body[key]; !given {
					return usagef("usage: pbctl auth %s", usage)
				}
			}
			path := collectionPath(arguments[0], action.endpoint)
			if guard.Classify(http.MethodPost, path) == guard.Write {
				if err := a.confirm(action.name+" in "+arguments[0], false); err != nil {
					return err
				}
			}
			return a.postAndPrint(cmd, path, body)
		},
	}
}

func (a *app) authImpersonateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "impersonate <collection> <id> [duration:=3600]",
		Short: "Print a token that acts as another auth record (superuser only)",
		Long:  "Refused in read-only mode because the token it prints can write.\nTo read as another user without ever seeing a token, add --as <collection>/<id> to any command.",
		Args:  minimumArgs(2, "auth impersonate <collection> <id> [duration:=3600]"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			body, err := a.objectFrom("", arguments[2:])
			if err != nil {
				return err
			}
			if err := a.confirm("issue a token for "+arguments[0]+"/"+arguments[1], false); err != nil {
				return err
			}
			return a.postAndPrint(cmd, collectionPath(arguments[0], "impersonate", arguments[1]), body)
		},
	}
}

func (a *app) postAndPrint(cmd *cobra.Command, path string, body map[string]any) error {
	client, err := a.pb()
	if err != nil {
		return err
	}
	var payload any
	if err := client.JSON(cmd.Context(), http.MethodPost, path, nil, body, &payload); err != nil {
		return err
	}
	if payload == nil {
		a.printer.Line("ok")
		return nil
	}
	return a.printer.Value(payload)
}

func (a *app) apiCommand() *cobra.Command {
	var data string
	var queryPairs []string
	var anonymous bool
	cmd := &cobra.Command{
		Use:     "api <method> <path> [key=value | key:=json ...]",
		Short:   "Call any endpoint, including your app's custom routes",
		Long:    "Sends the request with the profile's auth and prints the response body.\nRead-only mode applies: only GET, HEAD, OPTIONS and a short list of harmless POSTs go out.",
		Example: "  pbctl api GET /api/health\n  pbctl api GET /api/collections/posts/records --query perPage=1 --query 'filter=views > 10'\n  pbctl api POST /api/my-route name=demo count:=3",
		Args:    minimumArgs(2, "api <method> <path> [key=value ...]"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			method, path := strings.ToUpper(arguments[0]), arguments[1]
			request, err := a.apiRequest(method, path, data, queryPairs, arguments[2:])
			if err != nil {
				return err
			}
			request.Anonymous = anonymous
			if guard.Classify(method, request.Path) == guard.Write {
				if err := a.confirm(method+" "+request.Path, method == http.MethodDelete); err != nil {
					return err
				}
			}
			client, err := a.pb()
			if err != nil {
				return err
			}
			response, err := client.Do(cmd.Context(), request)
			if err != nil {
				return err
			}
			defer response.Body.Close()
			return a.printBody(response)
		},
	}
	cmd.Flags().StringVarP(&data, "data", "d", "", "JSON body, @file or - for stdin")
	cmd.Flags().StringArrayVar(&queryPairs, "query", nil, "query parameter key=value (repeatable)")
	cmd.Flags().BoolVar(&anonymous, "anonymous", false, "send no auth, to see what a guest gets")
	return cmd
}

func (a *app) apiRequest(method, target, data string, queryPairs, assignments []string) (pb.Request, error) {
	parsed, err := url.Parse(target)
	if err != nil || !strings.HasPrefix(target, "/") {
		return pb.Request{}, usagef("the path must start with /, e.g. /api/health; got %q", target)
	}
	request := pb.Request{Method: method, Path: parsed.Path, Query: parsed.Query()}
	for _, pair := range queryPairs {
		key, value, found := strings.Cut(pair, "=")
		if !found {
			return pb.Request{}, usagef("--query expects key=value, got %q", pair)
		}
		request.Query.Add(key, value)
	}
	if data == "" && len(assignments) == 0 {
		return request, nil
	}
	body, err := a.objectFrom(data, assignments)
	if err != nil {
		return pb.Request{}, err
	}
	request.Body, err = json.Marshal(body)
	request.ContentType = "application/json"
	return request, err
}

func (a *app) printBody(response *http.Response) error {
	if !strings.Contains(response.Header.Get("Content-Type"), "json") {
		_, err := io.Copy(a.printer.Out, response.Body)
		return err
	}
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var payload any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		_, err = a.printer.Out.Write(raw)
		return err
	}
	return a.printer.Value(payload)
}

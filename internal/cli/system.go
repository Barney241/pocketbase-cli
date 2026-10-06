package cli

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Barney241/pocketbase-cli/internal/config"
	"github.com/Barney241/pocketbase-cli/internal/gateway"
	"github.com/Barney241/pocketbase-cli/internal/pb"
)

//go:embed guide.md
var agentGuide string

//go:embed pbctl_readonly.pb.js
var serverHookTemplate string

const (
	hookEmailsPlaceholder = "__READ_ONLY_SUPERUSER_EMAILS__"
	envGatewayPassword    = "PBCTL_GATEWAY_PASSWORD"
	envGatewayAccessKey   = "PBCTL_GATEWAY_KEY"
)

func (a *app) guideCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "guide",
		Short: "Print the one-page reference: commands, filter syntax, output, exit codes",
		Args:  exactArgs(0, "guide"),
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprint(a.printer.Out, agentGuide)
			return nil
		},
	}
}

func (a *app) statusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the profile in use, who you are, and what enforces read-only",
		Args:  exactArgs(0, "status"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			guardStatus, guardErr := client.GuardStatus(cmd.Context())
			report := map[string]any{
				"profile":     a.profileName,
				"url":         a.profile.URL,
				"health":      a.describeHealth(cmd.Context(), client),
				"identity":    a.describeIdentity(cmd.Context(), client),
				"mode":        a.describeProfileMode(),
				"enforcement": describeEnforcement(a.readOnlySource != "", guardStatus, guardErr),
			}
			for _, key := range []string{"profile", "url", "health", "identity", "mode", "enforcement"} {
				a.printer.Line("%s: %v", key, report[key])
			}
			return nil
		},
	}
}

func (a *app) describeHealth(ctx context.Context, client *pb.Client) string {
	var health struct {
		Message string `json:"message"`
	}
	if err := client.JSON(ctx, http.MethodGet, "/api/health", nil, nil, &health); err != nil {
		return "unreachable: " + err.Error()
	}
	return "ok"
}

func (a *app) describeIdentity(ctx context.Context, client *pb.Client) string {
	if !a.profile.HasCredentials() {
		return "anonymous (a gateway supplies its own credential)"
	}
	token, err := client.Token(ctx)
	if err != nil {
		return "login failed: " + err.Error()
	}
	claims := pb.TokenClaims(token)
	collectionID, recordID := fmt.Sprint(claims["collectionId"]), fmt.Sprint(claims["id"])
	record := map[string]any{}
	if err := client.JSON(ctx, http.MethodGet, collectionPath(collectionID, "records", recordID), url.Values{"fields": {"id,email,collectionName"}}, nil, &record); err != nil {
		return "token rejected: " + err.Error()
	}
	return fmt.Sprintf("%v in %v, token valid until %s", firstNonEmpty(fmt.Sprint(record["email"]), recordID), record["collectionName"], pb.TokenExpiry(token).Format(time.RFC3339))
}

func (a *app) describeProfileMode() string {
	if a.readOnlySource == "" {
		return "writable"
	}
	return "read-only, set by " + a.readOnlySource
}

func describeEnforcement(clientReadOnly bool, guardStatus *pb.GuardStatus, guardErr error) string {
	switch {
	case guardErr != nil:
		return "unknown: " + guardErr.Error()
	case guardStatus != nil && guardStatus.ReadOnly:
		return guardStatus.EnforcedBy + " (writes are refused on the server side, whatever client is used)"
	case clientReadOnly:
		return "this client only (pbctl sends no writes; the credential itself could still write if used elsewhere)"
	}
	return "none"
}

func (a *app) guardCommand() *cobra.Command {
	guardCmd := &cobra.Command{Use: "guard", Short: "Server-side read-only: generate the PocketBase hook that makes a superuser read-only"}
	var emails []string
	hook := &cobra.Command{
		Use:     "hook --superuser <email>",
		Short:   "Print a pb_hooks file that turns the given superusers into read-only accounts",
		Long:    "Save the output as pb_hooks/pbctl_readonly.pb.js on the server. From then on those superusers can read everything\nbut every write, impersonation and backup download they attempt is refused by PocketBase itself.",
		Example: "  pbctl guard hook --superuser agent@example.com > pb_hooks/pbctl_readonly.pb.js",
		Args:    exactArgs(0, "guard hook --superuser <email>"),
		RunE: func(_ *cobra.Command, _ []string) error {
			quoted := make([]string, 0, len(emails))
			for _, email := range emails {
				if strings.ContainsAny(email, "\"\\\n") || !strings.Contains(email, "@") {
					return usagef("%q is not an email address", email)
				}
				quoted = append(quoted, `"`+strings.ToLower(email)+`"`)
			}
			fmt.Fprint(a.printer.Out, strings.ReplaceAll(serverHookTemplate, hookEmailsPlaceholder, strings.Join(quoted, ", ")))
			return nil
		},
	}
	hook.Flags().StringArrayVar(&emails, "superuser", nil, "email of a superuser to make read-only (repeatable)")
	hook.MarkFlagRequired("superuser")
	guardCmd.AddCommand(hook)
	return guardCmd
}

type gatewayFlags struct {
	listen     string
	upstream   string
	identity   string
	collection string
	allowHosts []string
}

func (a *app) gatewayCommand() *cobra.Command {
	flags := &gatewayFlags{}
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Run a read-only gateway that keeps the credential away from the agent",
		Long: "The gateway logs in to PocketBase itself and forwards only read requests. An agent that is given the gateway URL\n" +
			"never holds a credential, so it cannot write even with curl.\n\n" +
			"With --upstream the password is asked for on the terminal (or read from $" + envGatewayPassword + ") and kept in memory only.\n" +
			"Without it the selected profile's credentials are used.\n" +
			"Set $" + envGatewayAccessKey + " to require that key from clients.",
		Example: "  pbctl gateway --upstream https://pb.example.com --identity admin@example.com\n  pbctl profile add prod --url http://127.0.0.1:8099   # what the agent uses",
		Args:    exactArgs(0, "gateway"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			gatewayConfig, err := a.gatewayConfig(flags)
			if err != nil {
				return err
			}
			return a.serveGateway(cmd.Context(), flags.listen, gatewayConfig)
		},
	}
	options := cmd.Flags()
	options.StringVar(&flags.listen, "listen", "127.0.0.1:8099", "address to listen on")
	options.StringVar(&flags.upstream, "upstream", "", "PocketBase URL (default: the selected profile)")
	options.StringVar(&flags.identity, "identity", "", "superuser email for --upstream")
	options.StringVar(&flags.collection, "collection", config.SuperusersCollection, "auth collection for --upstream")
	options.StringArrayVar(&flags.allowHosts, "allow-host", nil, "extra Host header value to accept, e.g. gateway.internal:8099")
	return cmd
}

func (a *app) gatewayConfig(flags *gatewayFlags) (gateway.Config, error) {
	gatewayConfig := gateway.Config{Log: a.printer.Err, AccessKey: os.Getenv(envGatewayAccessKey), AllowedHosts: acceptedHosts(flags.listen, flags.allowHosts)}
	if flags.upstream == "" {
		if err := a.selectProfile(); err != nil {
			return gatewayConfig, err
		}
		upstream, err := a.profile.BaseURL()
		gatewayConfig.Upstream = upstream
		gatewayConfig.Collection = a.profile.Collection()
		gatewayConfig.Identity = a.profile.Identity
		gatewayConfig.Password = a.profile.ResolvedPassword()
		gatewayConfig.StaticToken = a.profile.ResolvedToken()
		return gatewayConfig, err
	}
	upstream, err := (&config.Profile{URL: flags.upstream}).BaseURL()
	if err != nil {
		return gatewayConfig, &usageError{message: err.Error()}
	}
	if flags.identity == "" {
		return gatewayConfig, usagef("--upstream needs --identity <superuser email>")
	}
	password, err := a.gatewayPassword(flags.identity)
	gatewayConfig.Upstream = upstream
	gatewayConfig.Collection = flags.collection
	gatewayConfig.Identity = flags.identity
	gatewayConfig.Password = password
	return gatewayConfig, err
}

func (a *app) gatewayPassword(identity string) (string, error) {
	if password := os.Getenv(envGatewayPassword); password != "" {
		return password, nil
	}
	if !a.stdinIsTerminal() {
		return "", usagef("no password: run the gateway from a terminal or set $%s", envGatewayPassword)
	}
	fmt.Fprintf(a.printer.Err, "Password for %s: ", identity)
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(a.printer.Err)
	return string(password), err
}

func acceptedHosts(listen string, extra []string) []string {
	hosts := append([]string{listen}, extra...)
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return hosts
	}
	if ip := net.ParseIP(host); host == "" || host == "localhost" || (ip != nil && (ip.IsLoopback() || ip.IsUnspecified())) {
		hosts = append(hosts, "localhost:"+port, "127.0.0.1:"+port, "[::1]:"+port)
	}
	return hosts
}

func (a *app) serveGateway(ctx context.Context, listen string, gatewayConfig gateway.Config) error {
	if !listensOnLoopback(listen) && gatewayConfig.AccessKey == "" {
		return usagef("%s is reachable from other machines: set $%s so only your clients can read through the gateway", listen, envGatewayAccessKey)
	}
	protectProcessMemory()
	handler := gateway.New(gatewayConfig)
	if err := handler.CheckCredentials(ctx); err != nil {
		return &pb.AuthError{Reason: err.Error()}
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		server.Close()
	}()
	a.printer.Note("read-only gateway for %s listening on http://%s", gatewayConfig.Upstream, listener.Addr())
	a.printer.Note("give the agent: pbctl profile add <name> --url http://%s", listener.Addr())
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func listensOnLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

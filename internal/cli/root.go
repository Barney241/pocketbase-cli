package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Barney241/pocketbase-cli/internal/config"
	"github.com/Barney241/pocketbase-cli/internal/guard"
	"github.com/Barney241/pocketbase-cli/internal/output"
	"github.com/Barney241/pocketbase-cli/internal/pb"
)

const (
	exitFailure      = 1
	exitUsage        = 2
	exitBlocked      = guard.ExitCodeBlocked
	exitAuth         = 4
	exitNotFound     = 5
	exitUnconfirmed  = 6
	envOutput        = "PBCTL_OUTPUT"
	defaultTimeoutIn = 30 * time.Second
)

type app struct {
	version        string
	stdin          io.Reader
	printer        *output.Printer
	profileFlag    string
	readOnlyFlag   bool
	outputFlag     string
	fullFlag       bool
	maxCellFlag    int
	dryRunFlag     bool
	yesFlag        bool
	quietFlag      bool
	asFlag         string
	timeoutFlag    time.Duration
	profileName    string
	profile        *config.Profile
	readOnlySource string
	client         *pb.Client
}

type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }

type unconfirmedError struct{ action string }

func (e *unconfirmedError) Error() string {
	return fmt.Sprintf("refusing to %s without --yes", e.action)
}

type humanRequiredError struct{ reason string }

func (e *humanRequiredError) Error() string { return e.reason }

func usagef(format string, arguments ...any) error {
	return &usageError{message: fmt.Sprintf(format, arguments...)}
}

func Execute(version string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return Run(ctx, version, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

func Run(ctx context.Context, version string, arguments []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{version: version, stdin: stdin}
	a.printer = &output.Printer{Out: stdout, Err: stderr, Interactive: isTerminal(stdout)}
	root := a.rootCommand()
	root.SetArgs(arguments)
	root.SetOut(stdout)
	root.SetErr(stderr)
	return a.report(root.ExecuteContext(ctx))
}

func isTerminal(stream any) bool {
	file, isFile := stream.(*os.File)
	return isFile && term.IsTerminal(int(file.Fd()))
}

func (a *app) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "pbctl",
		Short:         "PocketBase from the command line, built for agents",
		Long:          "pbctl reads and manages any PocketBase instance over its REST API.\nOutput is compact by default. Run `pbctl guide` for the one-page reference.",
		Version:       a.version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return a.configurePrinter()
		},
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{message: err.Error()} })
	flags := root.PersistentFlags()
	flags.StringVarP(&a.profileFlag, "profile", "p", "", "profile to use (default: $PBCTL_PROFILE, then the current profile)")
	flags.BoolVar(&a.readOnlyFlag, "read-only", false, "refuse every request that could write; it can only add protection, never remove it")
	flags.StringVarP(&a.outputFlag, "output", "o", "", "table, json or jsonl (default: $PBCTL_OUTPUT, then table)")
	flags.BoolVar(&a.fullFlag, "full", false, "do not shorten long values in table output")
	flags.IntVar(&a.maxCellFlag, "max-cell", output.DefaultMaxCell, "longest value shown in table output")
	flags.BoolVar(&a.dryRunFlag, "dry-run", false, "print the write request instead of sending it")
	flags.BoolVarP(&a.yesFlag, "yes", "y", false, "confirm a destructive or protected write")
	flags.BoolVarP(&a.quietFlag, "quiet", "q", false, "print only the essential result and no notes")
	flags.StringVar(&a.asFlag, "as", "", "act as an auth record, <collection>/<id>, to test API rules")
	flags.DurationVar(&a.timeoutFlag, "timeout", defaultTimeoutIn, "time limit for one request")
	root.AddCommand(
		a.recordsCommand(), a.collectionsCommand(), a.logsCommand(), a.filesCommand(),
		a.settingsCommand(), a.backupsCommand(), a.cronsCommand(), a.sqlCommand(),
		a.authCommand(), a.apiCommand(), a.profileCommand(), a.statusCommand(),
		a.guideCommand(), a.gatewayCommand(), a.guardCommand(), a.mcpCommand(),
	)
	return root
}

func (a *app) configurePrinter() error {
	format, err := output.ParseFormat(firstNonEmpty(a.outputFlag, os.Getenv(envOutput)))
	if err != nil {
		return &usageError{message: err.Error()}
	}
	a.printer.Format = format
	a.printer.Quiet = a.quietFlag
	a.printer.MaxCell = a.maxCellFlag
	if a.fullFlag {
		a.printer.MaxCell = 0
	}
	return nil
}

func (a *app) selectProfile() error {
	if a.profile != nil {
		return nil
	}
	file, err := config.Load()
	if err != nil {
		return err
	}
	name, profile, err := file.Resolve(a.profileFlag)
	if err != nil {
		return &usageError{message: err.Error()}
	}
	source, err := readOnlySourceFor(profile, a.readOnlyFlag)
	if err != nil {
		return err
	}
	a.profileName, a.profile, a.readOnlySource = name, profile, source
	return nil
}

func readOnlySourceFor(profile *config.Profile, flag bool) (string, error) {
	base, err := profile.BaseURL()
	if err != nil {
		return "", &usageError{message: err.Error()}
	}
	policy, err := config.LoadPolicy()
	if err != nil {
		return "", err
	}
	return config.ReadOnlySource(policy, profile, base.Hostname(), flag), nil
}

func (a *app) pb() (*pb.Client, error) {
	if a.client != nil {
		return a.client, nil
	}
	if err := a.selectProfile(); err != nil {
		return nil, err
	}
	client, err := pb.New(pb.Options{
		ProfileName:    a.profileName,
		Profile:        a.profile,
		ReadOnlySource: a.readOnlySource,
		DryRun:         a.dryRunFlag,
		Timeout:        a.timeoutFlag,
		Impersonate:    a.asFlag,
		Version:        a.version,
	})
	if err != nil {
		return nil, err
	}
	a.client = client
	return client, nil
}

func (a *app) clientForProfile(name string) (*pb.Client, error) {
	file, err := config.Load()
	if err != nil {
		return nil, err
	}
	profile, found := file.Profiles[name]
	if !found {
		return nil, usagef("profile %q does not exist (known: %s)", name, strings.Join(file.Names(), ", "))
	}
	source, err := readOnlySourceFor(profile, a.readOnlyFlag)
	if err != nil {
		return nil, err
	}
	return pb.New(pb.Options{ProfileName: name, Profile: profile, ReadOnlySource: source, Timeout: a.timeoutFlag, Version: a.version})
}

func (a *app) confirm(action string, destructive bool) error {
	if err := a.selectProfile(); err != nil {
		return err
	}
	if a.dryRunFlag || a.readOnlySource != "" || a.yesFlag {
		return nil
	}
	if !destructive && !a.profile.ConfirmWrites {
		return nil
	}
	if !a.stdinIsTerminal() {
		return &unconfirmedError{action: action}
	}
	fmt.Fprintf(a.printer.Err, "%s on %s (%s)? [y/N] ", capitalize(action), a.profileName, a.profile.URL)
	answer, _ := bufio.NewReader(a.stdin).ReadString('\n')
	if strings.EqualFold(strings.TrimSpace(answer), "y") {
		return nil
	}
	return &unconfirmedError{action: action}
}

func (a *app) stdinIsTerminal() bool {
	return isTerminal(a.stdin)
}

func (a *app) report(err error) int {
	if err == nil {
		return 0
	}
	var dryRun *pb.DryRunError
	if errors.As(err, &dryRun) {
		a.printDryRun(dryRun)
		return 0
	}
	fmt.Fprintln(a.printer.Err, "error:", err)
	if hint := hintFor(err); hint != "" {
		fmt.Fprintln(a.printer.Err, "hint:", hint)
	}
	return exitCodeFor(err)
}

func (a *app) printDryRun(dryRun *pb.DryRunError) {
	preview := map[string]any{"dryRun": true, "method": dryRun.Method, "url": dryRun.URL}
	if json.Valid(dryRun.Body) {
		preview["body"] = json.RawMessage(dryRun.Body)
	} else if len(dryRun.Body) > 0 {
		preview["body"] = fmt.Sprintf("(%d bytes, not JSON)", len(dryRun.Body))
	}
	a.printer.Value(preview)
}

func exitCodeFor(err error) int {
	var usage *usageError
	var blocked *guard.BlockedError
	var auth *pb.AuthError
	var unconfirmed *unconfirmedError
	var api *pb.APIError
	var humanRequired *humanRequiredError
	switch {
	case errors.As(err, &humanRequired):
		return exitUnconfirmed
	case errors.As(err, &usage):
		return exitUsage
	case errors.As(err, &blocked):
		return exitBlocked
	case errors.As(err, &auth):
		return exitAuth
	case errors.As(err, &unconfirmed):
		return exitUnconfirmed
	case errors.As(err, &api) && refusedByServerGuard(api):
		return exitBlocked
	case errors.As(err, &api):
		return exitCodeForStatus(api.Status)
	}
	return exitFailure
}

func exitCodeForStatus(status int) int {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return exitAuth
	case http.StatusNotFound:
		return exitNotFound
	}
	return exitFailure
}

func hintFor(err error) string {
	var blocked *guard.BlockedError
	if errors.As(err, &blocked) {
		return "nothing was changed; writes need a profile that is not read-only, and only a person can set one up"
	}
	var unconfirmed *unconfirmedError
	if errors.As(err, &unconfirmed) {
		return "preview it with --dry-run, then repeat with --yes"
	}
	var api *pb.APIError
	if !errors.As(err, &api) {
		return ""
	}
	return hintForAPIError(api)
}

func refusedByServerGuard(api *pb.APIError) bool {
	return api.Status == http.StatusForbidden && (strings.Contains(api.Message, "pbctl guard") || strings.Contains(api.Message, "read-only gateway"))
}

func hintForAPIError(api *pb.APIError) string {
	switch {
	case refusedByServerGuard(api):
		return "nothing was changed; this access is read-only on the server side and no client setting lifts it"
	case api.Status == http.StatusNotFound && api.Path == "/api/sql":
		return "this PocketBase has no SQL endpoint; `pbctl collections dry-run-view` runs a SELECT on any version"
	case api.Status == http.StatusNotFound && strings.Contains(api.Message, "collection"):
		return "list the collection names with `pbctl collections list`"
	case api.Status == http.StatusBadRequest && strings.HasSuffix(api.Path, "/records") && api.Method == http.MethodGet:
		return "a bad --filter, --sort or --expand is the usual cause; `pbctl guide` has the syntax, `pbctl collections show <name>` the field names"
	case api.Status == http.StatusBadRequest && strings.HasPrefix(api.Path, "/api/logs"):
		return "log filters use the fields level, message, created and data.<key>"
	case api.Status == http.StatusForbidden && api.Path == "/api/batch":
		return "batch requests are off on this server; enable them with `pbctl settings update batch.enabled:=true`"
	case api.Status == http.StatusUnauthorized || api.Status == http.StatusForbidden:
		return "check who you are with `pbctl status`; most admin endpoints need a superuser profile"
	}
	return ""
}

func exactArgs(count int, usage string) cobra.PositionalArgs {
	return func(_ *cobra.Command, arguments []string) error {
		if len(arguments) != count {
			return usagef("usage: pbctl %s", usage)
		}
		return nil
	}
}

func minimumArgs(count int, usage string) cobra.PositionalArgs {
	return func(_ *cobra.Command, arguments []string) error {
		if len(arguments) < count {
			return usagef("usage: pbctl %s", usage)
		}
		return nil
	}
}

func capitalize(text string) string {
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

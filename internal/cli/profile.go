package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Barney241/pocketbase-cli/internal/config"
)

type profileFlags struct {
	url           string
	collection    string
	identity      string
	identityEnv   string
	headerEnv     []string
	passwordEnv   string
	passwordStdin bool
	tokenEnv      string
	tokenStdin    bool
	gatewayKeyEnv string
	writable      bool
	confirmWrites bool
	makeCurrent   bool
}

func (a *app) profileCommand() *cobra.Command {
	profile := &cobra.Command{Use: "profile", Aliases: []string{"profiles"}, Short: "Manage connections to PocketBase instances"}
	profile.AddCommand(a.profileAddCommand(), a.profileListCommand(), a.profileUseCommand(), a.profileRemoveCommand(), a.profileLockCommand(), a.profileUnlockCommand())
	return profile
}

func (a *app) profileAddCommand() *cobra.Command {
	flags := &profileFlags{}
	cmd := &cobra.Command{
		Use:   "add <name> --url <url>",
		Short: "Add a profile; it is read-only unless a person passes --writable",
		Long: "Credentials are optional: without them requests go out anonymously, which is what a read-only gateway expects.\n" +
			"The password is asked for on the terminal, or read from --password-stdin, or looked up in --password-env at run time.\n" +
			"A --writable profile can only be created from an interactive terminal.",
		Example: "  pbctl profile add local --url http://127.0.0.1:8090 --identity admin@example.com --writable\n  pbctl profile add prod --url https://pb.example.com --identity ro@example.com --password-env PB_PROD_PASSWORD\n  pbctl profile add prod-gw --url http://127.0.0.1:8099",
		Args:    exactArgs(1, "profile add <name> --url <url>"),
		RunE: func(_ *cobra.Command, arguments []string) error {
			return a.addProfile(arguments[0], flags)
		},
	}
	options := cmd.Flags()
	options.StringVar(&flags.url, "url", "", "base URL of the instance or of a pbctl gateway")
	options.StringVar(&flags.collection, "collection", config.SuperusersCollection, "auth collection to log in to")
	options.StringVar(&flags.identity, "identity", "", "email or username to log in with")
	options.StringVar(&flags.identityEnv, "identity-env", "", "name of the environment variable that holds the email or username")
	options.StringArrayVar(&flags.headerEnv, "header-env", nil, "extra request header read from an environment variable, <Header>=<VAR>; repeatable")
	options.StringVar(&flags.passwordEnv, "password-env", "", "name of the environment variable that holds the password")
	options.BoolVar(&flags.passwordStdin, "password-stdin", false, "read the password from stdin and store it in the config file")
	options.StringVar(&flags.tokenEnv, "token-env", "", "name of the environment variable that holds an auth token")
	options.BoolVar(&flags.tokenStdin, "token-stdin", false, "read an auth token from stdin and store it in the config file")
	options.StringVar(&flags.gatewayKeyEnv, "gateway-key-env", "", "name of the environment variable that holds the gateway access key, stored in the config file")
	options.BoolVar(&flags.writable, "writable", false, "allow writes (asks for confirmation on the terminal)")
	options.BoolVar(&flags.confirmWrites, "confirm-writes", false, "require --yes for every write, not only destructive ones")
	options.BoolVar(&flags.makeCurrent, "use", false, "make it the current profile")
	cmd.MarkFlagRequired("url")
	return cmd
}

func (a *app) addProfile(name string, flags *profileFlags) error {
	file, err := config.Load()
	if err != nil {
		return err
	}
	if _, exists := file.Profiles[name]; exists {
		return usagef("profile %q already exists; remove it first with `pbctl profile remove %s`", name, name)
	}
	headerEnv, err := headerVariables(flags.headerEnv)
	if err != nil {
		return err
	}
	profile := &config.Profile{
		URL:            strings.TrimRight(flags.url, "/"),
		AuthCollection: flags.collection,
		Identity:       flags.identity,
		IdentityEnv:    flags.identityEnv,
		HeaderEnv:      headerEnv,
		PasswordEnv:    flags.passwordEnv,
		TokenEnv:       flags.tokenEnv,
		GatewayKey:     os.Getenv(flags.gatewayKeyEnv),
		ReadOnly:       !flags.writable,
		ConfirmWrites:  flags.confirmWrites,
	}
	if _, err := profile.BaseURL(); err != nil {
		return &usageError{message: err.Error()}
	}
	if flags.writable {
		if err := a.requireHuman(fmt.Sprintf("Profile %q will be able to WRITE to %s.", name, profile.URL), name); err != nil {
			return err
		}
	}
	if err := a.collectSecrets(profile, flags); err != nil {
		return err
	}
	file.Profiles[name] = profile
	if flags.makeCurrent || file.Current == "" {
		file.Current = name
	}
	if err := file.Save(); err != nil {
		return err
	}
	a.printer.Line("added %s (%s)", name, describeMode(profile.ReadOnly))
	return nil
}

func headerVariables(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	variables := map[string]string{}
	for _, pair := range pairs {
		header, variable, found := strings.Cut(pair, "=")
		header, variable = strings.TrimSpace(header), strings.TrimSpace(variable)
		if !found || header == "" || variable == "" {
			return nil, usagef("--header-env expects <Header>=<VAR>, got %q", pair)
		}
		if config.IsReservedHeader(header) {
			return nil, usagef("--header-env cannot set %s: pbctl sets that header itself", header)
		}
		variables[header] = variable
	}
	return variables, nil
}

func (a *app) collectSecrets(profile *config.Profile, flags *profileFlags) error {
	switch {
	case flags.tokenStdin:
		token, err := a.readSecretLine()
		profile.Token = token
		return err
	case flags.passwordStdin:
		password, err := a.readSecretLine()
		profile.Password = password
		return err
	case profile.IdentityEnv != "" && profile.PasswordEnv == "" && profile.TokenEnv == "":
		return usagef("--identity-env needs --password-env <VAR>")
	case profile.Identity != "" && profile.PasswordEnv == "" && profile.TokenEnv == "":
		if !a.stdinIsTerminal() {
			return usagef("no password source: add --password-env <VAR> or --password-stdin")
		}
		fmt.Fprintf(a.printer.Err, "Password for %s: ", profile.Identity)
		password, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(a.printer.Err)
		profile.Password = string(password)
		return err
	}
	return nil
}

func (a *app) readSecretLine() (string, error) {
	line, err := bufio.NewReader(a.stdin).ReadString('\n')
	secret := strings.TrimSpace(line)
	if secret == "" {
		return "", usagef("nothing was read from stdin: %v", err)
	}
	return secret, nil
}

func (a *app) requireHuman(warning, wordToType string) error {
	if !a.stdinIsTerminal() {
		return &humanRequiredError{reason: "making a profile writable needs a person at an interactive terminal; this run has none, so nothing was changed"}
	}
	fmt.Fprintf(a.printer.Err, "%s\nType %q to confirm: ", warning, wordToType)
	answer, _ := bufio.NewReader(a.stdin).ReadString('\n')
	if strings.TrimSpace(answer) != wordToType {
		return &humanRequiredError{reason: "the confirmation did not match; nothing was changed"}
	}
	return nil
}

func describeMode(readOnly bool) string {
	if readOnly {
		return "read-only"
	}
	return "writable"
}

func (a *app) profileListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List profiles; secrets are never printed",
		Args:    exactArgs(0, "profile list"),
		RunE: func(_ *cobra.Command, _ []string) error {
			file, err := config.Load()
			if err != nil {
				return err
			}
			rows := []map[string]any{}
			for _, name := range file.Names() {
				profile := file.Profiles[name]
				rows = append(rows, map[string]any{
					"name":     name,
					"current":  name == file.Current,
					"url":      profile.URL,
					"identity": describeIdentitySource(profile),
					"mode":     describeMode(profile.ReadOnly),
				})
			}
			if len(rows) == 0 {
				a.printer.Note("no profiles; add one with `pbctl profile add <name> --url <url>`")
			}
			return a.printer.List(rows, []string{"name", "current", "url", "identity", "mode"}, rows)
		},
	}
}

func describeIdentitySource(profile *config.Profile) string {
	switch {
	case profile.Identity != "":
		return profile.Identity
	case profile.IdentityEnv != "":
		return "(identity from $" + profile.IdentityEnv + ")"
	}
	return describeTokenSource(profile)
}

func describeTokenSource(profile *config.Profile) string {
	switch {
	case profile.TokenEnv != "":
		return "(token from $" + profile.TokenEnv + ")"
	case profile.Token != "":
		return "(stored token)"
	}
	return "(anonymous)"
}

func (a *app) profileUseCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Make a profile the current one",
		Args:  exactArgs(1, "profile use <name>"),
		RunE: func(_ *cobra.Command, arguments []string) error {
			return a.editProfile(arguments[0], func(file *config.File, _ *config.Profile) error {
				file.Current = arguments[0]
				a.printer.Line("now using %s", arguments[0])
				return nil
			})
		},
	}
}

func (a *app) profileRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Remove a profile and its stored secrets",
		Args:    exactArgs(1, "profile remove <name>"),
		RunE: func(_ *cobra.Command, arguments []string) error {
			return a.editProfile(arguments[0], func(file *config.File, _ *config.Profile) error {
				delete(file.Profiles, arguments[0])
				if file.Current == arguments[0] {
					file.Current = ""
				}
				a.printer.Line("removed %s", arguments[0])
				return nil
			})
		},
	}
}

func (a *app) profileLockCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "lock <name>",
		Short: "Make a profile read-only",
		Args:  exactArgs(1, "profile lock <name>"),
		RunE: func(_ *cobra.Command, arguments []string) error {
			return a.editProfile(arguments[0], func(_ *config.File, profile *config.Profile) error {
				profile.ReadOnly = true
				a.printer.Line("%s is read-only", arguments[0])
				return nil
			})
		},
	}
}

func (a *app) profileUnlockCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock <name>",
		Short: "Allow writes on a profile (interactive terminal only)",
		Args:  exactArgs(1, "profile unlock <name>"),
		RunE: func(_ *cobra.Command, arguments []string) error {
			return a.editProfile(arguments[0], func(_ *config.File, profile *config.Profile) error {
				if err := a.requireHuman(fmt.Sprintf("Profile %q will be able to WRITE to %s.", arguments[0], profile.URL), arguments[0]); err != nil {
					return err
				}
				profile.ReadOnly = false
				a.printer.Line("%s is writable", arguments[0])
				return nil
			})
		},
	}
}

func (a *app) editProfile(name string, change func(*config.File, *config.Profile) error) error {
	file, err := config.Load()
	if err != nil {
		return err
	}
	profile, found := file.Profiles[name]
	if !found {
		return usagef("profile %q does not exist (known: %s)", name, strings.Join(file.Names(), ", "))
	}
	if err := change(file, profile); err != nil {
		return err
	}
	return file.Save()
}

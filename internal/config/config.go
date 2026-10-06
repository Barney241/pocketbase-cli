package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	SuperusersCollection = "_superusers"
	EnvConfigPath        = "PBCTL_CONFIG"
	EnvProfile           = "PBCTL_PROFILE"
	EnvReadOnly          = "PBCTL_READ_ONLY"
	EnvURL               = "PBCTL_URL"
	EnvToken             = "PBCTL_TOKEN"
	EnvIdentity          = "PBCTL_IDENTITY"
	EnvPassword          = "PBCTL_PASSWORD"
	EnvProfileName       = "(environment)"
)

var PolicyPath = "/etc/pbctl/policy.json"

type Profile struct {
	URL            string `json:"url"`
	AuthCollection string `json:"auth_collection,omitempty"`
	Identity       string `json:"identity,omitempty"`
	Password       string `json:"password,omitempty"`
	PasswordEnv    string `json:"password_env,omitempty"`
	Token          string `json:"token,omitempty"`
	TokenEnv       string `json:"token_env,omitempty"`
	Gateway        bool   `json:"gateway,omitempty"`
	GatewayKey     string `json:"gateway_key,omitempty"`
	ReadOnly       bool   `json:"read_only"`
	ConfirmWrites  bool   `json:"confirm_writes,omitempty"`
}

type File struct {
	Current  string              `json:"current,omitempty"`
	Profiles map[string]*Profile `json:"profiles"`
}

type Policy struct {
	ReadOnly      bool     `json:"read_only"`
	ReadOnlyHosts []string `json:"read_only_hosts"`
}

func Path() (string, error) {
	if custom := os.Getenv(EnvConfigPath); custom != "" {
		return custom, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate the config directory: %w", err)
	}
	return filepath.Join(dir, "pbctl", "config.json"), nil
}

func Load() (*File, error) {
	file := &File{Profiles: map[string]*Profile{}}
	location, err := Path()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(location)
	if errors.Is(err, os.ErrNotExist) {
		return file, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", location, err)
	}
	if err := json.Unmarshal(raw, file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", location, err)
	}
	if file.Profiles == nil {
		file.Profiles = map[string]*Profile{}
	}
	return file, nil
}

func (f *File) Save() error {
	location, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(location), 0o700); err != nil {
		return fmt.Errorf("create the config directory: %w", err)
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	temporary := location + ".tmp"
	if err := os.WriteFile(temporary, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", temporary, err)
	}
	return os.Rename(temporary, location)
}

func (f *File) Names() []string {
	names := make([]string, 0, len(f.Profiles))
	for name := range f.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (f *File) Resolve(requested string) (string, *Profile, error) {
	name := firstNonEmpty(requested, os.Getenv(EnvProfile))
	if name == "" && os.Getenv(EnvURL) != "" {
		return EnvProfileName, profileFromEnvironment(), nil
	}
	name = firstNonEmpty(name, f.Current)
	if name == "" {
		return "", nil, errors.New("no profile selected: run `pbctl profile add <name> --url <url>` or set PBCTL_URL")
	}
	profile, found := f.Profiles[name]
	if !found {
		return "", nil, fmt.Errorf("profile %q does not exist (known: %s)", name, strings.Join(f.Names(), ", "))
	}
	return name, profile, nil
}

func profileFromEnvironment() *Profile {
	return &Profile{
		URL:         os.Getenv(EnvURL),
		Identity:    os.Getenv(EnvIdentity),
		PasswordEnv: EnvPassword,
		TokenEnv:    EnvToken,
	}
}

func (p *Profile) Collection() string {
	return firstNonEmpty(p.AuthCollection, SuperusersCollection)
}

func (p *Profile) ResolvedToken() string {
	if p.TokenEnv != "" {
		if token := os.Getenv(p.TokenEnv); token != "" {
			return token
		}
	}
	return p.Token
}

func (p *Profile) ResolvedPassword() string {
	if p.PasswordEnv != "" {
		if password := os.Getenv(p.PasswordEnv); password != "" {
			return password
		}
	}
	return p.Password
}

func (p *Profile) HasCredentials() bool {
	return p.ResolvedToken() != "" || (p.Identity != "" && p.ResolvedPassword() != "")
}

func (p *Profile) BaseURL() (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimRight(p.URL, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid profile url %q: %w", p.URL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("profile url %q must start with http:// or https://", p.URL)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("profile url %q has no host", p.URL)
	}
	return parsed, nil
}

func LoadPolicy() (*Policy, error) {
	policy := &Policy{}
	raw, err := os.ReadFile(PolicyPath)
	if errors.Is(err, os.ErrNotExist) {
		return policy, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the system policy %s: %w", PolicyPath, err)
	}
	if err := json.Unmarshal(raw, policy); err != nil {
		return nil, fmt.Errorf("parse the system policy %s: %w", PolicyPath, err)
	}
	return policy, nil
}

func (p *Policy) ForcesReadOnly(host string) bool {
	if p.ReadOnly {
		return true
	}
	for _, pattern := range p.ReadOnlyHosts {
		if matched, _ := path.Match(strings.ToLower(pattern), strings.ToLower(host)); matched {
			return true
		}
	}
	return false
}

func ReadOnlySource(policy *Policy, profile *Profile, host string, flag bool) string {
	switch {
	case policy.ForcesReadOnly(host):
		return "the system policy " + PolicyPath
	case profile.ReadOnly:
		return "the profile"
	case isTruthy(os.Getenv(EnvReadOnly)):
		return "the " + EnvReadOnly + " environment variable"
	case flag:
		return "the --read-only flag"
	}
	return ""
}

func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

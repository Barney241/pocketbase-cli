package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func usePolicy(t *testing.T, content string) {
	t.Helper()
	original := PolicyPath
	PolicyPath = filepath.Join(t.TempDir(), "policy.json")
	t.Cleanup(func() { PolicyPath = original })
	if content == "" {
		return
	}
	if err := os.WriteFile(PolicyPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadOnlyTurnsOnFromAnySourceAndNothingTurnsItOff(t *testing.T) {
	cases := []struct {
		name       string
		policy     string
		profile    bool
		env        string
		flag       bool
		host       string
		wantSource string
	}{
		{name: "nothing set", host: "pb.example.com"},
		{name: "profile", profile: true, host: "pb.example.com", wantSource: "the profile"},
		{name: "environment", env: "1", host: "pb.example.com", wantSource: "PBCTL_READ_ONLY"},
		{name: "environment set to false", env: "false", host: "pb.example.com"},
		{name: "flag", flag: true, host: "pb.example.com", wantSource: "--read-only"},
		{name: "policy for every host", policy: `{"read_only":true}`, host: "localhost", wantSource: "system policy"},
		{name: "policy for a matching host", policy: `{"read_only_hosts":["*.example.com"]}`, host: "PB.example.com", wantSource: "system policy"},
		{name: "policy for another host", policy: `{"read_only_hosts":["*.example.com"]}`, host: "localhost"},
		{name: "policy wins over a writable profile and a false env", policy: `{"read_only":true}`, env: "0", host: "x", wantSource: "system policy"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			usePolicy(t, testCase.policy)
			t.Setenv(EnvReadOnly, testCase.env)
			policy, err := LoadPolicy()
			if err != nil {
				t.Fatal(err)
			}
			source := ReadOnlySource(policy, &Profile{ReadOnly: testCase.profile}, testCase.host, testCase.flag)
			if testCase.wantSource == "" && source != "" {
				t.Fatalf("read-only is on (%s), want off", source)
			}
			if testCase.wantSource != "" && !strings.Contains(source, testCase.wantSource) {
				t.Fatalf("source = %q, want it to mention %q", source, testCase.wantSource)
			}
		})
	}
}

func TestABrokenPolicyFileIsAnErrorNotAnOpenDoor(t *testing.T) {
	usePolicy(t, `{"read_only": tru`)
	if _, err := LoadPolicy(); err == nil {
		t.Fatal("a malformed policy file was accepted")
	}
}

func TestProfilesSurviveASaveAndStayPrivate(t *testing.T) {
	location := filepath.Join(t.TempDir(), "nested", "config.json")
	t.Setenv(EnvConfigPath, location)
	t.Setenv(EnvProfile, "")
	t.Setenv(EnvURL, "")
	file, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	file.Profiles["prod"] = &Profile{URL: "https://pb.example.com", Identity: "a@b.c", Password: "secret", ReadOnly: true}
	file.Current = "prod"
	if err := file.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(location)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, want 0600", info.Mode().Perm())
	}
	reloaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	name, profile, err := reloaded.Resolve("")
	if err != nil || name != "prod" || !profile.ReadOnly || profile.ResolvedPassword() != "secret" {
		t.Fatalf("resolved %q %+v %v", name, profile, err)
	}
	if _, _, err := reloaded.Resolve("missing"); err == nil {
		t.Fatal("an unknown profile resolved")
	}
}

func TestEnvironmentVariablesFormAProfileAndOverrideStoredSecrets(t *testing.T) {
	t.Setenv(EnvProfile, "")
	t.Setenv(EnvURL, "http://127.0.0.1:8090/")
	t.Setenv(EnvIdentity, "admin@example.com")
	t.Setenv(EnvPassword, "from-env")
	name, profile, err := (&File{Profiles: map[string]*Profile{}}).Resolve("")
	if err != nil || name != EnvProfileName {
		t.Fatalf("resolved %q %v", name, err)
	}
	base, err := profile.BaseURL()
	if err != nil || base.String() != "http://127.0.0.1:8090" {
		t.Fatalf("base url = %v %v", base, err)
	}
	if !profile.HasCredentials() || profile.ResolvedPassword() != "from-env" || profile.Collection() != SuperusersCollection {
		t.Fatalf("unexpected profile %+v", profile)
	}
	for _, invalid := range []string{"pb.example.com", "ftp://pb.example.com", "http://"} {
		if _, err := (&Profile{URL: invalid}).BaseURL(); err == nil {
			t.Errorf("url %q was accepted", invalid)
		}
	}
}

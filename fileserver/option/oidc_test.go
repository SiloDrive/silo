package option

import "testing"

func TestTheOIDCSectionIsRead(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, `[oidc]
issuer = https://id.example.com/
client_id = silo
client_secret = from the file
accounts = create
allowed_domains = Example.com, @example.org
`))

	if OIDC.Issuer != "https://id.example.com/" || OIDC.ClientID != "silo" ||
		OIDC.ClientSecret != "from the file" || OIDC.Accounts != "create" {
		t.Errorf("[oidc] read as %+v", OIDC)
	}
	if OIDC.AllowedDomains != "Example.com, @example.org" {
		t.Errorf("allowed_domains = %q, want it as written", OIDC.AllowedDomains)
	}
	if !OIDC.Configured() {
		t.Error("a full [oidc] section reads as unconfigured")
	}
}

// The environment wins, key by key, as it does for every other setting --
// which is what lets the secret live in silo.env while the rest sits in the
// file.
func TestTheEnvironmentBeatsTheOIDCSection(t *testing.T) {
	t.Setenv("SILO_OIDC_CLIENT_SECRET", "from the environment")
	t.Setenv("SILO_OIDC_ALLOWED_DOMAINS", "example.net")
	LoadFileServerOptions(writeConfig(t, "[oidc]\nissuer = https://id.example.com/\nclient_secret = from the file\n"))

	if OIDC.ClientSecret != "from the environment" {
		t.Errorf("client secret = %q, want the environment's", OIDC.ClientSecret)
	}
	if OIDC.Issuer != "https://id.example.com/" {
		t.Errorf("issuer = %q, want the file's, which the environment did not set", OIDC.Issuer)
	}
	if OIDC.AllowedDomains != "example.net" {
		t.Errorf("allowed domains = %q", OIDC.AllowedDomains)
	}
}

// A second load starts from nothing: an issuer from the last load must not
// survive into one whose file and environment say nothing about OIDC.
func TestOIDCIsUnconfiguredByDefault(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, "[oidc]\nissuer = https://id.example.com/\n"))
	LoadFileServerOptions("")
	if OIDC.Configured() {
		t.Errorf("OIDC is configured with no file and no environment: %+v", OIDC)
	}
}

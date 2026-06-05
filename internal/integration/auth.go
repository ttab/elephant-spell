package integration

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/ttab/elephantine"
	"github.com/ttab/eltest"
)

// BearerClient is a Twirp HTTPClient that attaches an Authorization: Bearer
// header to every request, so typed Twirp clients speak production-shape auth.
type BearerClient struct {
	base  *http.Client
	token string
}

// BearerHTTPClient wraps base so each request carries the given bearer token.
// A nil base uses http.DefaultClient.
func BearerHTTPClient(base *http.Client, token string) *BearerClient {
	if base == nil {
		base = http.DefaultClient
	}

	return &BearerClient{
		base:  base,
		token: strings.TrimPrefix(token, "Bearer "),
	}
}

// Do implements Twirp's HTTPClient interface.
func (c *BearerClient) Do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.base.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bearer client: %w", err)
	}

	return resp, nil
}

// Caller bundles a JWT subject and a matching bearer token for one test
// identity.
type Caller struct {
	Subject string
	Token   string
}

// Caller mints a real JWT through Elsinod for name (the client_credentials
// client_id) with the given scopes. The subject lands on
// elephantine.AuthInfo.Claims.Subject when the server validates the token.
func (e Environment) Caller(t T, name string, scopes ...string) Caller {
	t.Helper()

	auth, err := elephantine.AuthenticationConfigFromSettings(t.Context(),
		elephantine.AuthenticationSettings{
			OIDCConfig:   e.Elsinod.HostOIDCConfig(),
			ClientID:     name,
			ClientSecret: "pass",
		}, scopes)
	eltest.Must(eltestT{t}, err, "build authentication config for %s", name)

	tok, err := auth.TokenSource.Token()
	eltest.Must(eltestT{t}, err, "fetch token for %s", name)

	return Caller{
		Subject: "core://application/" + name,
		Token:   tok.AccessToken,
	}
}

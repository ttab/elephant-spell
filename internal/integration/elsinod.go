package integration

import (
	"fmt"
	"net/http"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/ttab/eltest"
)

// ElsinodConfig wires the mock OIDC provider. PublicURL is the issuer claim
// Elsinod serves; the discovery document is reachable on the mapped host port
// regardless of this value, so tests use InsecureIssuerURLContext to build a
// provider against the host URL while expecting this issuer.
type ElsinodConfig struct {
	PublicURL string
}

// NewElsinod boots the Elsinod mock OIDC provider (a process-wide singleton
// via eltest.BootstrapService) and returns a handle to it.
func NewElsinod(t T, conf ElsinodConfig) *Elsinod {
	e, err := eltest.BootstrapService("elsinod", &Elsinod{conf: conf}, eltestT{t})

	eltest.Must(eltestT{t}, err, "bootstrap elsinod")

	return e
}

// Elsinod is the per-process Elsinod docker container handle. Implements
// eltest.BackingService so eltest manages startup, retry, and tear-down.
type Elsinod struct {
	conf ElsinodConfig
	res  *dockertest.Resource
}

// Issuer returns the issuer claim Elsinod stamps on tokens (its PublicURL).
func (e *Elsinod) Issuer() string {
	return e.conf.PublicURL
}

// HostBaseURL returns the OIDC base URL reachable from the test process.
func (e *Elsinod) HostBaseURL() string {
	return fmt.Sprintf("http://localhost:%s", e.res.GetPort("1080/tcp"))
}

// HostOIDCConfig returns the OIDC discovery URL reachable from the test
// process.
func (e *Elsinod) HostOIDCConfig() string {
	return e.HostBaseURL() + "/.well-known/openid-configuration"
}

// SetUp implements eltest.BackingService.
func (e *Elsinod) SetUp(pool *dockertest.Pool, network *dockertest.Network) error {
	res, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "ghcr.io/ttab/elsinod",
		Tag:        "v0.2.0-pre2",
		Cmd:        []string{"mock"},
		Env: []string{
			fmt.Sprintf("PUBLIC_URL=%s", e.conf.PublicURL),
			"CLIENT_SECRET=pass",
			"DEMO_PASSWORD=pass",
			"ORGANISATION=example",
		},
		NetworkID: network.Network.ID,
	}, func(hc *docker.HostConfig) {
		hc.AutoRemove = true
	})
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}

	e.res = res

	// Cap the container's lifetime in case in-process cleanup fails.
	_ = res.Expire(3600)

	err = pool.Retry(func() error {
		readyEndpoint := fmt.Sprintf("http://localhost:%s/health/ready",
			res.GetPort("1081/tcp"))

		resp, err := http.Get(readyEndpoint) //nolint:gosec // test backing service.
		if err != nil {
			return fmt.Errorf("readiness probe: %w", err)
		}

		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("not ready: %s", resp.Status)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("wait for elsinod ready: %w", err)
	}

	return nil
}

// Purge implements eltest.BackingService.
func (e *Elsinod) Purge(pool *dockertest.Pool) error {
	if e.res == nil {
		return nil
	}

	if err := pool.Purge(e.res); err != nil {
		return fmt.Errorf("purge elsinod container: %w", err)
	}

	return nil
}

// eltestT adapts our T (which has Context()) to eltest.T (which doesn't).
type eltestT struct {
	t T
}

func (e eltestT) Name() string {
	return e.t.Name()
}

func (e eltestT) Helper() {
	e.t.Helper()
}

func (e eltestT) Fatalf(f string, a ...any) {
	e.t.Fatalf(f, a...)
}

func (e eltestT) Cleanup(fn func()) {
	e.t.Cleanup(fn)
}

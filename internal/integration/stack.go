package integration

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	spellapi "github.com/ttab/elephant-api/spell"
	spellweb "github.com/ttab/elephant-spell"
	"github.com/ttab/elephant-spell/docs"
	"github.com/ttab/elephant-spell/internal"
	"github.com/ttab/elephantine"
	"github.com/ttab/eltest"
	"golang.org/x/oauth2"
)

// Stack is the per-test wiring: a real spell server (started via
// Application.Run against a per-test Postgres and the shared Elsinod OIDC
// provider) plus typed Twirp clients that call it over the wire with a real
// admin JWT.
type Stack struct {
	Env     Environment
	Pool    *pgxpool.Pool
	BaseURL string

	// Admin holds spell_write scope; the default clients use its token.
	Admin        Caller
	Check        spellapi.Check
	Dictionaries spellapi.Dictionaries
	Rules        spellapi.Rules
}

// NewStack boots backing services, starts the real spell server, and waits for
// it to accept connections. All failures call t.Fatalf.
func NewStack(t T) *Stack {
	t.Helper()

	ctx := t.Context()
	env := SetUpBackingServices(t)

	pool, err := pgxpool.New(ctx, env.PostgresURI)
	eltest.Must(eltestT{t}, err, "create connection pool")
	t.Cleanup(pool.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// API auth uses the production code path: a JWKS parser bound to Elsinod.
	apiAuth, err := elephantine.AuthenticationConfigFromSettings(ctx,
		elephantine.AuthenticationSettings{
			OIDCConfig: env.Elsinod.HostOIDCConfig(),
		}, nil)
	eltest.Must(eltestT{t}, err, "build API auth parser")

	// Web-UI OIDC: Elsinod stamps a fixed issuer but serves reachable
	// endpoints on the mapped host port, so discover against the host URL
	// while expecting the configured issuer.
	pctx := oidc.InsecureIssuerURLContext(ctx, env.Elsinod.Issuer())

	provider, err := oidc.NewProvider(pctx, env.Elsinod.HostBaseURL())
	eltest.Must(eltestT{t}, err, "create OIDC provider")

	verifier := provider.Verifier(&oidc.Config{
		ClientID:        "spell-ui",
		SkipIssuerCheck: true,
	})

	addr := freeAddr(t)
	baseURL := "http://" + addr

	params := internal.Parameters{
		Addr:            addr,
		Logger:          logger,
		Database:        pool,
		PubsubDatabase:  pool,
		AuthInfoParser:  apiAuth.AuthParser,
		Registerer:      prometheus.NewRegistry(),
		PingInterval:    time.Second,
		PingGrace:       3 * time.Second,
		DefaultLanguage: "sv-se",
		OIDCProvider:    provider,
		OIDCVerifier:    verifier,
		OIDCConfig: &oauth2.Config{
			ClientID:     "spell-ui",
			ClientSecret: "pass",
			Endpoint:     provider.Endpoint(),
			RedirectURL:  baseURL + "/auth/callback",
			Scopes: []string{
				oidc.ScopeOpenID, "profile", "email",
				internal.ScopeSpellcheckWrite,
			},
		},
		Templates: mustSub(t, spellweb.TemplateFS, "templates"),
		Locales:   mustSub(t, spellweb.LocaleFS, "locales"),
		Assets:    mustSub(t, spellweb.AssetFS, "assets"),
		Docs:      docs.FS,
	}

	app, err := internal.NewApplication(ctx, params)
	eltest.Must(eltestT{t}, err, "create application")

	// Run the real server; cancelling on cleanup tears down the HTTP server
	// and the background eventlog sync.
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)

	go func() {
		_ = app.Run(runCtx)
	}()

	waitReady(t, addr)

	admin := env.Caller(t, "spell-admin", internal.ScopeSpellcheckWrite)

	return &Stack{
		Env:          env,
		Pool:         pool,
		BaseURL:      baseURL,
		Admin:        admin,
		Check:        spellapi.NewCheckProtobufClient(baseURL, BearerHTTPClient(nil, admin.Token)),
		Dictionaries: spellapi.NewDictionariesProtobufClient(baseURL, BearerHTTPClient(nil, admin.Token)),
		Rules:        spellapi.NewRulesProtobufClient(baseURL, BearerHTTPClient(nil, admin.Token)),
	}
}

// CheckClient returns a Check client authenticated as the given caller.
func (s *Stack) CheckClient(c Caller) spellapi.Check {
	return spellapi.NewCheckProtobufClient(s.BaseURL, BearerHTTPClient(nil, c.Token))
}

// DictionariesClient returns a Dictionaries client authenticated as the given
// caller.
func (s *Stack) DictionariesClient(c Caller) spellapi.Dictionaries {
	return spellapi.NewDictionariesProtobufClient(s.BaseURL, BearerHTTPClient(nil, c.Token))
}

// mustSub returns the named sub-filesystem or fails the test.
func mustSub(t T, f fs.FS, dir string) fs.FS {
	t.Helper()

	sub, err := fs.Sub(f, dir)
	eltest.Must(eltestT{t}, err, "sub filesystem %q", dir)

	return sub
}

// freeAddr reserves a free localhost port and returns its address. There is a
// small race between closing the probe listener and the server binding it,
// which is acceptable for tests.
func freeAddr(t T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	eltest.Must(eltestT{t}, err, "reserve free port")

	defer func() { _ = l.Close() }()

	return l.Addr().String()
}

// waitReady blocks until the server at addr accepts connections.
func waitReady(t T, addr string) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)

	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()

			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("server at %s did not become ready", addr)
}

// WaitFor polls cond until it returns true or the deadline elapses. Used for
// asynchronous propagation (RPC commit → NOTIFY → consumer drain).
func WaitFor(t T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

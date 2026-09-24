package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"runtime/debug"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus"
	spell "github.com/ttab/elephant-spell"
	"github.com/ttab/elephant-spell/docs"
	"github.com/ttab/elephant-spell/internal"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg"
	"github.com/ttab/howdah"
	"github.com/urfave/cli/v3"
	"golang.org/x/oauth2"
)

// DefaultDBMaxConns is the size of the pool queries run on, set here rather
// than left to pgx: its default is max(4, NumCPU()) read from the node's cpuset
// rather than the cgroup quota, so an unset pool tracks whichever node the pod
// lands on and changes size invisibly on reschedule.
//
// Spellchecking never touches the database — Text and Suggestions answer from
// the in-memory checkers — so the pool only carries the dictionary and rule
// management RPCs and the background work. The background work is the entry
// updater draining the eventlog (one query at a time), the eventlog pruner and
// its job lock, and, when no bouncer is configured, the subscriber's ping. That
// is at most four connections. The management writes are editor-driven and
// serialise on the eventlog's exclusive lock, so extra concurrent writers queue
// on the lock while each holding a connection; eight leaves room for a handful
// of them on top of the background work. Trim it once
// pgxpool_empty_acquire_wait_seconds_total says what it actually needs.
const DefaultDBMaxConns = 8

// ListenPoolMaxConns is the size of the direct pool when queries go through a
// bouncer: it then carries only the LISTEN session, which the subscriber
// hijacks out of the pool, and the subscriber's ping.
const ListenPoolMaxConns = 2

func main() {
	err := godotenv.Load()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Error("exiting: ",
			elephantine.LogKeyError, err)
		os.Exit(1)
	}

	runCmd := cli.Command{
		Name:        "run",
		Description: "Runs the spelling server",
		Action:      runSpell,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "addr",
				Sources: cli.EnvVars("ADDR"),
				Value:   ":1080",
			},
			&cli.StringFlag{
				Name:    "profile-addr",
				Sources: cli.EnvVars("PROFILE_ADDR"),
				Value:   ":1081",
			},
			&cli.StringFlag{
				Name:    "tls-addr",
				Value:   ":1443",
				Sources: cli.EnvVars("TLS_ADDR", "TLS_LISTEN_ADDR"),
			},
			&cli.StringFlag{
				Name:    "cert-file",
				Sources: cli.EnvVars("TLS_CERT_PATH"),
			},
			&cli.StringFlag{
				Name:    "key-file",
				Sources: cli.EnvVars("TLS_KEY_PATH"),
			},
			&cli.StringFlag{
				Name:    "log-level",
				Sources: cli.EnvVars("LOG_LEVEL"),
				Value:   "debug",
			},
			&cli.StringFlag{
				Name:    "parameter-source",
				Sources: cli.EnvVars("PARAMETER_SOURCE"),
				Value:   "ssm",
			},
			// G101: the default is the local development database
			// created by "mage sql:db", which is the only place
			// this password reaches.
			//nolint:gosec
			&cli.StringFlag{
				Name:    "db",
				Value:   "postgres://elephant-spell:pass@localhost/elephant-spell",
				Sources: cli.EnvVars("CONN_STRING"),
			},
			&cli.StringFlag{
				Name:    "db-parameter",
				Sources: cli.EnvVars("CONN_STRING_PARAMETER"),
			},
			&cli.StringFlag{
				Name:    "db-bouncer",
				Sources: cli.EnvVars("BOUNCER_CONN_STRING"),
			},
			&cli.IntFlag{
				Name:    "db-max-conns",
				Sources: cli.EnvVars("DB_MAX_CONNS"),
				Value:   DefaultDBMaxConns,
				Usage: "Maximum size of the Postgres connection pool used for" +
					" queries. Overrides pool_max_conns in the connection string." +
					" Zero or less leaves the pool to size itself, which means" +
					" max(4, NumCPU()) read from the node's cpuset. With a bouncer" +
					" configured the direct pool is fixed at 2 and this applies" +
					" to the bouncer pool.",
			},
			&cli.StringSliceFlag{
				Name:    "cors-host",
				Usage:   "CORS hosts to allow, supports wildcards",
				Sources: cli.EnvVars("CORS_HOSTS"),
			},
			&cli.DurationFlag{
				Name:    "ping-interval",
				Usage:   "How often to send listener ping notifications",
				Sources: cli.EnvVars("PING_INTERVAL"),
				Value:   5 * time.Minute,
			},
			&cli.DurationFlag{
				Name:    "ping-grace",
				Usage:   "How long to wait for a ping before reconnecting the listener",
				Sources: cli.EnvVars("PING_GRACE"),
				Value:   7 * time.Minute,
			},
			&cli.StringFlag{
				Name:     "oidc-provider",
				Sources:  cli.EnvVars("OIDC_PROVIDER"),
				Usage:    "OIDC provider URL for the dictionary management UI",
				Required: true,
			},
			&cli.StringFlag{
				Name:    "oidc-issuer",
				Sources: cli.EnvVars("OIDC_ISSUER"),
				Usage:   "OIDC issuer URL (optional, for validating tokens from a different issuer)",
			},
			&cli.StringFlag{
				Name:     "client-id",
				Sources:  cli.EnvVars("CLIENT_ID"),
				Required: true,
			},
			&cli.StringFlag{
				Name:     "client-secret",
				Sources:  cli.EnvVars("CLIENT_SECRET"),
				Required: true,
			},
			&cli.StringFlag{
				Name:     "callback-url",
				Sources:  cli.EnvVars("CALLBACK_URL"),
				Value:    "http://localhost:1080/auth/callback",
				Required: true,
			},
			&cli.StringFlag{
				Name:    "default-language",
				Sources: cli.EnvVars("DEFAULT_LANGUAGE"),
				Usage:   "Language to redirect to from the root page",
				Value:   "sv-se",
			},
			&cli.BoolFlag{
				Name:    "insecure-cookies",
				Sources: cli.EnvVars("INSECURE_COOKIES"),
				Usage: "Drop the Secure attribute from session cookies," +
					" needed when serving the UI over plain HTTP locally",
			},
		},
	}

	runCmd.Flags = append(runCmd.Flags, elephantine.AuthenticationCLIFlags()...)

	app := cli.Command{
		Name:  "spell",
		Usage: "The Elephant spelling service",
		Commands: []*cli.Command{
			&runCmd,
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		slog.Error("failed to run application",
			elephantine.LogKeyError, err)
		os.Exit(1)
	}
}

func runSpell(ctx context.Context, c *cli.Command) error {
	var (
		addr              = c.String("addr")
		profileAddr       = c.String("profile-addr")
		tlsAddr           = c.String("tls-addr")
		certFile          = c.String("cert-file")
		keyFile           = c.String("key-file")
		logLevel          = c.String("log-level")
		corsHosts         = c.StringSlice("cors-host")
		connString        = c.String("db")
		bouncerConnString = c.String("db-bouncer")
		dbMaxConns        = c.Int("db-max-conns")
		pingInterval      = c.Duration("ping-interval")
		pingGrace         = c.Duration("ping-grace")
		oidcProviderURL   = c.String("oidc-provider")
		oidcIssuer        = c.String("oidc-issuer")
		clientID          = c.String("client-id")
		clientSecret      = c.String("client-secret")
		callbackURL       = c.String("callback-url")
		defaultLanguage   = c.String("default-language")
	)

	logger := elephantine.SetUpLogger(logLevel, os.Stdout)

	defer func() {
		if p := recover(); p != nil {
			slog.ErrorContext(ctx, "panic during setup",
				elephantine.LogKeyError, p,
				"stack", string(debug.Stack()),
			)

			os.Exit(2)
		}
	}()

	// LISTEN cannot go through PgBouncer in transaction pooling mode, so the
	// subscriber always runs on the direct pool. With a bouncer configured
	// everything else goes through it and the direct pool is kept small;
	// without one the direct pool is the only pool.
	useBouncer := bouncerConnString != "" && bouncerConnString != connString

	pubsubMaxConns := dbMaxConns
	if useBouncer {
		pubsubMaxConns = ListenPoolMaxConns
	}

	pubsubPool, err := newPool(ctx, connString, pubsubMaxConns)
	if err != nil {
		return fmt.Errorf("pubsub database: %w", err)
	}

	defer func() {
		// Don't block for close
		go pubsubPool.Close()
	}()

	dbpool := pubsubPool

	if useBouncer {
		dbpool, err = newPool(ctx, bouncerConnString, dbMaxConns)
		if err != nil {
			return fmt.Errorf("bouncer database: %w", err)
		}

		defer func() {
			go dbpool.Close()
		}()
	}

	logger.InfoContext(ctx, "created connection pools",
		"max_conns", dbMaxConns,
		"direct_max_conns", pubsubMaxConns,
		"bouncer", useBouncer)

	// The pubsub pool doubles as the main pool when no bouncer is
	// configured, and is only registered on its own when it is separate.
	poolMetrics := elephantine.NewMetricsHelper(prometheus.DefaultRegisterer)

	poolMetrics.Collector("main",
		pg.NewPoolStatCollector(dbpool, "main"))

	if pubsubPool != dbpool {
		poolMetrics.Collector("pubsub",
			pg.NewPoolStatCollector(pubsubPool, "pubsub"))
	}

	err = poolMetrics.Err()
	if err != nil {
		return fmt.Errorf("register pool metrics: %w", err)
	}

	auth, err := elephantine.AuthenticationConfigFromCLI(
		ctx, c, nil)
	if err != nil {
		return fmt.Errorf("set up authentication: %w", err)
	}

	params := internal.Parameters{
		Addr:            addr,
		ProfileAddr:     profileAddr,
		TLSAddr:         tlsAddr,
		CertFile:        certFile,
		KeyFile:         keyFile,
		Logger:          logger,
		Database:        dbpool,
		PubsubDatabase:  pubsubPool,
		AuthInfoParser:  auth.AuthParser,
		Registerer:      prometheus.DefaultRegisterer,
		CORSHosts:       corsHosts,
		PingInterval:    pingInterval,
		PingGrace:       pingGrace,
		DefaultLanguage: defaultLanguage,
		InsecureCookies: c.Bool("insecure-cookies"),
	}

	// The keyring seals the session and post-login redirect cookies, and
	// is read from COOKIE_KEY_* — howdah.DefaultCookieKeyPrefix — so that
	// one secret naming convention holds across the fleet.
	keyring, err := howdah.CookieKeyringFromEnv(
		howdah.WithCookieKeyLogger(logger))
	if err != nil {
		return fmt.Errorf("read cookie keyring: %w", err)
	}

	params.CookieKeyring = keyring

	provider, err := oidc.NewProvider(ctx, oidcProviderURL)
	if err != nil {
		return fmt.Errorf("create OIDC provider: %w", err)
	}

	verifierConfig := &oidc.Config{ClientID: clientID}

	if oidcIssuer != "" {
		verifierConfig.SkipIssuerCheck = true
	}

	verifier := provider.Verifier(verifierConfig)

	params.OIDCProvider = provider
	params.OIDCVerifier = verifier
	params.OIDCConfig = &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  callbackURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email", internal.ScopeSpellcheckWrite},
	}

	params.Templates = mustSubFS(spell.TemplateFS, "templates")
	params.Locales = mustSubFS(spell.LocaleFS, "locales")
	params.Assets = mustSubFS(spell.AssetFS, "assets")
	params.Docs = docs.FS

	app, err := internal.NewApplication(ctx, params)
	if err != nil {
		return fmt.Errorf("create application: %w", err)
	}

	err = app.Run(ctx)
	if err != nil {
		return fmt.Errorf("run application: %w", err)
	}

	return nil
}

// newPool creates a connection pool and verifies that the database answers.
// A positive maxConns sizes the pool; zero or less leaves that to the
// connection string or pgx.
func newPool(
	ctx context.Context, connString string, maxConns int,
) (*pgxpool.Pool, error) {
	conf, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}

	if maxConns > math.MaxInt32 {
		return nil, fmt.Errorf("max conns %d exceeds %d",
			maxConns, math.MaxInt32)
	}

	if maxConns > 0 {
		conf.MaxConns = int32(maxConns)
	}

	pool, err := pgxpool.NewWithConfig(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	err = pool.Ping(ctx)
	if err != nil {
		pool.Close()

		return nil, fmt.Errorf("connect to database: %w", err)
	}

	return pool, nil
}

func mustSubFS(f fs.FS, directory string) fs.FS {
	s, err := fs.Sub(f, directory)
	if err != nil {
		panic(fmt.Errorf("create %q sub FS from embedded fs: %w",
			directory, err))
	}

	return s
}

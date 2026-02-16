package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"runtime/debug"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephant-spell/internal"
	"github.com/ttab/elephantine"
	"github.com/urfave/cli/v3"
)

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
				Sources: cli.EnvVars("TLS_CERT"),
			},
			&cli.StringFlag{
				Name:    "key-file",
				Sources: cli.EnvVars("TLS_KEY"),
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
		pingInterval      = c.Duration("ping-interval")
		pingGrace         = c.Duration("ping-grace")
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

	pubsubPool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return fmt.Errorf("create pubsub connection pool: %w", err)
	}

	defer func() {
		// Don't block for close
		go pubsubPool.Close()
	}()

	err = pubsubPool.Ping(ctx)
	if err != nil {
		return fmt.Errorf("connect to pubsub database: %w", err)
	}

	dbpool := pubsubPool

	if bouncerConnString != "" && bouncerConnString != connString {
		dbpool, err = pgxpool.New(ctx, bouncerConnString)
		if err != nil {
			return fmt.Errorf("create bouncer connection pool: %w", err)
		}

		defer func() {
			go dbpool.Close()
		}()

		err = dbpool.Ping(ctx)
		if err != nil {
			return fmt.Errorf("connect to bouncer database: %w", err)
		}
	}

	auth, err := elephantine.AuthenticationConfigFromCLI(
		ctx, c, nil)
	if err != nil {
		return fmt.Errorf("set up authentication: %w", err)
	}

	app, err := internal.NewApplication(ctx, internal.Parameters{
		Addr:           addr,
		ProfileAddr:    profileAddr,
		TLSAddr:        tlsAddr,
		CertFile:       certFile,
		KeyFile:        keyFile,
		Logger:         logger,
		Database:       dbpool,
		PubsubDatabase: pubsubPool,
		AuthInfoParser: auth.AuthParser,
		Registerer:     prometheus.DefaultRegisterer,
		CORSHosts:      corsHosts,
		PingInterval:   pingInterval,
		PingGrace:      pingGrace,
	})
	if err != nil {
		return fmt.Errorf("create application: %w", err)
	}

	err = app.Run(ctx)
	if err != nil {
		return fmt.Errorf("run application: %w", err)
	}

	return nil
}

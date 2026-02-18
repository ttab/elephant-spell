package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/ttab/clitools"
	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/elephantine"
	"github.com/urfave/cli/v3"
	"golang.org/x/oauth2"
)

func main() {
	err := clitools.LoadEnv("spell-client")
	if err != nil {
		slog.Error("load environment config",
			"err", err)
		os.Exit(1)
	}

	uploadCSVCmd := cli.Command{
		Name:   "upload-csv",
		Action: uploadCSV,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:      "file",
				Usage:     "CSV file",
				Required:  true,
				TakesFile: true,
			},
			&cli.StringFlag{
				Name:  "language",
				Value: "sv-se",
			},
		},
	}

	app := cli.Command{
		Name:  "spell-client",
		Usage: "The spell client",
		Commands: []*cli.Command{
			&uploadCSVCmd,
		},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "env",
				Usage:   "Environment (local/stage/prod)",
				Sources: cli.EnvVars("ENV"),
				Value:   "stage",
			},
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		println("error: ", err.Error())
		os.Exit(1)
	}
}

func uploadCSV(ctx context.Context, c *cli.Command) (outErr error) {
	var (
		env  = c.String("env")
		file = c.String("file")
		lang = c.String("language")
	)

	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}

	defer elephantine.Close("csv file", f, &outErr)

	clients, err := getClients(ctx, env)
	if err != nil {
		return err
	}

	reader := csv.NewReader(f)

	_, err = reader.Read()
	if err != nil {
		return fmt.Errorf("skip header row: %w", err)
	}

	for {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("read CSV row: %w", err)
		}

		if len(row) != 3 || row[0] == "" {
			continue
		}

		correct := row[0]
		wrong := row[1]
		comment := row[2]

		var mistakes []string

		for m := range strings.SplitSeq(wrong, " | ") {
			m = strings.TrimSpace(m)
			if m != "" {
				mistakes = append(mistakes, m)
			}
		}

		slog.Info("setting entry",
			"correct", correct,
			"mistakes", mistakes,
			"comment", comment,
		)

		_, err = clients.Dictionaries.SetEntry(ctx, &spell.SetEntryRequest{
			Entry: &spell.CustomEntry{
				Language:       lang,
				Text:           correct,
				Status:         "accepted",
				CommonMistakes: mistakes,
				Description:    comment,
			},
		})
		if err != nil {
			return fmt.Errorf("save entry %q: %w", correct, err)
		}
	}

	return nil
}

type spellClients struct {
	Env          string
	Spellcheck   spell.Check
	Dictionaries spell.Dictionaries
}

func getClients(
	ctx context.Context,
	env string,
) (*spellClients, error) {
	conf, err := clitools.NewConfigurationHandler(
		"spell-client", clitools.DefaultApplicationID, env)
	if err != nil {
		return nil, fmt.Errorf("load configuration: %w", err)
	}

	endpoint, ok := conf.GetEndpoint("spell")
	if !ok {
		return nil, fmt.Errorf("no spell endpoint for %q", env)
	}

	token, err := conf.GetAccessToken(ctx, []string{
		"spell_write",
	})
	if err != nil {
		return nil, fmt.Errorf("get access token: %w", err)
	}

	err = conf.Save()
	if err != nil {
		slog.Warn("failed to save configuration", "err", err)
	}

	client := oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: token.Token,
	}))

	check := spell.NewCheckProtobufClient(endpoint, client)
	dict := spell.NewDictionariesProtobufClient(endpoint, client)

	return &spellClients{
		Env:          env,
		Spellcheck:   check,
		Dictionaries: dict,
	}, nil
}

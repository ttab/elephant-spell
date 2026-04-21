package main

import (
	"bufio"
	"bytes"
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
	"google.golang.org/protobuf/encoding/protojson"
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

	downloadCmd := cli.Command{
		Name:   "download",
		Usage:  "Download a dictionary as newline-delimited JSON",
		Action: downloadDictionary,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "language",
				Usage:    "Language code (e.g. sv-se)",
				Required: true,
			},
		},
	}

	uploadCmd := cli.Command{
		Name:   "upload",
		Usage:  "Upload a dictionary from newline-delimited JSON",
		Action: uploadDictionary,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:      "file",
				Usage:     "Newline-delimited JSON file",
				Required:  true,
				TakesFile: true,
			},
		},
	}

	app := cli.Command{
		Name:  "spell-client",
		Usage: "The spell client",
		Commands: []*cli.Command{
			&uploadCSVCmd,
			&downloadCmd,
			&uploadCmd,
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

func downloadDictionary(ctx context.Context, c *cli.Command) error {
	var (
		env  = c.String("env")
		lang = c.String("language")
	)

	clients, err := getClients(ctx, env)
	if err != nil {
		return err
	}

	marshaler := protojson.MarshalOptions{}

	var n int

	for page := int64(0); ; page++ {
		res, err := clients.Dictionaries.ListEntries(ctx,
			&spell.ListEntriesRequest{
				Language: lang,
				Page:     page,
			})
		if err != nil {
			return fmt.Errorf("list entries page %d: %w", page, err)
		}

		if len(res.Entries) == 0 {
			break
		}

		for _, entry := range res.Entries {
			data, err := marshaler.Marshal(entry)
			if err != nil {
				return fmt.Errorf("marshal entry %q: %w",
					entry.Text, err)
			}

			_, err = fmt.Fprintf(os.Stdout, "%s\n", data)
			if err != nil {
				return fmt.Errorf("write entry %q: %w",
					entry.Text, err)
			}

			n++
		}
	}

	slog.Info("download complete",
		"language", lang,
		"entries", n,
	)

	return nil
}

func uploadDictionary(ctx context.Context, c *cli.Command) (outErr error) {
	var (
		env  = c.String("env")
		file = c.String("file")
	)

	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}

	defer elephantine.Close("dictionary file", f, &outErr)

	clients, err := getClients(ctx, env)
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(f)
	unmarshaler := protojson.UnmarshalOptions{
		DiscardUnknown: true,
	}

	var n int

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var entry spell.CustomEntry

		err := unmarshaler.Unmarshal(line, &entry)
		if err != nil {
			return fmt.Errorf("unmarshal line %d: %w", n+1, err)
		}

		slog.Info("setting entry",
			"text", entry.Text,
			"language", entry.Language,
		)

		_, err = clients.Dictionaries.SetEntry(ctx,
			&spell.SetEntryRequest{
				Entry: &entry,
			})
		if err != nil {
			return fmt.Errorf("set entry %q: %w", entry.Text, err)
		}

		n++
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	slog.Info("upload complete", "entries", n)

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

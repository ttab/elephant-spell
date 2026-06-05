// Package integration provides the shared test harness for elephant-spell
// integration tests. It boots a Postgres + Elsinod (mock OIDC) docker stack
// via eltest, creates a per-test database with the spell migrations applied,
// and runs the real spell server (Application.Run) so tests exercise the
// production server setup and full API surface over the wire.
//
// The harness lives in a non-test package so multiple test files can share it.
// It only depends on eltest, which pulls in dockertest; production builds that
// don't import this package don't pull docker.
package integration

import (
	"context"

	"github.com/ttab/elephant-spell/schema"
	"github.com/ttab/eltest"
)

// T is the subset of *testing.T the harness needs. testing.T from Go 1.24+
// satisfies it via Context().
type T interface {
	Name() string
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(fn func())
	Context() context.Context
}

// Environment is the resolved backing-service environment.
type Environment struct {
	PostgresURI string
	Elsinod     *Elsinod
}

// SetUpBackingServices boots Postgres and Elsinod (shared across the test
// process), creates a per-test database with the spell migrations applied, and
// returns the resolved endpoints. Containers are torn down at process exit via
// eltest.PurgeBackingServices (TestMain).
func SetUpBackingServices(t T) Environment {
	t.Helper()

	pg := eltest.NewPostgres(eltestT{t}, eltest.Postgres17_6)
	elsinod := NewElsinod(t, ElsinodConfig{PublicURL: "http://elsinod.test"})

	pgEnv := pg.Database(eltestT{t}, "spell", schema.Migrations, true)

	return Environment{
		PostgresURI: pgEnv.PostgresURI,
		Elsinod:     elsinod,
	}
}

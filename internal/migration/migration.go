package migration

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

// Up applies every explicit SQL migration at path.
func Up(databaseURL, path string) error {
	runner, err := newRunner(databaseURL, path)
	if err != nil {
		return err
	}
	defer runner.Close()
	if err := runner.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// To migrates to an exact version. It exists for upgrade and reversible
// migration acceptance tests; production startup should continue to call Up.
func To(databaseURL, path string, version uint) error {
	runner, err := newRunner(databaseURL, path)
	if err != nil {
		return err
	}
	defer runner.Close()
	if err := runner.Migrate(version); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate to version %d: %w", version, err)
	}
	return nil
}

func newRunner(databaseURL, path string) (*migrate.Migrate, error) {
	if databaseURL == "" || path == "" {
		return nil, errors.New("database URL and migration path are required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve migration path: %w", err)
	}
	sourceURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolutePath)}).String()
	runner, err := migrate.New(sourceURL, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("create migration runner: %w", err)
	}
	return runner, nil
}

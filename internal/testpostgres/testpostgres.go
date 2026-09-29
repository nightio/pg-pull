package testpostgres

import (
	"context"
	"os"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// DSN uses an explicitly configured database, or starts an isolated PostgreSQL
// container for this test. Containers are cleaned up even when the test fails.
func DSN(t *testing.T, envName, image string) string {
	t.Helper()
	if dsn := os.Getenv(envName); dsn != "" {
		return dsn
	}
	ctx := context.Background()
	container, err := postgres.Run(ctx, image,
		postgres.WithDatabase("pgpull_test"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("secret"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start %s: %v", image, err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Errorf("stop %s: %v", image, err)
		}
	})
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string for %s: %v", image, err)
	}
	return dsn
}

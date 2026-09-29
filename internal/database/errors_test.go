package database

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nightio/pg-pull/internal/config"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestConnectionMessages(t *testing.T) {
	db := config.Database{Host: "localhost", Port: 5432, DBName: "app", User: "reader", Password: "secret", ConnectTimeout: "250ms"}
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"password", &pgconn.PgError{Code: "28P01", Message: "secret"}, "Password authentication failed"},
		{"fallback", errors.Join(errors.New("tls error: server refused TLS connection secret"), &pgconn.PgError{Code: "28P01", Message: "secret"}), "Password authentication failed"},
		{"access", &pgconn.PgError{Code: "28000", Message: "secret"}, "authentication rules"},
		{"permission", &pgconn.PgError{Code: "42501", Message: "secret"}, "CONNECT permission"},
		{"database", &pgconn.PgError{Code: "3D000"}, "Database does not exist"},
		{"connections", &pgconn.PgError{Code: "53300"}, "no free connections"},
		{"timeout", fmt.Errorf("dial: %w", context.DeadlineExceeded), "connect_timeout: 250ms"},
		{"cancel", context.Canceled, "Connection cancelled"},
		{"dns", &net.DNSError{Err: "secret", Name: "unknown", IsNotFound: true}, "Cannot resolve"},
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "Connection refused"},
		{"tls", errors.New("tls error: server refused TLS connection secret"), "TLS connection failed"},
		{"unknown", errors.New("secret"), "Connection failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := connectionError(db, "source", tt.err)
			message := UserMessage(fmt.Errorf("open: %w", err))
			if !strings.Contains(message, tt.want) || strings.Contains(message, "secret") {
				t.Fatalf("unexpected message: %s", message)
			}
			if !errors.Is(err, tt.err) {
				t.Fatal("lost original error")
			}
		})
	}
}

func TestOperationMessages(t *testing.T) {
	for _, tt := range []struct{ code, want string }{
		{"55P03", "lock_timeout"}, {"42501", "Permission denied"}, {"40P01", "deadlock"}, {"57014", "query cancelled"},
	} {
		err := fmt.Errorf("restore table items: %w", &pgconn.PgError{Code: tt.code, Message: "server detail"})
		message := UserMessage(err)
		if !strings.HasPrefix(message, "restore table items: ") || !strings.Contains(message, tt.want) || strings.Contains(message, "server detail") {
			t.Fatal(message)
		}
	}
}

func TestConnectionSettings(t *testing.T) {
	db := config.Database{Host: "localhost", Port: 5432, DBName: "app", User: "reader", ConnectTimeout: "1500ms", LockTimeout: "250ms"}
	for _, readOnly := range []bool{false, true} {
		cfg, err := parseConnection(db, "secret", readOnly)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ConnectTimeout != 1500*time.Millisecond || cfg.RuntimeParams["lock_timeout"] != "250" {
			t.Fatal("timeouts not applied")
		}
		if readOnly && cfg.RuntimeParams["default_transaction_read_only"] != "on" {
			t.Fatal("read-only protection lost")
		}
	}
	db.SSLMode = "invalid"
	_, err := parseConnection(db, "secret", true)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe parse error: %v", err)
	}
}

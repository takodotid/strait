package plugin

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The checker needs its credentials, fails at once on a refused connection within its time limit, and never puts the
// password into an error.
func TestPGRoleChecker(t *testing.T) {
	ctx := context.Background()
	if _, err := (PGRoleChecker{}).InRecovery(ctx, "127.0.0.1", 5432); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("no directory: %v, want ErrNoCredentials", err)
	}
	dir := t.TempDir()
	if _, err := (PGRoleChecker{Dir: dir}).InRecovery(ctx, "127.0.0.1", 5432); err == nil || !strings.Contains(err.Error(), "user name") {
		t.Errorf("no files: %v", err)
	}
	const password = "s3cret-pass"
	for name, value := range map[string]string{"username": "strait_role_check\n", "password": password + "\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A port that refuses: a listener closed at once.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := int32(l.Addr().(*net.TCPAddr).Port)
	l.Close()
	start := time.Now()
	_, err = (PGRoleChecker{Dir: dir}).InRecovery(ctx, "127.0.0.1", port)
	if err == nil {
		t.Fatal("a refused connection gave no error")
	}
	if took := time.Since(start); took > RoleCheckTimeout+time.Second {
		t.Errorf("the check took %s", took)
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("the error holds the password: %v", err)
	}
	if got := redact(errors.New("auth failed for "+password), password).Error(); strings.Contains(got, password) {
		t.Errorf("redact left the password: %s", got)
	}
}

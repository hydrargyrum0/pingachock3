package serveragent

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsUniqueViolation(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unique_violation", &pgconn.PgError{Code: "23505"}, true},
		{"wrapped unique_violation", fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505"}), true},
		{"different pg error code", &pgconn.PgError{Code: "23503"}, false}, // foreign_key_violation
		{"unrelated error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUniqueViolation(tc.err); got != tc.want {
				t.Errorf("isUniqueViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

package repository

import (
	"errors"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

func TestHandleErrorMapping(t *testing.T) {
	require.Nil(t, handleError(nil))
	sentinel := errors.New("other")
	require.ErrorIs(t, handleError(sentinel), sentinel)

	require.ErrorIs(t, handleError(pgx.ErrNoRows), ErrNotFound)
	require.ErrorIs(t, handleError(&pgconn.PgError{Code: pgerrcode.ForeignKeyViolation}), ErrNotFound)
	require.ErrorIs(t, handleError(&pgconn.PgError{Code: pgerrcode.UniqueViolation}), ErrAlreadyExists)
}

package tokens

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore lee de la tabla res_users de Odoo SOLO las columnas que necesita. El rol de
// Postgres del gateway tiene `GRANT SELECT (id, active, moodle_username, moodle_token_enc)`
// y nada más: no puede ver los hashes de contraseña ni ninguna otra columna (ver sql/grants.sql).
type PGStore struct {
	pool *pgxpool.Pool
}

func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

func (s *PGStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// userID convierte el identificador de usuario (id de res.users en Odoo) a entero.
func userID(user string) (int64, error) {
	id, err := strconv.ParseInt(user, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("tokens: identificador de usuario no válido: %q", user)
	}
	return id, nil
}

// Importante: la consulta solo nombra columnas con permiso. Un "SELECT *" fallaría.
const selectSQL = `
SELECT COALESCE(moodle_token_enc, ''),
       COALESCE(moodle_username, ''),
       COALESCE(active, false)
  FROM res_users
 WHERE id = $1`

func (s *PGStore) Get(ctx context.Context, user string) (Row, bool, error) {
	id, err := userID(user)
	if err != nil {
		return Row{}, false, err
	}
	var row Row
	err = s.pool.QueryRow(ctx, selectSQL, id).Scan(&row.TokenEnc, &row.MoodleUsername, &row.Active)
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, false, nil
	}
	if err != nil {
		return Row{}, false, fmt.Errorf("tokens: leyendo res_users: %w", err)
	}
	return row, true, nil
}

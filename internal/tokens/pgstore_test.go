package tokens

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Test de integración contra Postgres. Solo se ejecuta si se define TEST_ODOO_DSN, y debe
// apuntar a una base de datos DESECHABLE (NO a la de Odoo): crea una tabla res_users mínima
// con las columnas que lee el gateway, solo si no existe.
//
//	TEST_ODOO_DSN=postgres://user:pass@localhost:5432/test go test ./internal/tokens -run PGStore
const testDDL = `
CREATE TABLE IF NOT EXISTS res_users (
    id SERIAL PRIMARY KEY,
    active BOOLEAN,
    moodle_username VARCHAR,
    moodle_token_enc TEXT
);`

func TestPGStore(t *testing.T) {
	dsn := os.Getenv("TEST_ODOO_DSN")
	if dsn == "" {
		t.Skip("define TEST_ODOO_DSN (BD desechable) para ejecutar este test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, testDDL); err != nil {
		t.Fatal(err)
	}

	var withToken, withoutToken, inactive int
	insert := func(dst *int, active bool, username, token any) {
		err := pool.QueryRow(ctx,
			`INSERT INTO res_users (active, moodle_username, moodle_token_enc) VALUES ($1, $2, $3) RETURNING id`,
			active, username, token).Scan(dst)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(&withToken, true, "12345678z", "v1.ABC")
	insert(&withoutToken, true, nil, nil) // columnas a NULL, como las deja Odoo
	insert(&inactive, false, "x", "v1.DEF")
	defer pool.Exec(ctx, `DELETE FROM res_users WHERE id = ANY($1)`, []int{withToken, withoutToken, inactive})

	s := NewPGStore(pool)
	get := func(id int) (Row, bool) {
		row, ok, err := s.Get(ctx, strconv.Itoa(id))
		if err != nil {
			t.Fatal(err)
		}
		return row, ok
	}

	if row, ok := get(withToken); !ok || row.TokenEnc != "v1.ABC" || row.MoodleUsername != "12345678z" || !row.Active {
		t.Fatalf("con token: %+v ok=%v", row, ok)
	}
	if row, ok := get(withoutToken); !ok || row.TokenEnc != "" || row.MoodleUsername != "" {
		t.Fatalf("NULL debe leerse como vacío: %+v ok=%v", row, ok)
	}
	if row, ok := get(inactive); !ok || row.Active {
		t.Fatalf("inactivo: %+v ok=%v", row, ok)
	}
	if _, ok := get(999999999); ok {
		t.Fatal("un id inexistente no debería encontrarse")
	}
	if _, _, err := s.Get(ctx, "no-es-un-id"); err == nil {
		t.Fatal("un identificador no numérico debe dar error")
	}
}

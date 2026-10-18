#!/bin/sh
# Solo para desarrollo (compose.yaml): crea una res_users mínima como la de Odoo y aplica
# sql/grants.sql. Lo ejecuta la imagen de postgres la primera vez que arranca con el volumen vacío.
set -eu

psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" <<'SQL'
CREATE TABLE IF NOT EXISTS res_users (
    id SERIAL PRIMARY KEY,
    login VARCHAR NOT NULL,
    password VARCHAR,
    active BOOLEAN,
    moodle_username VARCHAR,
    moodle_token_enc TEXT
);
-- Mismos ids que en una instalación de Odoo: 1 = __system__ (inactivo), 2 = admin.
INSERT INTO res_users (login, active) VALUES ('__system__', false), ('admin', true), ('profesor', true);
SQL

psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
     -v gateway_password="$GATEWAY_DB_PASSWORD" -f /sql/grants.sql

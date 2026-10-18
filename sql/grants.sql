-- Rol de Postgres del gateway: SOLO LECTURA de 4 columnas de res_users.
-- No puede ver contraseñas ni ninguna otra columna o tabla de Odoo, ni escribir nada.
--
-- Ejecutar como propietario de la BD de Odoo (después de instalar el módulo que añade
-- moodle_username y moodle_token_enc a res_users):
--   psql -d odoo -v gateway_password='...' -f sql/grants.sql

CREATE ROLE moodle_gateway LOGIN PASSWORD :'gateway_password';

GRANT CONNECT ON DATABASE :"DBNAME" TO moodle_gateway;
GRANT USAGE ON SCHEMA public TO moodle_gateway;

-- Permisos por columna: un "SELECT *" del gateway fallaría a propósito.
GRANT SELECT (id, active, moodle_username, moodle_token_enc) ON res_users TO moodle_gateway;

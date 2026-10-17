# maya-moodlink

Gateway HTTP, escrito en Go, entre **Odoo (Maya)** y **Moodle (Aules)**. Odoo es su único cliente: llama a la API del gateway indicando en nombre de qué usuario actúa, y el gateway habla con los web services REST de Moodle usando el token de Moodle de ese usuario.

```
 ┌──────────┐  Bearer API_KEY       ┌───────────────┐  wstoken (POST)    ┌──────────┐
 │   Odoo   │  X-Maya-User: <id>    │ maya-moodlink │ ─────────────────▶ │  Moodle  │
 │  (Maya)  │ ────────────────────▶ │   (gateway)   │                    │ (Aules)  │
 └──────────┘                       └───────────────┘                    └──────────┘
      │                                │  SELECT (solo 4 columnas)
      │  escribe el token cifrado      ▼
      └──────────────────────────▶ res_users (BD de Odoo)
```

Objetivos del diseño:

- **Odoo nunca ve un token de Moodle en claro.** Los tokens se guardan cifrados (AES-256-GCM) y solo el gateway tiene la clave.
- **El gateway no escribe en la base de datos.** Lee cuatro columnas de `res_users`; quien escribe es Odoo, con su ORM.
- **Solo transporte.** El gateway traduce peticiones y errores. La lógica de negocio (qué hacer con una entrega, cuándo pedir la contraseña…) está en Odoo.
- **Protege a Moodle.** Reutiliza conexiones, limita la concurrencia y solo reintenta las operaciones idempotentes.

---

## Requisitos

### 1. Moodle

- Web services REST activados (*Administración del sitio → Servidor → Servicios web*).
- Un servicio web cuyo *shortname* se indica en `MOODLE_SERVICE` (por defecto `moodle_mobile_app`). Tiene que incluir las funciones que usa el gateway:

  | Función de Moodle               | La usa                                       |
  |---------------------------------|----------------------------------------------|
  | `core_webservice_get_site_info` | `POST /v1/users/{user}/token`                |
  | `mod_assign_get_submissions`    | `GET /v1/assignments/{id}/submissions`       |
  | `core_user_get_users_by_field`  | `GET /v1/assignments/{id}/submissions`       |

  Al añadir un endpoint nuevo hay que comprobar que su función está en el servicio. Si no lo está, Moodle responde con `accessexception` y el gateway devuelve **403**.
- Los usuarios (profesores y cuentas de servicio) necesitan en Moodle los permisos de la operación que hagan. Por ejemplo, para listar las entregas de una tarea, permiso para calificarla en ese curso.

### 2. Base de datos de Odoo

El gateway se conecta a la base de datos de Odoo con un rol propio, de **solo lectura** y limitado a las columnas que necesita:

```sql
CREATE ROLE maya_gateway LOGIN PASSWORD '...';
GRANT CONNECT ON DATABASE odoo TO maya_gateway;
GRANT USAGE ON SCHEMA public TO maya_gateway;
GRANT SELECT (id, active, moodle_username, moodle_token_enc) ON res_users TO maya_gateway;
```

Así el gateway no puede leer los hashes de contraseña ni ninguna otra columna. Las consultas nombran las columnas una a una: un `SELECT *` fallaría por falta de permisos, y está hecho a propósito. Las columnas `moodle_username` y `moodle_token_enc` las crea el addon `maya_moodle`.

### 3. Odoo (addon `maya_moodle`)

- *Ajustes → Maya | Moodle → URL del gateway* (parámetro `maya_moodle.gateway_url`), por ejemplo `http://maya-moodlink:8080`.
- Variable de entorno `MAYA_GATEWAY_API_KEY` en el contenedor de Odoo, con el mismo valor que `API_KEY` del gateway.
- Para los procesos automáticos: *Ajustes → Maya | Moodle → Usuario de servicio* (parámetro `maya_moodle.service_user_id`). Ese usuario tiene que ser una cuenta de servicio del gateway (ver más abajo).

### 4. Configuración del gateway

Se configura solo con variables de entorno (ver `.env.example`). `config.Load` valida todas y muestra todos los errores juntos.

| Variable                   | Obligatoria | Descripción |
|----------------------------|:-----------:|-------------|
| `MOODLE_URL`               | sí | URL base de Moodle (http/https). |
| `API_KEY`                  | sí | Clave que Odoo presenta en `Authorization: Bearer`. Mínimo 16 caracteres (`openssl rand -hex 32`). |
| `ODOO_DB_DSN`              | sí | DSN de Postgres con el rol de solo lectura. |
| `TOKEN_ENC_KEYS`           | sí | Clave(s) de cifrado de los tokens: `v2:<base64>,v1:<base64>`. La primera cifra y todas descifran. |
| `SERVICE_CREDENTIALS_FILE` | no | Fichero JSON con las contraseñas de las cuentas de servicio. Vacío significa que no hay ninguna. |
| `MOODLE_SERVICE`           | no | *Shortname* del servicio web (`moodle_mobile_app`). |
| `LISTEN_ADDR`              | no | `:8080` |
| `REQUEST_TIMEOUT`          | no | Tiempo máximo de cada petición (`30s`). |
| `MOODLE_MAX_CONCURRENCY`   | no | Peticiones simultáneas máximas hacia Moodle (`30`). |
| `MOODLE_ATTEMPT_TIMEOUT`   | no | Tiempo máximo de cada intento contra Moodle (`15s`). |
| `MOODLE_MAX_RETRIES`       | no | Reintentos extra, solo en operaciones idempotentes (`2`). |
| `ODOO_DB_MAX_CONNS`        | no | Conexiones máximas a la BD de Odoo (`10`). |
| `LOG_LEVEL`                | no | `debug`, `info`, `warn` o `error`. |

**Rotación de la clave de cifrado.** Pon la clave nueva delante y conserva la antigua: `TOKEN_ENC_KEYS=v2:<nueva>,v1:<antigua>`. Los tokens nuevos se cifran con `v2` y los antiguos se siguen descifrando con `v1`, así que nadie tiene que volver a identificarse. La clave antigua se puede retirar cuando ningún token la use.

---

## Autenticación

Hay dos niveles independientes.

### Nivel 1: quién llama al gateway (Odoo)

Todas las rutas `/v1/...` exigen `Authorization: Bearer <API_KEY>`. La comparación se hace en tiempo constante y, si `API_KEY` no está configurada, el gateway responde 503 (falla cerrado).

> ⚠️ Es una clave compartida: quien la tenga puede actuar en nombre de cualquier usuario. Es válido mientras el único cliente sea Odoo y esté en una red de confianza. La autorización por operación o por aula está pendiente.

### Nivel 2: en nombre de quién se habla con Moodle

Cada petición indica el usuario con la cabecera `X-Maya-User` (o con `{user}` en la ruta del endpoint de token). Es el **id de `res.users` de Odoo** en forma canónica (`"7"`, no `"007"`). Con él, el gateway lee la fila de `res_users`: si el usuario no existe o está archivado, devuelve **403 `user_inactive`**.

A partir de ahí hay dos tipos de usuario.

#### Profesores (usuarios normales)

Su token de Moodle está cifrado en `res_users.moodle_token_enc`. **El gateway no lo renueva nunca**, porque no conoce la contraseña del profesor.

```
 Profesor           Odoo                         Gateway                     Moodle
    │  acción         │                              │                           │
    │───────────────▶ │  GET /v1/...  X-Maya-User:7  │                           │
    │                 │ ───────────────────────────▶ │  lee y descifra el token  │
    │                 │                              │ ────────────────────────▶ │
    │                 │  401 moodle_token_missing    │   (sin token, o Moodle    │
    │                 │      / moodle_token_invalid  │    lo rechaza)            │
    │                 │ ◀─────────────────────────── │                           │
    │  formulario     │                              │                           │
    │  /maya/moodle/token                            │                           │
    │ ◀────────────── │                              │                           │
    │  contraseña     │  POST /v1/users/7/token      │                           │
    │───────────────▶ │  {username, password}        │  login/token.php          │
    │                 │ ───────────────────────────▶ │ ────────────────────────▶ │
    │                 │                              │  get_site_info (comprueba)│
    │                 │  200 {token_enc, ...}        │ ────────────────────────▶ │
    │                 │ ◀─────────────────────────── │                           │
    │                 │  guarda token_enc en         │                           │
    │                 │  res_users (ORM)             │                           │
    │  repite la acción ─────────────────────────────▶  ahora con token válido   │
```

- La primera vez, o cuando Moodle invalida el token, el gateway responde **401**. Odoo abre el formulario `/maya/moodle/token`, que es un controlador HTTP y no un *wizard*, para que la contraseña no se guarde en la BD.
- El usuario de Moodle no se le pide al profesor: es su `moodle_username` o, la primera vez, su login de Odoo (DNI/NIE). Así nadie puede identificarse como otra persona.
- El gateway canjea usuario y contraseña en `login/token.php`, comprueba el token con `core_webservice_get_site_info` y lo devuelve **cifrado**, con el id de usuario como dato asociado (AAD). Si alguien copia ese valor en otra fila de `res_users`, no se puede descifrar.
- La contraseña solo pasa por esa petición. No se guarda, no se registra en el log y nunca se reintenta, para no provocar bloqueos de cuenta.
- Odoo borra el token cuando cambia el `moodle_username` del usuario o cuando se archiva.

#### Cuentas de servicio (usuarios técnicos, p. ej. Maya)

Son las que usan los procesos automáticos (crons, n8n), donde no hay nadie a quien pedirle una contraseña. Su contraseña de Moodle está en el fichero `SERVICE_CREDENTIALS_FILE`, indexada por id de usuario de Odoo (en el futuro, Vault):

```json
{ "2": { "password": "..." } }
```

- El fichero tiene que montarse como *secret* (Docker/Kubernetes); **nunca** debe estar en el repositorio.
- El nombre de usuario de Moodle **no** va en el fichero: se lee de `res_users.moodle_username`, que tiene que estar configurado. Si falta, el gateway devuelve **502 `service_account_failed`**: es un fallo de configuración, no algo que el usuario pueda arreglar.
- El token se obtiene la primera vez que hace falta y se guarda **solo en memoria**. Nunca se escribe en la BD, porque siempre se puede volver a generar.
- Si Moodle rechaza el token, el gateway lo renueva y **repite la operación una vez**. Si varias peticiones fallan a la vez, solo una va a Moodle a renovarlo y las demás reutilizan el resultado.

Para dar de alta una cuenta de servicio:

1. En Odoo, crea el usuario técnico (p. ej. *Maya*) y rellena su campo `moodle_username`.
2. Añade su id de Odoo y su contraseña de Moodle a `SERVICE_CREDENTIALS_FILE` y reinicia el gateway.
3. Si es la que van a usar los procesos automáticos de Maya, selecciónala en *Ajustes → Maya | Moodle → Usuario de servicio*.

#### Qué significa cada error para Odoo

La diferencia importante es **401 frente a 403**:

| HTTP | `error.code`                                   | Qué hacer |
|------|------------------------------------------------|-----------|
| 401  | `moodle_token_missing`, `moodle_token_invalid` | Pedir la contraseña al usuario (`token_prompt_action()`). |
| 401  | `unauthenticated`                              | Falta la `API_KEY` o la cabecera `X-Maya-User`, o no son válidas. Es un error de configuración de Odoo. |
| 403  | `moodle_forbidden`                             | Moodle niega el permiso (`moodle_code` indica el motivo). **Volver a identificarse no lo arregla.** |
| 403  | `user_inactive`                                | El usuario no existe o está archivado. |
| 404  | `not_found`                                    | El recurso de Moodle (curso, tarea…) no existe. |
| 400  | `invalid_*`                                    | Parámetros mal formados. |
| 422  | `invalid_credentials`                          | Usuario o contraseña de Moodle incorrectos (solo en el endpoint de token). |
| 502  | `moodle_unavailable`, `moodle_error`, `service_account_failed` | Fallo de Moodle o de configuración de una cuenta de servicio. |
| 503  | `overloaded`                                   | Hay demasiadas peticiones simultáneas hacia Moodle. Reintentar (`Retry-After`). |
| 504  | `upstream_timeout`                             | Moodle o la BD no respondieron a tiempo. |

En Odoo, `maya_moodle.moodlink` los convierte en `MoodleTokenRequired` (401 de token), `MoodleForbidden` (403) y `MoodleGatewayError` (el resto).

---

## API

Todas las respuestas son JSON. Los errores tienen siempre esta forma:

```json
{ "error": { "code": "moodle_forbidden", "message": "...", "moodle_code": "nopermissions" } }
```

Cada respuesta lleva la cabecera `X-Request-ID`, que también aparece en el log del gateway.

### Salud

| Método y ruta  | Auth | Descripción |
|----------------|------|-------------|
| `GET /healthz` | no   | *Liveness*: el proceso responde. No depende de nada externo. |
| `GET /readyz`  | no   | *Readiness*: Moodle y la BD de Odoo responden. Devuelve 503 con `{"failing": ["moodle", "database"]}` si alguno falla. El resultado se cachea 10 s. |

### `POST /v1/users/{user}/token`

Canjea la contraseña de Moodle de un usuario por un token cifrado. Requiere `API_KEY`; el usuario va en la ruta.

```http
POST /v1/users/7/token
Authorization: Bearer <API_KEY>
Content-Type: application/json

{ "username": "12345678A", "password": "..." }
```

```json
{ "token_enc": "v1.AbC...", "moodle_username": "12345678A", "moodle_user_id": 42 }
```

Odoo guarda `token_enc` tal cual en `res_users.moodle_token_enc`. Errores propios: 400 si falta `username` o `password` y 422 `invalid_credentials`.

### `GET /v1/assignments/{assignmentID}/submissions`

Devuelve la última entrega de cada alumno en una tarea de Moodle, con los datos del alumno y los ficheros adjuntos. Las entregas de grupo (`userid` 0) se descartan.

| Parámetro (query) | Descripción |
|-------------------|-------------|
| `status`          | Opcional: `new`, `draft`, `submitted` o `reopened`. Sin él, devuelve todas. |

```http
GET /v1/assignments/359859/submissions?status=submitted
Authorization: Bearer <API_KEY>
X-Maya-User: 2
```

```json
{
  "assignment_id": 359859,
  "count": 1,
  "submissions": [
    {
      "id": 10,
      "user": { "id": 1, "username": "10000001", "firstname": "Ana", "lastname": "Ruiz", "email": "a@x.org" },
      "attempt_number": 0,
      "status": "submitted",
      "grading_status": "notgraded",
      "time_created": 1700000000,
      "time_modified": 1700000500,
      "files": [
        {
          "filename": "convalidaciones.zip", "filepath": "/", "filesize": 1234,
          "mimetype": "application/zip", "time_modified": 1700000400,
          "file_url": "https://.../webservice/pluginfile.php/..."
        }
      ]
    }
  ]
}
```

- `attempt_number` es el de Moodle: **el primer intento es 0**.
- Las fechas son *timestamps* Unix en UTC.
- Si Moodle no devuelve los datos de un usuario (borrado o no visible), `user` solo trae el `id`.
- `files` es siempre una lista (vacía si no hay ficheros).
- `file_url` no sirve sin un token. La descarga se hará a través del gateway (pendiente).
- Errores propios: 400 `invalid_assignment` o `invalid_status`; 403 `moodle_forbidden` sin permiso sobre la tarea; 404 `not_found` si la tarea no existe.

La usa `maya_moodle.moodlink.get_submissions()`, y con ella la sincronización de convalidaciones de `maya_valid`.

### Pendientes

| Ruta | Estado |
|------|--------|
| `GET /v1/courses/{courseID}/students` | Comentada en `server.New`. Los tests de `internal/server` ya la usan y fallan con 404 hasta que se implemente. |
| Descarga de ficheros de entregas | Siguiente fase de convalidaciones. |
| Calificar, *feedback*, nuevo intento, prórrogas y bloqueo de entregas | Fases posteriores. Hay que confirmar que las funciones `mod_assign_*` están en el servicio web. |

### Añadir un endpoint

1. Registra la ruta en `server.New` con `s.auth(s.withUser(...))`.
2. Haz las llamadas a Moodle **dentro** de `s.Auth.Do(ctx, user, func(token string) error {...})`. Así funciona igual para profesores y para cuentas de servicio, incluida la renovación del token.
3. Devuelve los errores **siempre** con `s.writeUpstreamError(w, err)`.
4. Declara la llamada como idempotente (`Call(..., true, ...)`) solo si es segura de repetir.
5. Añade la función de Moodle al servicio web y a la tabla de requisitos de este README.

---

## Desarrollo

1. Descarga las dependencias y ordena `go.mod`:

   ```bash
   go mod tidy
   ```

2. Comprobación completa (análisis estático y tests con detección de condiciones de carrera):

   ```bash
   go mod tidy && go vet ./... && go test -race ./...
   ```

   * `go mod tidy` añade las dependencias que usa el código y no están registradas, y quita las que no se usan.
   * `go vet ./...` es el analizador estático oficial de Go: busca errores sospechosos que el compilador no detecta. `./...` incluye todos los subdirectorios.
   * `go test -race ./...` ejecuta todos los tests detectando condiciones de carrera.

   Un test concreto: `go test ./internal/server -run TestSubmissions`.

   `TestPGStore` es de integración y solo se ejecuta si `TEST_ODOO_DSN` apunta a una BD **desechable** (crea una tabla `res_users` mínima):

   ```bash
   TEST_ODOO_DSN=postgres://user:pass@localhost:5432/test go test ./internal/tokens -run PGStore
   ```

3. Ejecutar:

   ```bash
   set -a; . ./.env; set +a
   go run ./cmd/gateway
   ```

   `go run ./cmd/gateway -healthcheck` consulta `/healthz` en local. Está pensado para el `HEALTHCHECK` de Docker, porque la imagen *distroless* no trae `curl`.

### Estructura

| Paquete            | Responsabilidad |
|--------------------|-----------------|
| `cmd/gateway`      | Cablea todo: configuración, cifrado, BD, credenciales, cliente de Moodle, autenticador y servidor HTTP con apagado suave. |
| `internal/config`  | Carga y valida las variables de entorno. |
| `internal/server`  | API HTTP, *middlewares* (log, *recover*, `API_KEY`, usuario) y traducción de errores. |
| `internal/session` | Decide con qué token se hace cada petición (profesor o cuenta de servicio) y lo renueva cuando toca. |
| `internal/tokens`  | Cifrado AES-256-GCM con rotación de claves y lectura de `res_users`. |
| `internal/moodle`  | Cliente de Moodle: *keep-alive*, semáforo de concurrencia, reintentos con *backoff* y excepciones de Moodle convertidas en errores. |

// Package config carga la configuración del servicio desde variables de entorno (.env)
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"maya-moodlink/internal/tokens"
)

// Config ESTRUCTURA (struct).
// En Go no existen las clases. Usamos 'struct' para definir un molde de datos con varios campos agrupados.
type Config struct {
	ListenAddr     string        // Dirección IP y puerto donde escuchará el servidor
	MoodleURL      string        // URL base de Moodle
	MoodleService  string        // Nombre del servicio de Moodle
	MaxConcurrency int           // Número máximo de peticiones simultáneas
	AttemptTimeout time.Duration // Timeout para cada intento de petición a Moodle
	RequestTimeout time.Duration // Timeout máximo global por petición HTTP
	MaxRetries     int           // Máximo número de reintentos para peticiones a Moodle
	APIKey         string
	OdooDSN        string
	OdooMaxConns   int
	TokenKeys      []tokens.Key
	LogLevel       string // Nivel de detalle de los logs ("debug", "info", "warn")

	// Fichero con las contraseñas de las cuentas de servicio (administradores). Esta informacion debe acabar en el vault
	ServiceCredentialsFile string
}

// Load es la función que construye y valida nuestra configuración.
// En Go, las funciones pueden devolver multiples valores a la vez.
// Esta función devuelve: (Config, error).
func Load() (Config, error) {
	var errs []error

	// Instancio el struct Config llenando los campos de tipo texto.
	// Si la variable de entorno no existe, le asignamos un valor por defecto mediante envStr().
	c := Config{
		ListenAddr:    envStr("LISTEN_ADDR", ":8080"),
		MoodleURL:     strings.TrimRight(os.Getenv("MOODLE_URL"), "/"),
		MoodleService: envStr("MOODLE_SERVICE", "moodle_mobile_app"),
		APIKey:        os.Getenv("API_KEY"),
		OdooDSN:       os.Getenv("ODOO_DB_DSN"),
		LogLevel:      envStr("LOG_LEVEL", "info"),

		ServiceCredentialsFile: os.Getenv("SERVICE_CREDENTIALS_FILE"),
	}
	c.MaxConcurrency = envInt("MOODLE_MAX_CONCURRENCY", 30, &errs)
	c.MaxRetries = envInt("MOODLE_MAX_RETRIES", 2, &errs)
	c.OdooMaxConns = envInt("ODOO_DB_MAX_CONNS", 10, &errs)
	c.AttemptTimeout = envDuration("MOODLE_ATTEMPT_TIMEOUT", 15*time.Second, &errs)
	c.RequestTimeout = envDuration("REQUEST_TIMEOUT", 30*time.Second, &errs)

	if c.MoodleURL == "" {
		errs = append(errs, errors.New("MOODLE_URL es obligatoria"))
	} else if u, err := url.Parse(c.MoodleURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("MOODLE_URL no es una URL http(s) válida: %q", c.MoodleURL))
	}
	if len(c.APIKey) < 16 {
		errs = append(errs, errors.New("API_KEY es obligatoria y debe tener al menos 16 caracteres (openssl rand -hex 32)"))
	}
	if c.MaxConcurrency < 1 {
		errs = append(errs, errors.New("MOODLE_MAX_CONCURRENCY debe ser >= 1"))
	}
	if c.MaxRetries < 0 {
		errs = append(errs, errors.New("MOODLE_MAX_RETRIES debe ser >= 0"))
	}
	if c.OdooMaxConns < 1 {
		errs = append(errs, errors.New("ODOO_DB_MAX_CONNS debe ser >= 1"))
	}
	if c.OdooDSN == "" {
		errs = append(errs, errors.New("ODOO_DB_DSN es obligatoria"))
	}

	keys, err := tokens.ParseKeys(os.Getenv("TOKEN_ENC_KEYS"))
	if err != nil {
		errs = append(errs, err)
	} else {
		c.TokenKeys = keys
	}

	return c, errors.Join(errs...)
}

// --- FUNCIONES AUXILIARES ---
// Empiezan por minúscula -> significa que son PRIVADAS del paquete 'config'
// y no pueden ser usadas desde fuera.

// Busca una variable de entorno. Si no existe (está vacía), devuelve un valor por defecto (def).
func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Lee una variable de entorno y la convierte a un entero (int).
// Recibe `errs *[]error`, que es un puntero a la lista de errores.
func envInt(key string, def int, errs *[]error) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return n
}

// Lee una variable de entorno y la convierte a tiempo (time.Duration).
// Entiende formatos legibles de Go como "10s", "1m", "500ms".
func envDuration(key string, def time.Duration, errs *[]error) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return d
}

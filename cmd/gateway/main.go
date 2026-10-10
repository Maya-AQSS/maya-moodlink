package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"maya-moodlink/internal/config"
	"maya-moodlink/internal/moodle"
	"maya-moodlink/internal/server"
	"maya-moodlink/internal/session"
	"maya-moodlink/internal/tokens"
)

func main() {
	// flag.Bool define una flag llamada `-healthcheck`. Retorna un PUNTERO (*bool).
	// Los valores devueltos por `flag` siempre son punteros porque su valor real
	// se asigna tras invocar flag.Parse().
	healthcheck := flag.Bool("healthcheck", false, "consulta /healthz en local y sale (para HEALTHCHECK de Docker)")

	// Procesa os.Args y asigna valores a las flags declaradas
	flag.Parse()

	// si está activo el healthcheck comprueba que este levantado y sale
	if *healthcheck {
		// os.Exit termina la ejecución de inmediato devolviendo un código de salida (0 u otro).
		os.Exit(runHealthcheck())
	}

	// encapsulo la lógica en una función `run() error`
	// para poder usar `defer` y propagar errores limpiamente.
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// Con error si hay un error dentro de la función se ejecutan los defer
// que permiten controlar los memory leaks o elementos no cerrados.
// Dicho de otro modo se realiza una salida limpia
func run() error {

	// Carga la configuración del sistema (variables de entorno, ficheros, etc.)
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	var level slog.Level
	// UnmarshalText convierte un string ("info", "debug") en un nivel de slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}

	// Creo un nuevo logger estructurado que emitirá registros en formato JSON a stdout
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	cipher, err := tokens.NewCipher(cfg.TokenKeys)
	if err != nil {
		return err
	}

	// La base de datos de Odoo, en SOLO LECTURA y solo las columnas necesarias de res_users.
	pcfg, err := pgxpool.ParseConfig(cfg.OdooDSN)
	if err != nil {
		return fmt.Errorf("ODOO_DB_DSN no válido: %w", err)
	}
	pcfg.MaxConns = int32(cfg.OdooMaxConns)
	pool, err := pgxpool.NewWithConfig(context.Background(), pcfg)
	if err != nil {
		return fmt.Errorf("pool de conexiones a la BD de Odoo: %w", err)
	}
	defer pool.Close()
	store := tokens.NewPGStore(pool)

	// Cuentas de servicio (administradores): contraseñas desde un fichero; en el futuro, Vault.
	var creds session.CredentialProvider
	if cfg.ServiceCredentialsFile != "" {
		c, err := session.LoadCredentials(cfg.ServiceCredentialsFile)
		if err != nil {
			return err
		}
		creds = c
		log.Info("cuentas de servicio cargadas", "count", len(c))
	}

	// Inicialización del cliente/módulo de Moodle
	mc := moodle.New(moodle.Options{
		BaseURL:        cfg.MoodleURL,
		MaxConcurrency: cfg.MaxConcurrency,
		AttemptTimeout: cfg.AttemptTimeout,
		MaxRetries:     cfg.MaxRetries,
		Logger:         log,
	})

	auth := session.New(session.Options{
		Store:   store,
		Cipher:  cipher,
		Creds:   creds,
		Moodle:  mc,
		Service: cfg.MoodleService,
		Log:     log,
	})

	// Inicialización de la lógica del servidor. el handler.
	// El Handler (o Router) es el componente encargado de recibir las peticiones HTTP de los usuarios
	// y decidir qué hacer con ella
	handler := server.New(server.Deps{
		Log:            log,
		Moodle:         mc,
		Auth:           auth,
		APIKey:         cfg.APIKey,
		MoodleService:  cfg.MoodleService,
		RequestTimeout: cfg.RequestTimeout,
	})

	// Instanciación del servidor HTTP configurando timeouts explícitos por seguridad
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Manejo de apagado suave (Graceful Shutdown):
	// Escuchamos llamadas de cancelación externas (CTRL+C / SIGTERM).
	// signal.NotifyContext cancela el contexto `ctx` automáticamente al recibir la señal.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Creamos un canal con buffer de tamaño 1 para capturar errores de la goroutine del servidor
	errCh := make(chan error, 1)

	go func() {
		// Lanzamos el servidor en segundo plano (goroutine)
		errCh <- srv.ListenAndServe()
	}()

	log.Info("gateway en marcha",
		"addr", cfg.ListenAddr,
		"moodle", cfg.MoodleURL,
		"max_concurrency", cfg.MaxConcurrency,
	)

	// La sentencia `select` bloquea hasta que uno de los canales reciba un mensaje/evento
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// Se recibió una señal de interrupción (CTRL+C / SIGTERM en Docker/K8s)
	}

	log.Info("apagando: terminando las peticiones en curso")
	// Se crea un contexto con un timeout estricto de 20s para forzar el cierre si se cuelga
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// srv.Shutdown deja de aceptar nuevas conexiones y espera a que las actuales terminen
	return srv.Shutdown(shutdownCtx)
}

// runHealthcheck lo usa el HEALTHCHECK del Dockerfile: la imagen distroless de producción no trae curl.
func runHealthcheck() int {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	// Separamos el host y el puerto
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 1
	}

	// Creo un cliente HTTP ligero con timeout estricto (3 segundos) para evitar bloqueos
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}

	// Cierro el Body de la respuesta HTTP en Go para evitar fugas de memoria/conexiones
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

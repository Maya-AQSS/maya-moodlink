package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maya-moodlink/internal/moodle"
	"maya-moodlink/internal/tokens"
)

// fakeMoodle emite tokens "tok-1", "tok-2"... en cada login; "badpass" da invalidlogin.
func fakeMoodle(issued *int32) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("/login/token.php", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("password") == "badpass" {
			io.WriteString(w, `{"error":"Invalid login","errorcode":"invalidlogin"}`)
			return
		}
		fmt.Fprintf(w, `{"token":"tok-%d"}`, atomic.AddInt32(issued, 1))
	})
	mux.HandleFunc("/webservice/rest/server.php", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"userid":5,"username":"profe"}`)
	})
	return httptest.NewServer(mux)
}

type env struct {
	auth   *Authenticator
	store  *tokens.MemStore
	cipher *tokens.Cipher
	issued *int32
}

func newEnv(t *testing.T, creds CredentialProvider) *env {
	t.Helper()
	var issued int32
	fm := fakeMoodle(&issued)
	t.Cleanup(fm.Close)

	cipher, err := tokens.NewCipher([]tokens.Key{{ID: "v1", Key: make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	store := tokens.NewMemStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mc := moodle.New(moodle.Options{BaseURL: fm.URL, MaxConcurrency: 8, AttemptTimeout: 2 * time.Second, Logger: log})
	return &env{
		auth:   New(Options{Store: store, Cipher: cipher, Creds: creds, Moodle: mc, Service: "svc", Log: log}),
		store:  store,
		cipher: cipher,
		issued: &issued,
	}
}

func (e *env) seedToken(t *testing.T, user, token string) {
	t.Helper()
	enc, err := e.cipher.Encrypt(user, token)
	if err != nil {
		t.Fatal(err)
	}
	e.store.Set(user, tokens.Row{TokenEnc: enc, MoodleUsername: "ana", Active: true})
}

func TestProfessorUsesStoredToken(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()

	// usuario que no existe o archivado
	if err := e.auth.Do(ctx, "7", func(string) error { return nil }); !errors.Is(err, ErrUserInactive) {
		t.Fatalf("inexistente: %v", err)
	}
	e.store.Set("7", tokens.Row{Active: false, TokenEnc: "x"})
	if err := e.auth.Do(ctx, "7", func(string) error { return nil }); !errors.Is(err, ErrUserInactive) {
		t.Fatalf("archivado: %v", err)
	}

	// activo pero sin token => debe identificarse
	e.store.Set("7", tokens.Row{Active: true})
	if err := e.auth.Do(ctx, "7", func(string) error { return nil }); !errors.Is(err, ErrNoToken) {
		t.Fatalf("sin token: %v", err)
	}

	// con token: fn recibe el token EN CLARO
	e.seedToken(t, "7", "SECRET")
	var got string
	if err := e.auth.Do(ctx, "7", func(tok string) error { got = tok; return nil }); err != nil || got != "SECRET" {
		t.Fatalf("got = %q err = %v", got, err)
	}

	// si Moodle lo rechaza NO se renueva (no hay contraseña): se devuelve el error
	calls := 0
	err := e.auth.Do(ctx, "7", func(string) error { calls++; return moodle.ErrInvalidToken })
	if !errors.Is(err, moodle.ErrInvalidToken) || calls != 1 {
		t.Fatalf("err = %v calls = %d", err, calls)
	}
	if n := atomic.LoadInt32(e.issued); n != 0 {
		t.Fatalf("un profesor no debe provocar logins en Moodle: %d", n)
	}
}

func TestProfessorCorruptTokenIsDecryptError(t *testing.T) {
	e := newEnv(t, nil)
	e.store.Set("7", tokens.Row{Active: true, TokenEnc: "v1.basura"})
	err := e.auth.Do(context.Background(), "7", func(string) error { return nil })
	if !errors.Is(err, tokens.ErrDecrypt) {
		t.Fatalf("esperaba ErrDecrypt, obtuve %v", err)
	}
}

func TestServiceAccountRenewsOnceUnderConcurrency(t *testing.T) {
	e := newEnv(t, StaticCredentials{"9": "adminpass"})
	e.store.Set("9", tokens.Row{Active: true, MoodleUsername: "admin"})

	// Moodle solo acepta "tok-2": el primer token que se emite ya está "revocado".
	op := func(token string) error {
		if token != "tok-2" {
			return moodle.ErrInvalidToken
		}
		return nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.auth.Do(context.Background(), "9", op); err != nil {
				t.Errorf("Do: %v", err)
			}
		}()
	}
	wg.Wait()

	if n := atomic.LoadInt32(e.issued); n != 2 {
		t.Fatalf("logins en Moodle = %d, esperaba 2 (30 peticiones concurrentes no deben renovar 30 veces)", n)
	}
	// ya cacheado: no vuelve a ir a Moodle
	if err := e.auth.Do(context.Background(), "9", op); err != nil || atomic.LoadInt32(e.issued) != 2 {
		t.Fatalf("err = %v logins = %d", err, atomic.LoadInt32(e.issued))
	}
}

func TestServiceAccountFailures(t *testing.T) {
	ctx := context.Background()

	// contraseña mal => ErrServiceAccount (y se conserva la causa)
	e := newEnv(t, StaticCredentials{"9": "badpass"})
	e.store.Set("9", tokens.Row{Active: true, MoodleUsername: "admin"})
	err := e.auth.Do(ctx, "9", func(string) error { return nil })
	if !errors.Is(err, ErrServiceAccount) || !errors.Is(err, moodle.ErrInvalidLogin) {
		t.Fatalf("contraseña incorrecta: %v", err)
	}

	// sin moodle_username configurado
	e = newEnv(t, StaticCredentials{"9": "adminpass"})
	e.store.Set("9", tokens.Row{Active: true})
	if err := e.auth.Do(ctx, "9", func(string) error { return nil }); !errors.Is(err, ErrServiceAccount) {
		t.Fatalf("sin moodle_username: %v", err)
	}

	// archivado
	e.store.Set("9", tokens.Row{Active: false, MoodleUsername: "admin"})
	if err := e.auth.Do(ctx, "9", func(string) error { return nil }); !errors.Is(err, ErrUserInactive) {
		t.Fatalf("archivado: %v", err)
	}

	// Moodle sigue rechazando el token recién emitido: se reintenta UNA vez y se rinde,
	// sin devolver ErrInvalidToken (eso haría que Odoo pidiera una contraseña que no existe)
	e = newEnv(t, StaticCredentials{"9": "adminpass"})
	e.store.Set("9", tokens.Row{Active: true, MoodleUsername: "admin"})
	calls := 0
	err = e.auth.Do(ctx, "9", func(string) error { calls++; return moodle.ErrInvalidToken })
	if !errors.Is(err, ErrServiceAccount) || errors.Is(err, moodle.ErrInvalidToken) || calls != 2 {
		t.Fatalf("err = %v calls = %d", err, calls)
	}
}

func TestIssueTokenReturnsOnlyEncryptedToken(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()

	issued, err := e.auth.IssueToken(ctx, "7", "ana", "correcta")
	if err != nil {
		t.Fatal(err)
	}
	if issued.TokenEnc == "tok-1" || issued.MoodleUserID != 5 {
		t.Fatalf("issued = %+v", issued)
	}
	if plain, err := e.cipher.Decrypt("7", issued.TokenEnc); err != nil || plain != "tok-1" {
		t.Fatalf("descifrado = %q err = %v", plain, err)
	}
	// y además de verdad queda ligado a ese usuario
	if _, err := e.cipher.Decrypt("8", issued.TokenEnc); !errors.Is(err, tokens.ErrDecrypt) {
		t.Fatalf("no debería poder usarse para otro usuario: %v", err)
	}

	if _, err := e.auth.IssueToken(ctx, "7", "ana", "badpass"); !errors.Is(err, moodle.ErrInvalidLogin) {
		t.Fatalf("contraseña incorrecta: %v", err)
	}
}

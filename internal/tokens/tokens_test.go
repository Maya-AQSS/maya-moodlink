package tokens

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func b64(b byte) string { return base64.StdEncoding.EncodeToString(key(b)) }

func newCipher(t *testing.T, keys ...Key) *Cipher {
	t.Helper()
	c, err := NewCipher(keys)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCipherRoundTrip(t *testing.T) {
	c := newCipher(t, Key{ID: "v1", Key: key(1)})

	enc, err := c.Encrypt("7", "SECRET-TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, "SECRET-TOKEN") {
		t.Fatal("el token aparece en claro")
	}
	if !strings.HasPrefix(enc, "v1.") {
		t.Fatalf("falta el id de clave: %q", enc)
	}
	dec, err := c.Decrypt("7", enc)
	if err != nil || dec != "SECRET-TOKEN" {
		t.Fatalf("descifrado = %q, err = %v", dec, err)
	}

	enc2, _ := c.Encrypt("7", "SECRET-TOKEN")
	if enc == enc2 {
		t.Fatal("dos cifrados del mismo valor no deberían ser idénticos (nonce aleatorio)")
	}
}

func TestCipherIsBoundToUser(t *testing.T) {
	c := newCipher(t, Key{ID: "v1", Key: key(1)})
	enc, _ := c.Encrypt("7", "SECRET-TOKEN")
	// copiar el valor cifrado a la fila de otro usuario no debe funcionar
	if _, err := c.Decrypt("8", enc); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("esperaba ErrDecrypt, obtuve %v", err)
	}
}

func TestCipherWrongKeyAndGarbage(t *testing.T) {
	a := newCipher(t, Key{ID: "v1", Key: key(1)})
	b := newCipher(t, Key{ID: "v1", Key: key(2)})
	enc, _ := a.Encrypt("7", "x")
	if _, err := b.Decrypt("7", enc); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("clave incorrecta: %v", err)
	}
	for _, bad := range []string{"", "sinpunto", "v1.", "v1.@@@", "v9.AAAA"} {
		if _, err := a.Decrypt("7", bad); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("%q: esperaba ErrDecrypt, obtuve %v", bad, err)
		}
	}
}

func TestCipherKeyRotation(t *testing.T) {
	old := newCipher(t, Key{ID: "v1", Key: key(1)})
	encOld, _ := old.Encrypt("7", "TOKEN")

	rotated := newCipher(t, Key{ID: "v2", Key: key(2)}, Key{ID: "v1", Key: key(1)})
	if dec, err := rotated.Decrypt("7", encOld); err != nil || dec != "TOKEN" {
		t.Fatalf("la clave antigua debe seguir descifrando: %q %v", dec, err)
	}
	encNew, _ := rotated.Encrypt("7", "TOKEN")
	if !strings.HasPrefix(encNew, "v2.") {
		t.Fatalf("debe cifrar con la clave actual: %q", encNew)
	}

	// al retirar la clave antigua, lo cifrado con ella deja de poder leerse
	onlyNew := newCipher(t, Key{ID: "v2", Key: key(2)})
	if _, err := onlyNew.Decrypt("7", encOld); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("esperaba ErrDecrypt, obtuve %v", err)
	}
}

func TestParseKeys(t *testing.T) {
	keys, err := ParseKeys("v2:" + b64(2) + " , v1:" + b64(1))
	if err != nil || len(keys) != 2 || keys[0].ID != "v2" || keys[1].ID != "v1" {
		t.Fatalf("keys = %+v err = %v", keys, err)
	}
	for _, bad := range []string{
		"",
		"v1:abc",                         // no es una clave de 32 bytes
		"V1:" + b64(1),                   // id con mayúsculas
		"sinid" + b64(1),                 // sin separador
		"v1:" + b64(1) + ",v1:" + b64(2), // id repetido
	} {
		if _, err := ParseKeys(bad); err == nil {
			t.Fatalf("%q: esperaba error", bad)
		}
	}
}

func TestMemStore(t *testing.T) {
	s := NewMemStore()
	if _, ok, _ := s.Get(context.Background(), "7"); ok {
		t.Fatal("no debería existir")
	}
	s.Set("7", Row{TokenEnc: "x", MoodleUsername: "ana", Active: true})
	row, ok, err := s.Get(context.Background(), "7")
	if err != nil || !ok || row.MoodleUsername != "ana" || !row.Active {
		t.Fatalf("row = %+v ok = %v err = %v", row, ok, err)
	}
}

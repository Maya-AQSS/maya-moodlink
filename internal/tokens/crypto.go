package tokens

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// ErrDecrypt: el valor guardado no se puede descifrar (clave retirada o incorrecta,
// contenido corrupto o fila manipulada).
var ErrDecrypt = errors.New("tokens: no se pudo descifrar el token")

var keyIDRe = regexp.MustCompile(`^[a-z0-9]{1,8}$`)

// Key es una clave AES-256 con un identificador que se guarda junto al dato cifrado,
// de modo que se pueden rotar claves sin perder los datos antiguos.
type Key struct {
	ID  string
	Key []byte
}

// ParseKeys interpreta "v2:BASE64,v1:BASE64". La primera clave es la actual (con la que
// se cifra); las demás solo sirven para descifrar datos antiguos.
func ParseKeys(spec string) ([]Key, error) {
	var keys []Key
	seen := map[string]bool{}
	for i, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// los errores no incluyen el material de la clave
		id, b64, ok := strings.Cut(part, ":")
		if !ok || !keyIDRe.MatchString(id) {
			return nil, fmt.Errorf("TOKEN_ENC_KEYS, entrada %d: formato esperado id:base64 (id de 1 a 8 caracteres a-z0-9)", i+1)
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("TOKEN_ENC_KEYS, entrada %d: la clave debe ser base64 de exactamente 32 bytes", i+1)
		}
		if seen[id] {
			return nil, fmt.Errorf("TOKEN_ENC_KEYS: id de clave repetido %q", id)
		}
		seen[id] = true
		keys = append(keys, Key{ID: id, Key: raw})
	}
	if len(keys) == 0 {
		return nil, errors.New("TOKEN_ENC_KEYS: no hay ninguna clave")
	}
	return keys, nil
}

// Cipher cifra y descifra tokens con AES-256-GCM.
//
// Formato guardado: "<idClave>.<base64url(nonce || ciphertext+tag)>".
// Como dato asociado (AAD) se autentica el id de usuario: si alguien copia el valor
// cifrado de una fila a la de otro usuario, el descifrado falla.
type Cipher struct {
	currentID string
	aeads     map[string]cipher.AEAD
}

func NewCipher(keys []Key) (*Cipher, error) {
	if len(keys) == 0 {
		return nil, errors.New("tokens: se necesita al menos una clave")
	}
	c := &Cipher{currentID: keys[0].ID, aeads: make(map[string]cipher.AEAD, len(keys))}
	for _, k := range keys {
		block, err := aes.NewCipher(k.Key)
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		c.aeads[k.ID] = aead
	}
	return c, nil
}

func additionalData(user string) []byte {
	return []byte("maya-moodle-token/v1|user=" + user)
}

func (c *Cipher) Encrypt(user, plaintext string) (string, error) {
	aead := c.aeads[c.currentID]
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), additionalData(user)) // nonce || ciphertext
	return c.currentID + "." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c *Cipher) Decrypt(user, blob string) (string, error) {
	id, enc, ok := strings.Cut(blob, ".")
	if !ok {
		return "", fmt.Errorf("%w: formato desconocido", ErrDecrypt)
	}
	aead, ok := c.aeads[id]
	if !ok {
		return "", fmt.Errorf("%w: la clave %q no está disponible", ErrDecrypt, id)
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil || len(raw) < aead.NonceSize() {
		return "", fmt.Errorf("%w: contenido corrupto", ErrDecrypt)
	}
	ns := aead.NonceSize()
	plain, err := aead.Open(nil, raw[:ns], raw[ns:], additionalData(user))
	if err != nil {
		return "", fmt.Errorf("%w: autenticación fallida (clave incorrecta o fila manipulada)", ErrDecrypt)
	}
	return string(plain), nil
}

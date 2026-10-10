// Package tokens: acceso de solo lectura a los tokens de Moodle que guarda Odoo, y su cifrado.
package tokens

import (
	"context"
	"sync"
)

// Row es lo que el gateway lee de res_users para un usuario de Odoo.
type Row struct {
	TokenEnc       string // token cifrado; vacío si el usuario no tiene
	MoodleUsername string
	Active         bool
}

// Store es el acceso a los datos de Moodle de un usuario de Odoo.
//
// Es de SOLO LECTURA a propósito: toda escritura (guardar un token nuevo, borrarlo al
// archivar al usuario...) la hace Odoo con su ORM. El gateway ni siquiera tiene permiso de
// escritura en la base de datos.
type Store interface {
	// Get devuelve la fila del usuario; ok=false si el usuario no existe.
	Get(ctx context.Context, user string) (row Row, ok bool, err error)
}

// MemStore es un Store en memoria para tests.
type MemStore struct {
	mu sync.RWMutex
	m  map[string]Row
}

func NewMemStore() *MemStore { return &MemStore{m: map[string]Row{}} }

// Set simula lo que haría Odoo al escribir en res_users.
func (s *MemStore) Set(user string, row Row) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[user] = row
}

func (s *MemStore) Get(_ context.Context, user string) (Row, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row, ok := s.m[user]
	return row, ok, nil
}

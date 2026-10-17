package moodle

import (
	"context"
	"net/url"
	"strconv"
)

// User es la parte útil de un usuario de Moodle (core_user_get_users_by_field).
type User struct {
	ID        int    `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"firstname"`
	LastName  string `json:"lastname"`
	Email     string `json:"email"`
}

// usersBatch limita los ids por llamada para no generar peticiones enormes.
const usersBatch = 100

// UsersByID devuelve los usuarios con esos ids, indexados por id. Los que no existen (o
// el token no puede ver) simplemente no aparecen.
func (c *Client) UsersByID(ctx context.Context, token string, ids []int) (map[int]User, error) {
	users := make(map[int]User, len(ids))
	for start := 0; start < len(ids); start += usersBatch {
		end := min(start+usersBatch, len(ids))
		params := url.Values{"field": {"id"}}
		for i, id := range ids[start:end] {
			params.Set("values["+strconv.Itoa(i)+"]", strconv.Itoa(id))
		}
		var batch []User
		if err := c.Call(ctx, token, "core_user_get_users_by_field", params, true, &batch); err != nil {
			return nil, err
		}
		for _, u := range batch {
			users[u.ID] = u
		}
	}
	return users, nil
}

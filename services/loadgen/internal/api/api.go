// Package api implements the HTTP handlers k6 hits to generate write load
// against Postgres. A Store interface isolates the database so handlers
// can be unit-tested with a fake in-memory implementation.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Store is the minimum surface the handlers need. Concrete impl in
// internal/db wraps pgxpool; tests use an in-memory fake.
type Store interface {
	InsertUser(ctx context.Context, email, fullName string, profile map[string]any) (int64, error)
	UpdateRandomUser(ctx context.Context, newFullName string) (int64, error) // returns id updated, 0 if no users
	DeleteRandomUser(ctx context.Context) (int64, error)                     // returns id deleted, 0 if no users
}

// ErrNoRows signals "zero rows affected" — handlers translate to 404.
var ErrNoRows = errors.New("api: no rows")

type Handler struct {
	Store Store
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /users", h.insert)
	mux.HandleFunc("PATCH /users/random", h.updateRandom)
	mux.HandleFunc("DELETE /users/random", h.deleteRandom)
	mux.HandleFunc("GET /healthz", h.health)
}

type insertBody struct {
	Email    string         `json:"email"`
	FullName string         `json:"full_name"`
	Profile  map[string]any `json:"profile"`
}

type insertResp struct {
	ID int64 `json:"id"`
}

func (h *Handler) insert(w http.ResponseWriter, r *http.Request) {
	var b insertBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if b.Email == "" {
		httpErr(w, http.StatusBadRequest, "email required")
		return
	}
	if b.Profile == nil {
		b.Profile = map[string]any{}
	}
	id, err := h.Store.InsertUser(r.Context(), b.Email, b.FullName, b.Profile)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "insert: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, insertResp{ID: id})
}

type updateBody struct {
	FullName string `json:"full_name"`
}

func (h *Handler) updateRandom(w http.ResponseWriter, r *http.Request) {
	var b updateBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if b.FullName == "" {
		b.FullName = "updated"
	}
	id, err := h.Store.UpdateRandomUser(r.Context(), b.FullName)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "update: "+err.Error())
		return
	}
	if id == 0 {
		// No users to update yet — k6 is running faster than inserts. Not an error.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, insertResp{ID: id})
}

func (h *Handler) deleteRandom(w http.ResponseWriter, r *http.Request) {
	id, err := h.Store.DeleteRandomUser(r.Context())
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "delete: "+err.Error())
		return
	}
	if id == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, insertResp{ID: id})
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fmt.Fprintln(w, err)
	}
}

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"loadgen/internal/api"
)

// fakeStore is a concurrent-safe in-memory Store double.
type fakeStore struct {
	mu       sync.Mutex
	users    map[int64]string // id -> full_name
	nextID   int64
	failWith error
}

func newFakeStore() *fakeStore {
	return &fakeStore{users: map[int64]string{}, nextID: 1}
}

func (f *fakeStore) InsertUser(_ context.Context, email, fullName string, _ map[string]any) (int64, error) {
	if f.failWith != nil {
		return 0, f.failWith
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextID
	f.nextID++
	f.users[id] = fullName
	return id, nil
}

func (f *fakeStore) UpdateRandomUser(_ context.Context, newFullName string) (int64, error) {
	if f.failWith != nil {
		return 0, f.failWith
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.users {
		f.users[id] = newFullName
		return id, nil
	}
	return 0, nil // no users
}

func (f *fakeStore) DeleteRandomUser(_ context.Context) (int64, error) {
	if f.failWith != nil {
		return 0, f.failWith
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.users {
		delete(f.users, id)
		return id, nil
	}
	return 0, nil
}

func newServer(store api.Store) http.Handler {
	mux := http.NewServeMux()
	(&api.Handler{Store: store}).Register(mux)
	return mux
}

func TestInsert_Happy(t *testing.T) {
	s := newFakeStore()
	srv := newServer(s)
	body := `{"email":"a@b.c","full_name":"Alice","profile":{"plan":"pro"}}`
	req := httptest.NewRequest("POST", "/users", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d body=%s", rec.Code, rec.Body)
	}
	var r struct{ ID int64 }
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	if r.ID != 1 {
		t.Errorf("want id=1, got %d", r.ID)
	}
}

func TestInsert_MissingEmailIs400(t *testing.T) {
	srv := newServer(newFakeStore())
	req := httptest.NewRequest("POST", "/users", strings.NewReader(`{"full_name":"x"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rec.Code)
	}
}

func TestInsert_InvalidJSONIs400(t *testing.T) {
	srv := newServer(newFakeStore())
	req := httptest.NewRequest("POST", "/users", strings.NewReader(`{not json`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rec.Code)
	}
}

func TestUpdateRandom_HappyReturns200WithID(t *testing.T) {
	s := newFakeStore()
	_, _ = s.InsertUser(context.Background(), "x@y.z", "x", nil)
	srv := newServer(s)
	req := httptest.NewRequest("PATCH", "/users/random", strings.NewReader(`{"full_name":"new"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body)
	}
}

func TestUpdateRandom_EmptyStoreReturns204(t *testing.T) {
	srv := newServer(newFakeStore())
	req := httptest.NewRequest("PATCH", "/users/random", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("want 204 when no users exist, got %d", rec.Code)
	}
}

func TestDeleteRandom_HappyAndEmpty(t *testing.T) {
	s := newFakeStore()
	_, _ = s.InsertUser(context.Background(), "x@y.z", "x", nil)
	srv := newServer(s)

	req := httptest.NewRequest("DELETE", "/users/random", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("1st delete: want 200, got %d", rec.Code)
	}

	// Now empty — second delete must be 204.
	req = httptest.NewRequest("DELETE", "/users/random", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("2nd delete on empty: want 204, got %d", rec.Code)
	}
}

func TestHealth(t *testing.T) {
	srv := newServer(newFakeStore())
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

func TestInsert_StoreErrorBubbles500(t *testing.T) {
	s := newFakeStore()
	s.failWith = errors.New("db down")
	srv := newServer(s)
	req := httptest.NewRequest("POST", "/users",
		bytes.NewBufferString(`{"email":"a@b.c","full_name":"Alice"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
}

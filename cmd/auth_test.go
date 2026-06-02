package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestDeviceCode(t *testing.T) {
	t.Run("numeric interval", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_code":"ABCD-1234","device_auth_id":"dev-1","interval":7}`))
		}))
		defer ts.Close()

		userCode, deviceAuthID, interval, err := requestDeviceCode(context.Background(), ts.Client(), ts.URL, "client-1")
		if err != nil {
			t.Fatalf("requestDeviceCode: %v", err)
		}
		if userCode != "ABCD-1234" {
			t.Errorf("user_code = %q, want %q", userCode, "ABCD-1234")
		}
		if deviceAuthID != "dev-1" {
			t.Errorf("device_auth_id = %q, want %q", deviceAuthID, "dev-1")
		}
		if interval != 7*time.Second {
			t.Errorf("interval = %s, want 7s", interval)
		}
	})

	t.Run("string interval", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"user_code":"X","device_auth_id":"d","interval":"10"}`))
		}))
		defer ts.Close()

		_, _, interval, err := requestDeviceCode(context.Background(), ts.Client(), ts.URL, "client-1")
		if err != nil {
			t.Fatalf("requestDeviceCode: %v", err)
		}
		if interval != 10*time.Second {
			t.Errorf("interval = %s, want 10s", interval)
		}
	})

	t.Run("interval clamped to minimum", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"user_code":"X","device_auth_id":"d","interval":1}`))
		}))
		defer ts.Close()

		_, _, interval, err := requestDeviceCode(context.Background(), ts.Client(), ts.URL, "client-1")
		if err != nil {
			t.Fatalf("requestDeviceCode: %v", err)
		}
		if interval != deviceMinInterval {
			t.Errorf("interval = %s, want clamped %s", interval, deviceMinInterval)
		}
	})

	t.Run("missing interval defaults", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"user_code":"X","device_auth_id":"d"}`))
		}))
		defer ts.Close()

		_, _, interval, err := requestDeviceCode(context.Background(), ts.Client(), ts.URL, "client-1")
		if err != nil {
			t.Fatalf("requestDeviceCode: %v", err)
		}
		if interval != deviceDefaultInterval {
			t.Errorf("interval = %s, want default %s", interval, deviceDefaultInterval)
		}
	})

	t.Run("non-200 status", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusInternalServerError)
		}))
		defer ts.Close()

		_, _, _, err := requestDeviceCode(context.Background(), ts.Client(), ts.URL, "client-1")
		if err == nil {
			t.Fatal("expected error on HTTP 500")
		}
	})

	t.Run("incomplete response", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"user_code":"X"}`))
		}))
		defer ts.Close()

		_, _, _, err := requestDeviceCode(context.Background(), ts.Client(), ts.URL, "client-1")
		if err == nil {
			t.Fatal("expected error on missing device_auth_id")
		}
	})
}

func TestPollDeviceAuth(t *testing.T) {
	t.Run("pending then success", func(t *testing.T) {
		var calls int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := atomic.AddInt32(&calls, 1)
			if n < 3 {
				// Alternate the two "pending" statuses.
				if n == 1 {
					w.WriteHeader(http.StatusForbidden)
				} else {
					w.WriteHeader(http.StatusNotFound)
				}
				return
			}
			_, _ = w.Write([]byte(`{"authorization_code":"auth-xyz","code_verifier":"ver-abc"}`))
		}))
		defer ts.Close()

		authCode, verifier, err := pollDeviceAuth(context.Background(), ts.Client(), ts.URL, "d", "u", time.Millisecond)
		if err != nil {
			t.Fatalf("pollDeviceAuth: %v", err)
		}
		if authCode != "auth-xyz" {
			t.Errorf("authorization_code = %q, want %q", authCode, "auth-xyz")
		}
		if verifier != "ver-abc" {
			t.Errorf("code_verifier = %q, want %q", verifier, "ver-abc")
		}
		if atomic.LoadInt32(&calls) < 3 {
			t.Errorf("expected at least 3 polls, got %d", calls)
		}
	})

	t.Run("hard error status", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "bad request", http.StatusBadRequest)
		}))
		defer ts.Close()

		_, _, err := pollDeviceAuth(context.Background(), ts.Client(), ts.URL, "d", "u", time.Millisecond)
		if err == nil {
			t.Fatal("expected error on HTTP 400")
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden) // always pending
		}))
		defer ts.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, _, err := pollDeviceAuth(ctx, ts.Client(), ts.URL, "d", "u", time.Millisecond)
		if err == nil {
			t.Fatal("expected error on cancelled context")
		}
	})
}

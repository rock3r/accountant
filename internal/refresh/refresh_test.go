package refresh

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"accountant/internal/provider"
	"accountant/internal/provider/codex"
	"accountant/internal/vault"
)

// makeJWTCred builds a credential whose access token is a JWT expiring at exp.
func makeJWTCred(id string, exp time.Time) []byte {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload, _ := json.Marshal(map[string]any{"exp": exp.Unix()})
	jwt := header + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + id
	b, _ := json.Marshal(codex.Credential{AccessToken: jwt, RefreshToken: "rt-" + id, AccountID: "acct"})
	return b
}

func jwtCred(t *testing.T, id string, exp time.Time) []byte {
	t.Helper()
	return makeJWTCred(id, exp)
}

// fakeRefresher counts exchanges and rotates the credential.
type fakeRefresher struct {
	*codex.Provider
	calls atomic.Int32
	delay time.Duration
}

func (f *fakeRefresher) Refresh(ctx context.Context, cred []byte) ([]byte, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return makeJWTCred("new", time.Now().Add(240*time.Hour)), nil
}

func TestIfNeededFreshCredentialSkipsRefresh(t *testing.T) {
	v := vault.NewMemory()
	fresh := jwtCred(t, "fresh", time.Now().Add(240*time.Hour))
	if err := v.Put("a", fresh); err != nil {
		t.Fatal(err)
	}
	fr := &fakeRefresher{Provider: codex.New("")}
	cred, refreshed, err := IfNeeded(context.Background(), v, fr, t.TempDir()+"/state.json", "a", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed || fr.calls.Load() != 0 {
		t.Errorf("fresh credential: refreshed=%v calls=%d", refreshed, fr.calls.Load())
	}
	if string(cred) != string(fresh) {
		t.Error("returned credential is not the vault value")
	}
}

func TestIfNeededExpiredRefreshesAndWritesVault(t *testing.T) {
	v := vault.NewMemory()
	if err := v.Put("a", jwtCred(t, "old", time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	fr := &fakeRefresher{Provider: codex.New("")}
	stateFile := t.TempDir() + "/state.json"
	cred, refreshed, err := IfNeeded(context.Background(), v, fr, stateFile, "a", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed || fr.calls.Load() != 1 {
		t.Fatalf("refreshed=%v calls=%d", refreshed, fr.calls.Load())
	}
	stored, _ := v.Get("a")
	if string(stored) != string(cred) {
		t.Error("vault was not updated before the call returned")
	}

	// Second run: the vault now holds a fresh credential, so no exchange.
	fr.delay = 0
	if _, refreshed, err := IfNeeded(context.Background(), v, fr, stateFile, "a", 30*time.Minute); err != nil || refreshed {
		t.Errorf("second run: refreshed=%v err=%v", refreshed, err)
	}
	if fr.calls.Load() != 1 {
		t.Errorf("second run made another exchange: calls=%d", fr.calls.Load())
	}
}

// TestIfNeededSerializesLateArrivals is the Codex-review race: many callers
// arrive with the credential already expired; even with a slow exchange,
// exactly one refresh happens and every caller gets a usable credential.
func TestIfNeededSerializesLateArrivals(t *testing.T) {
	v := vault.NewMemory()
	if err := v.Put("a", jwtCred(t, "old", time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	fr := &fakeRefresher{Provider: codex.New(""), delay: 50 * time.Millisecond}
	stateFile := t.TempDir() + "/state.json"

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := IfNeeded(context.Background(), v, fr, stateFile, "a", 30*time.Minute)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := fr.calls.Load(); got != 1 {
		t.Errorf("exchanges: %d, want 1", got)
	}
}

// compile-time check: fakeRefresher satisfies the interfaces IfNeeded needs.
var (
	_ provider.Refresher         = (*fakeRefresher)(nil)
	_ provider.CredentialExpirer = (*fakeRefresher)(nil)
)

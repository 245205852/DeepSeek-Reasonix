package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reasonix/internal/config"
	"reasonix/internal/control"
)

const providerSetupTestKeyEnv = "REASONIX_REMOTE_SETUP_TEST_KEY"

func TestProviderSetupStoresRemoteCredentialAndRebuildsController(t *testing.T) {
	s, secret := newProviderSetupTestServer(t)
	if !s.EnableProviderSetupForListener("127.0.0.1:8787") {
		t.Fatal("loopback listener did not enable Provider setup")
	}

	built := 0
	s.buildController = func(_ context.Context, ref string) (*control.Controller, error) {
		built++
		if ref != "remote-demo/model-a" {
			t.Fatalf("rebuilt ref = %q, want remote-demo/model-a", ref)
		}
		return control.New(control.Options{
			Sink:       s.bc,
			Label:      "model-a",
			ModelRef:   ref,
			SessionDir: t.TempDir(),
		}), nil
	}

	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()

	index := getProviderSetupBody(t, httpServer.URL+"/")
	if !strings.Contains(index, "Reasonix Provider Setup") {
		t.Fatalf("missing-key index did not serve Provider setup page:\n%s", index)
	}
	if strings.Contains(index, secret) {
		t.Fatal("setup page reflected the Provider secret")
	}

	resp, err := http.Get(httpServer.URL + "/provider-setup")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("setup status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		resp.Body.Close()
		t.Fatalf("setup Cache-Control = %q, want no-store", got)
	}
	var state providerSetupState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if !state.Required || state.Provider != "remote-demo" || state.Model != "model-a" || state.KeyEnv != providerSetupTestKeyEnv {
		t.Fatalf("unexpected setup state: %+v", state)
	}

	resp = postProviderSetup(t, httpServer.URL, `{"apiKey":"`+secret+`"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("setup save = %d, want 204: %s", resp.StatusCode, body)
	}
	if bytes.Contains(body, []byte(secret)) {
		t.Fatal("setup response reflected the Provider secret")
	}
	if built != 1 {
		t.Fatalf("controller builds = %d, want 1", built)
	}
	resp = postProviderSetup(t, httpServer.URL, `{"apiKey":"second-secret"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("stale setup save = %d, want 409", resp.StatusCode)
	}
	if built != 1 {
		t.Fatalf("stale setup triggered %d controller builds, want 1 total", built)
	}
	resolved := config.ResolveCredentialForRootGlobalFirst(".", providerSetupTestKeyEnv)
	if !resolved.Set || resolved.Value != secret {
		t.Fatalf("stored credential = set:%v value:%q, want saved secret", resolved.Set, resolved.Value)
	}
	info, err := os.Stat(config.UserCredentialsPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential file mode = %o, want 600", info.Mode().Perm())
	}

	resp, err = http.Get(httpServer.URL + "/provider-setup")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if state.Required {
		t.Fatalf("setup still required after save: %+v", state)
	}
	index = getProviderSetupBody(t, httpServer.URL+"/")
	if strings.Contains(index, "Reasonix Provider Setup") {
		t.Fatal("normal Serve UI did not replace setup page after controller rebuild")
	}
}

func TestProviderSetupActivationFailureKeepsCredentialAndHidesDetails(t *testing.T) {
	s, secret := newProviderSetupTestServer(t)
	s.EnableProviderSetupForListener("127.0.0.1:8787")
	s.buildController = func(context.Context, string) (*control.Controller, error) {
		return nil, errors.New("sensitive remote path: /srv/private/config.toml")
	}
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()

	resp := postProviderSetup(t, httpServer.URL, `{"apiKey":"`+secret+`"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("activation failure = %d, want 500: %s", resp.StatusCode, body)
	}
	if bytes.Contains(body, []byte(secret)) || bytes.Contains(body, []byte("/srv/private")) {
		t.Fatalf("activation failure reflected sensitive details: %s", body)
	}
	resolved := config.ResolveCredentialForRootGlobalFirst(".", providerSetupTestKeyEnv)
	if !resolved.Set || resolved.Value != secret {
		t.Fatal("activation failure did not retain the successfully saved credential")
	}
	state, ok := s.providerSetupSnapshot()
	if !ok || !state.Required || !strings.Contains(state.Error, "credential was saved") {
		t.Fatalf("activation failure state = %+v, enabled:%v", state, ok)
	}
	if strings.Contains(state.Error, secret) || strings.Contains(state.Error, "/srv/private") {
		t.Fatalf("activation failure state exposed sensitive details: %q", state.Error)
	}
}

func TestProviderSetupIsLoopbackOnlyAndAuthenticated(t *testing.T) {
	s, _ := newProviderSetupTestServer(t)
	if s.EnableProviderSetupForListener("0.0.0.0:8787") {
		t.Fatal("non-loopback listener enabled Provider setup")
	}

	req := httptest.NewRequest(http.MethodGet, "/provider-setup", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled setup endpoint = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), providerSetupTestKeyEnv) {
		t.Fatal("disabled setup endpoint exposed Provider metadata")
	}

	s.EnableProviderSetupForListener("[::1]:8787")
	protected := New(s.ctl(), s.bc, config.ServeConfig{AuthMode: "token", Token: "serve-token"})
	protected.EnableProviderSetupForListener("127.0.0.1:8787")
	req = httptest.NewRequest(http.MethodGet, "/provider-setup", nil)
	req.Header.Set("Accept", "application/json")
	rec = httptest.NewRecorder()
	protected.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated setup endpoint = %d, want 401", rec.Code)
	}
}

func TestProviderSetupRejectsUnsafeOrAmbiguousRequests(t *testing.T) {
	s, _ := newProviderSetupTestServer(t)
	s.EnableProviderSetupForListener("127.0.0.1:8787")
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()

	req, err := http.NewRequest(http.MethodPost, httpServer.URL+"/provider-setup", strings.NewReader(`{"apiKey":"secret"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON setup = %d, want 415", resp.StatusCode)
	}

	cases := []string{
		`{"apiKey":"secret","extra":true}`,
		`{"apiKey":"secret"}{"apiKey":"second"}`,
		`{"apiKey":"` + strings.Repeat("x", providerSetupMaxBody) + `"}`,
	}
	for _, body := range cases {
		resp = postProviderSetup(t, httpServer.URL, body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("unsafe setup body status = %d, want 400", resp.StatusCode)
		}
	}
	if config.CredentialStored(providerSetupTestKeyEnv) {
		t.Fatal("rejected setup request persisted a credential")
	}

	page := string(providerSetupHTML)
	if !strings.Contains(page, `type="password"`) {
		t.Fatal("setup UI does not use a password input")
	}
	if strings.Contains(strings.ToLower(page), "localstorage") {
		t.Fatal("setup UI must not persist Provider secrets in localStorage")
	}
}

func newProviderSetupTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("REASONIX_CREDENTIALS_STORE", "file")
	t.Setenv(providerSetupTestKeyEnv, "")
	configPath := config.UserConfigPath()
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	configBody := `default_model = "remote-demo/model-a"

[[providers]]
name = "remote-demo"
kind = "openai"
base_url = "https://example.invalid/v1"
models = ["model-a"]
default = "model-a"
api_key_env = "` + providerSetupTestKeyEnv + `"
`
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}

	bc := NewBroadcaster()
	ctrl := control.New(control.Options{
		Sink:       bc,
		Label:      "model-a",
		ModelRef:   "remote-demo/model-a",
		SessionDir: t.TempDir(),
	})
	return New(ctrl, bc, config.ServeConfig{}), "remote-secret-for-test"
}

func postProviderSetup(t *testing.T, baseURL, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/provider-setup", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func getProviderSetupBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, body)
	}
	return string(body)
}

package loopback

import (
	"crypto/sha256"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReauthorizationCallbackParameters(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query url.Values
		valid bool
	}{
		{"client ID omitted", url.Values{"code": {"one-use-code"}}, true},
		{"same client ID", url.Values{"code": {"one-use-code"}, "client_id": {"saved"}}, true},
		{"explicit empty client ID", url.Values{"code": {"one-use-code"}, "client_id": {""}}, false},
		{"empty error with code", url.Values{"code": {"one-use-code"}, "error": {""}}, false},
		{"repeated error with code", url.Values{"code": {"one-use-code"}, "error": {"", "access_denied"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseCallback(tc.query, "saved")
			if tc.valid {
				if err != nil || result != (Result{Code: "one-use-code", ClientID: "saved"}) {
					t.Fatalf("callback lost the saved registration: %v", err)
				}
			} else if err == nil || result != (Result{}) {
				t.Fatal("malformed callback supplied a code for exchange")
			}
		})
	}
}

func TestNewRegistrationRequiresIssuedID(t *testing.T) {
	for _, tc := range []struct {
		id    string
		valid bool
	}{
		{"issued-client-v2", true}, {"oaiapp_example", true}, {"", false},
		{"dynamic_agent_client", false}, {"has space", false}, {"line\nbreak", false},
		{strings.Repeat("a", 257), false}, {"\xff", false},
	} {
		result, err := parseCallback(url.Values{"code": {"code"}, "client_id": {tc.id}}, "")
		if tc.valid {
			if err != nil || result.ClientID != tc.id {
				t.Errorf("issued ID %q did not round trip: %v", tc.id, err)
			}
		} else if err == nil || result != (Result{}) {
			t.Errorf("invalid ID %q produced a result", tc.id)
		}
	}
}

func TestCallbackIgnoresUnrelatedRequests(t *testing.T) {
	const host = "127.0.0.1:1234"
	callbacks := make(chan callback, 1)
	handler := callbackHandler(host, "expected", "account", callbacks)
	for _, tc := range []struct {
		method, host, target string
		status               int
	}{
		{"POST", host, "/auth/callback?state=expected", http.StatusNotFound},
		{"GET", "wrong.invalid", "/auth/callback?state=expected", http.StatusNotFound},
		{"GET", host, "/other?state=expected", http.StatusNotFound},
		{"GET", host, "/auth/callback?state=wrong", http.StatusBadRequest},
		{"GET", host, "/auth/callback?state=expected&state=expected", http.StatusBadRequest},
		{"GET", host, "/auth/callback?state=expected&code=one-use", http.StatusOK},
	} {
		r := httptest.NewRequest(tc.method, "http://"+host+tc.target, nil)
		r.Host = tc.host
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s: status %d, want %d", tc.method, tc.target, w.Code, tc.status)
		}
	}
	if len(callbacks) != 1 || (<-callbacks).result.Code != "one-use" {
		t.Fatal("an unrelated request consumed the callback")
	}
}

func TestCallbackPageAndDiagnosticsKeepGrantDataPrivate(t *testing.T) {
	const host, state = "127.0.0.1:1234", "private-state"
	const script = "history.replaceState(null,'','/auth/complete');"
	hash := sha256.Sum256([]byte(script))
	policy := "default-src 'none'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(hash[:]) + "'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	for _, tc := range []struct{ code, category string }{
		{"", ""}, {"access_denied", "access_denied"}, {"server_error", "server_error"},
		{"private-error", "oauth_error"}, {"access_denied\nprivate-error", "oauth_error"},
	} {
		callbacks := make(chan callback, 1)
		query := url.Values{"state": {state}, "code": {"private-code"}, "error_description": {"private-description"}, "error_uri": {"https://private.invalid/"}}
		if tc.code != "" {
			query.Set("error", tc.code)
		}
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/auth/callback?"+query.Encode(), nil)
		w := httptest.NewRecorder()
		callbackHandler(host, state, "account", callbacks)(w, r)
		if w.Code != http.StatusOK || len(callbacks) != 1 {
			t.Fatalf("callback returned %d without completing", w.Code)
		}
		got := <-callbacks
		if tc.category == "" {
			if got.err != nil || got.result.Code != "private-code" {
				t.Fatal("valid callback did not reach the exchange")
			}
		} else if got.err == nil || got.err.Error() != "ChatGPT authorization failed ("+tc.category+")" || got.result != (Result{}) {
			t.Fatal("OAuth error did not stop the exchange with a safe diagnostic")
		}
		if !strings.Contains(w.Body.String(), "<script>"+script+"</script>") || strings.Contains(w.Body.String(), "private") {
			t.Fatal("callback page must clear its URL and keep request data private")
		}
		for key, want := range map[string]string{
			"Content-Type": "text/html; charset=utf-8", "Content-Security-Policy": policy,
			"Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff",
		} {
			if values := w.Header().Values(key); len(values) != 1 || values[0] != want {
				t.Errorf("unexpected callback header %s", key)
			}
		}
		for key, values := range w.Header() {
			if key == "Location" || key == "Set-Cookie" || strings.Contains(strings.Join(values, " "), "private") {
				t.Errorf("unexpected callback response header %s", key)
			}
		}
	}
}

func TestCallbackListenerBoundsRequestIO(t *testing.T) {
	listener, err := Listen("state", "registration")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if listener.server.ReadTimeout <= 0 || listener.server.WriteTimeout <= 0 {
		t.Fatal("callback request reads and response writes require timeouts")
	}
}

func TestCallbackListenerTermination(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{}
	done := serveCallback(server, listener)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || err.Error() != "ChatGPT callback listener stopped before authorization completed" {
			t.Fatal("unexpected listener diagnostic")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("listener termination not reported")
	}
	closeCallback(server)
}

func TestCallbackShutdownIsBounded(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finished := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(finished)
	})}
	serveCallback(server, listener)
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		res, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + listener.Addr().String())
		if err == nil {
			res.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		_ = server.Close()
		t.Fatal("handler did not start")
	}
	begin := time.Now()
	closeCallback(server)
	if time.Since(begin) > 3*time.Second {
		t.Fatal("shutdown exceeded bound")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("fallback close did not cancel active request")
	}
	<-requestDone
}

func TestConcurrentCallbacksAcceptOneResult(t *testing.T) {
	const attempts = 16
	callbacks := make(chan callback, attempts)
	handler := callbackHandler("127.0.0.1:1234", "expected", "account", callbacks)
	statuses := make(chan int, attempts)
	start := make(chan struct{})
	var requests sync.WaitGroup
	for range attempts {
		requests.Go(func() {
			<-start
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1234/auth/callback?state=expected&code=code", nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			statuses <- response.Code
		})
	}
	close(start)
	requests.Wait()
	close(statuses)

	accepted := 0
	for status := range statuses {
		switch status {
		case http.StatusOK:
			accepted++
		case http.StatusBadRequest:
		default:
			t.Fatalf("unexpected callback status %d", status)
		}
	}
	if accepted != 1 || len(callbacks) != 1 {
		t.Fatalf("accepted %d requests and published %d results, want one each", accepted, len(callbacks))
	}
	got := <-callbacks
	if got.err != nil || got.result != (Result{Code: "code", ClientID: "account"}) {
		t.Fatalf("unexpected callback result: %+v", got)
	}
}

package loopback

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

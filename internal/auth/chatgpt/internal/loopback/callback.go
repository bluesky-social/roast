// Package loopback receives a single OAuth callback on a local IPv4 listener.
package loopback

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
)

const callbackPath = "/auth/callback"

type Result struct {
	Code     string
	ClientID string
}

type callback struct {
	result Result
	err    error
}

type Listener struct {
	redirect  string
	server    *http.Server
	stopped   <-chan error
	callbacks <-chan callback
}

func Listen(state, clientID string) (*Listener, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("cannot open ChatGPT loopback callback")
	}
	host := listener.Addr().String()
	callbacks := make(chan callback, 1)
	server := &http.Server{
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		ErrorLog:     log.New(io.Discard, "", 0),
		Handler:      callbackHandler(host, state, clientID, callbacks),
	}
	stopped := serveCallback(server, listener)
	return &Listener{
		redirect:  "http://" + host + callbackPath,
		server:    server,
		stopped:   stopped,
		callbacks: callbacks,
	}, nil
}

func (l *Listener) RedirectURI() string { return l.redirect }

// Wait returns the first callback with matching state. Close the listener when
// sign-in ends so it can reject duplicate callbacks during the exchange.
func (l *Listener) Wait(ctx context.Context) (Result, error) {
	var cb callback
	var stopped error
	select {
	case <-ctx.Done():
		stopped = ctx.Err()
	case stopped = <-l.stopped:
	case cb = <-l.callbacks:
	}
	if stopped != nil {
		// Serve can exit before an accepted handler finishes. Give the handler
		// the bounded shutdown interval to deliver any already-issued registration.
		l.Close()
		select {
		case cb = <-l.callbacks:
		default:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Result{}, ctxErr
			}
			return Result{}, stopped
		}
	}
	if cb.err != nil {
		return Result{}, errors.Join(cb.err, ctx.Err())
	}
	return cb.result, nil
}

func (l *Listener) Close() { closeCallback(l.server) }

func callbackHandler(host, state, clientID string, callbacks chan<- callback) http.HandlerFunc {
	var received atomic.Bool
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		isCallbackRequest := r.Method == http.MethodGet &&
			r.URL.Path == callbackPath && r.Host == host
		if !isCallbackRequest {
			http.NotFound(w, r)
			return
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		queryParsed := err == nil
		hasSingleState := len(values["state"]) == 1
		validState := queryParsed && hasSingleState &&
			subtle.ConstantTimeCompare([]byte(values.Get("state")), []byte(state)) == 1
		if !validState {
			http.Error(w, "Invalid authorization state", http.StatusBadRequest)
			return
		}
		if !received.CompareAndSwap(false, true) {
			http.Error(w, "Authorization already received", http.StatusBadRequest)
			return
		}
		result, err := parseCallback(values, clientID)
		callbacks <- callback{result: result, err: err}
		writeCallbackComplete(w)
	}
}

func parseCallback(values url.Values, clientID string) (Result, error) {
	code, issuedID := values.Get("code"), values.Get("client_id")
	hasOAuthError := values.Has("error")
	hasAuthorizationCode := len(values["code"]) == 1 && code != ""
	hasDuplicateClientID := len(values["client_id"]) > 1
	newRegistration := clientID == ""
	invalidIssuedID := newRegistration && !ValidIssuedClientID(issuedID)
	registrationChanged := !newRegistration && values.Has("client_id") && issuedID != clientID

	switch {
	case hasOAuthError:
		category := "oauth_error"
		if len(values["error"]) == 1 {
			category = oauth.ErrorCategory(values.Get("error"))
		}
		return Result{}, fmt.Errorf("ChatGPT authorization failed (%s)", category)
	case !hasAuthorizationCode || hasDuplicateClientID:
		return Result{}, errors.New("incomplete ChatGPT callback")
	case invalidIssuedID:
		return Result{}, errors.New("missing issued ChatGPT client ID")
	case registrationChanged:
		return Result{}, errors.New("ChatGPT registration changed during sign-in")
	}
	if newRegistration {
		clientID = issuedID
	}
	return Result{Code: code, ClientID: clientID}, nil
}

func serveCallback(server *http.Server, listener net.Listener) <-chan error {
	done := make(chan error, 1)
	go func() {
		_ = server.Serve(listener)
		// Do not expose the underlying network error, which may contain addresses.
		done <- errors.New("ChatGPT callback listener stopped before authorization completed")
	}()
	return done
}

func closeCallback(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if server.Shutdown(ctx) != nil {
		_ = server.Close()
	}
}

// ValidIssuedClientID checks an opaque client ID issued by the provider. The documented example prefix is not a format
// contract; reject only the registration placeholder and malformed values.
func ValidIssuedClientID(id string) bool {
	hasIssuedValue := id != "" && id != oauth.DynamicClientID
	withinLengthLimit := len(id) <= 256
	if !hasIssuedValue || !withinLengthLimit || !utf8.ValidString(id) {
		return false
	}
	return strings.IndexFunc(id, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) < 0
}

// This page never includes callback data or reports sign-in success: token
// exchange and validation finish separately. Its only script replaces the
// callback URL in history without navigating or sending another request.
const callbackCompletePage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Authorization received</title>
<script>history.replaceState(null,'','/auth/complete');</script>
</head>
<body><p>Authorization received. Return to Roast to check the result.</p></body>
</html>
`

// Keep this hash in sync with the exact static script bytes above. No other
// scripts, resources, frames, forms, or base URLs are allowed.
const callbackCompletePolicy = "default-src 'none'; script-src 'sha256-eAngspPXv2k/ukGQyHLbfpsuKrAbDQbH5ffw2WzwDBo='; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func writeCallbackComplete(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", callbackCompletePolicy)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, callbackCompletePage)
}

package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOAuthClientKeepsRedirectPolicy(t *testing.T) {
	client := Client{HTTP: &http.Client{
		Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {Issuer}}}, nil
		}),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			t.Error("OAuth requests must use the fixed redirect policy")
			return http.ErrUseLastResponse
		},
	}}
	if err := client.request(t.Context(), http.MethodGet, jwksURL, nil, "", nil); err == nil {
		t.Fatal("accepted a redirect as a successful OAuth response")
	}
}

func TestOAuthCancellationAfterResponseHeaders(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := NewClient()
	client.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		cancel()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(iotest.ErrReader(context.Canceled)),
		}, nil
	})
	if _, err := client.Refresh(ctx, "client", "refresh"); !errors.Is(err, context.Canceled) {
		t.Fatalf("refresh error = %v, want context cancellation", err)
	}
}

func TestTruncatedResponsePreservesAccessTokenForCleanup(t *testing.T) {
	var tokens TokenResponse
	err := readTokenResponse(strings.NewReader(`{"access_token":"issued-access","expires_in":`), "", &tokens)
	if err == nil || tokens.AccessToken != "issued-access" {
		t.Fatalf("did not reject the response while preserving cleanup evidence: %v", err)
	}
}

func TestInvalidOAuthTokenIsNotCleanupEvidence(t *testing.T) {
	for _, value := range []string{`"\n"`, `"\ud800"`, `" "`} {
		var tokens TokenResponse
		err := readTokenResponse(strings.NewReader(`{"refresh_token":`+value+`,"expires_in":`), "valid", &tokens)
		if err == nil || tokens.RefreshToken != "" {
			t.Fatal("malformed token was retained as cleanup evidence")
		}
	}
}

func TestMalformedBearerTokensAreRejected(t *testing.T) {
	for _, value := range []string{"abc def", "abc=def", "=", "abc?"} {
		var tokens TokenResponse
		err := readTokenResponse(strings.NewReader(`{"access_token":"`+value+`","expires_in":3600,"token_type":"Bearer"}`), "", &tokens)
		if err == nil || tokens.AccessToken != "" {
			t.Fatal("malformed Bearer token was accepted")
		}
	}
}

func TestOAuthErrorsExposeOnlySafeDetails(t *testing.T) {
	for _, tc := range []struct {
		name             string
		code             any
		headers          []string
		wantCode, wantID string
	}{
		{"known", "invalid_grant", []string{"req_12345678abcd"}, "invalid_grant", "req_12345678abcd"},
		{"nested", map[string]any{"code": "invalid_client", "message": "private-message-marker"}, nil, "invalid_client", ""},
		{"unknown", "private-code-marker", []string{"private-header-marker"}, "oauth_error", ""},
		{"unsafe header", "access_denied", []string{"req_12345678\nprivate-marker"}, "access_denied", ""},
		{"URL header", "access_denied", []string{"https://example.invalid/?private-marker"}, "access_denied", ""},
		{"too long", nil, []string{"req_" + strings.Repeat("x", 125)}, "oauth_error", ""},
		{"duplicate", "invalid_grant", []string{"req_12345678abcd", "req_abcdefgh1234"}, "invalid_grant", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient()
			c.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
				body, err := json.Marshal(map[string]any{"error": tc.code, "error_description": "private-description-marker", "request_id": "private-body-marker"})
				if err != nil {
					return nil, err
				}
				r := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}
				for _, id := range tc.headers {
					r.Header.Add("X-Request-Id", id)
				}
				return r, nil
			})
			err := c.request(context.Background(), http.MethodPost, tokenURL, nil, "", nil)
			var oe *oauthError
			if !errors.As(err, &oe) || oe.code != tc.wantCode || oe.requestID != tc.wantID || !errors.Is(err, ErrAuth) {
				t.Fatalf("unexpected safe error: %v", err)
			}
			message := err.Error()
			if strings.Contains(message, "private-") || strings.Contains(message, "https://") || strings.ContainsAny(message, "\r\n") {
				t.Fatal("error exposed provider data")
			}
			if !strings.Contains(message, tc.wantCode) || !strings.Contains(message, "HTTP 400") {
				t.Fatal("safe category or status missing")
			}
			if (tc.wantID != "") != strings.Contains(message, "request ID") {
				t.Fatal("request ID presentation mismatch")
			}
		})
	}
}

func TestOnlyOAuthBadRequestCanInvalidateRefreshGrant(t *testing.T) {
	for _, tc := range []struct {
		status       int
		contentTypes []string
		invalidGrant bool
	}{
		{http.StatusBadRequest, []string{"application/json; charset=UTF-8"}, true},
		{http.StatusBadRequest, []string{"text/plain"}, false},
		{http.StatusBadRequest, nil, false},
		{http.StatusBadRequest, []string{"application/json", "text/html"}, false},
		{http.StatusFound, []string{"application/json"}, false},
		{http.StatusForbidden, []string{"application/json"}, false},
		{http.StatusRequestTimeout, []string{"application/json"}, false},
		{http.StatusTooManyRequests, []string{"application/json"}, false},
	} {
		client := NewClient()
		client.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: tc.status,
				Header:     http.Header{"Content-Type": tc.contentTypes},
				Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)),
			}, nil
		})
		_, err := client.Refresh(t.Context(), "client", "refresh")
		if err == nil || InvalidRefreshGrant(err) != tc.invalidGrant {
			t.Errorf("incorrect grant invalidation for HTTP %d with content types %v: %v", tc.status, tc.contentTypes, err)
		}
	}
}

func TestOnlyExplicitCodeRejectionRulesOutAnIssuedGrant(t *testing.T) {
	for _, tc := range []struct {
		body     string
		rejected bool
	}{
		{`{"error":"invalid_scope"}`, true},
		{`{"error":"invalid_grant"}`, false},
		{`{"error":{"code":"invalid_scope","refresh_token":"echoed"}}`, false},
		{`{"error":"invalid_grant","Refresh_Token":"echoed"}`, false},
		{`{"error":"invalid_grant","id_token":"echoed"}`, false},
		{`{"error":`, false},
	} {
		resp := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.body))}
		if got := DefinitiveGrantRejection(responseError(resp, false)); got != tc.rejected {
			t.Fatalf("DefinitiveGrantRejection(%s) = %v", tc.body, got)
		}
	}
}

func TestModelCatalogResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
		cancel bool
	}{
		{"redirect", http.StatusFound, `{}`, ErrNetwork, false},
		{"missing endpoint", http.StatusNotFound, `{}`, ErrNetwork, false},
		{"invalid request", http.StatusBadRequest, `{"error":"invalid_request"}`, ErrNetwork, false},
		{"invalid access token", http.StatusBadRequest, `{"error":"invalid_token"}`, ErrAuth, false},
		{"unauthorized", http.StatusUnauthorized, `{}`, ErrAuth, false},
		{"forbidden", http.StatusForbidden, `{}`, ErrAuth, false},
		{"malformed JSON", http.StatusOK, `{"models":`, ErrNetwork, false},
		{"missing models", http.StatusOK, `{"error":"unavailable"}`, ErrNetwork, false},
		{"models is not an array", http.StatusOK, `{"models":{}}`, ErrNetwork, false},
		{"misnamed models array", http.StatusOK, `{"MODELS":[]}`, ErrNetwork, false},
		{"duplicate models arrays", http.StatusOK, `{"models":[],"models":[]}`, ErrNetwork, false},
		{"ambiguous models arrays", http.StatusOK, `{"models":[],"MODELS":[]}`, ErrNetwork, false},
		{"null models array", http.StatusOK, `{"models":null}`, ErrNetwork, false},
		{"error alongside models", http.StatusOK, `{"error":"unavailable","models":[]}`, ErrNetwork, false},
		{"mixed-case error alongside models", http.StatusOK, `{"Error":"unavailable","models":[]}`, ErrNetwork, false},
		{"missing listed model slug", http.StatusOK, `{"models":[{"visibility":"list","slug":" "}]}`, ErrNetwork, false},
		{"null catalog entry", http.StatusOK, `{"models":[null]}`, ErrNetwork, false},
		{"empty catalog", http.StatusOK, `{"models":[]}`, nil, false},
		{"canceled", http.StatusOK, `{"models":[]}`, context.Canceled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := NewClient()
			client.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != modelsURL {
					t.Fatalf("unexpected catalog request to %s", r.URL)
				}
				if tc.cancel {
					cancel()
				}
				return &http.Response{
					StatusCode: tc.status,
					Header: http.Header{
						"Content-Type": {"application/json"},
						"Location":     {"https://redirect.invalid"},
					},
					Body: io.NopCloser(strings.NewReader(tc.body)),
				}, nil
			})
			_, err := client.Models(ctx, "synthetic-access-token")
			if !errors.Is(err, tc.want) || (errors.Is(err, ErrAuth) && errors.Is(err, ErrNetwork)) {
				t.Fatalf("catalog error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestOAuthErrorConstructionSanitizesDetails(t *testing.T) {
	// Validate at the output boundary too, not merely during network parsing.
	unsafe := &oauthError{code: "private-code-marker", status: 400, requestID: "private-header-marker"}
	if strings.Contains(unsafe.Error(), "private-") {
		t.Fatal("direct error construction bypassed sanitization")
	}
}

func TestTruncatedOAuthErrorHasGenericCategory(t *testing.T) {
	const raw = `{"code":"invalid_grant"`
	if got := oauthResponseCategory(json.RawMessage(raw)); got != "oauth_error" {
		t.Fatalf("oauthResponseCategory(%s) = %q, want oauth_error", raw, got)
	}
}

func TestRequestIDForError(t *testing.T) {
	for _, tt := range []struct {
		name string
		id   string
		want bool
	}{
		{"empty", "", false},
		{"11 bytes", "req_" + strings.Repeat("a", 7), false},
		{"12 bytes", "req_" + strings.Repeat("a", 8), true},
		{"128 bytes", "req_" + strings.Repeat("a", 124), true},
		{"129 bytes", "req_" + strings.Repeat("a", 125), false},
		{"ASCII letters digits and separators", "req_aZ09_-ab", true},
		{"wrong prefix", "REQ_12345678", false},
		{"unicode", "req_1234567é", false},
		{"space", "req_1234 5678", false},
		{"newline", "req_12345678\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := ""
			if tt.want {
				want = tt.id
			}
			if got := requestIDForError(tt.id); got != want {
				t.Errorf("requestIDForError(%q) = %q, want %q", tt.id, got, want)
			}
			message := (&oauthError{status: http.StatusBadRequest, code: "invalid_grant", requestID: tt.id}).Error()
			if strings.Contains(message, "; request ID ") != tt.want {
				t.Errorf("error presentation bypassed request ID validation: %q", message)
			}
		})
	}
}

func TestRefreshTimestampRepresentations(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      int64
	}{
		{"seconds", `1794182400`, 1794182400},
		{"numeric string", `"1794182400"`, 1794182400},
		{"fraction", `1794182400.25`, 1794182401},
		{"sub-ULP fraction", `1794182400.0000001`, 1794182401},
		{"sub-ULP string", `"1794182400.0000001"`, 1794182401},
		{"long zero fraction", `"1794182400.` + strings.Repeat("0", 1500) + `"`, 1794182400},
		{"long nonzero fraction", `"1794182400.` + strings.Repeat("0", 1500) + `1"`, 1794182401},
		{"leading decimal point", `"+.25E2"`, 25},
		{"trailing decimal point", `"1.e2"`, 100},
		{"multiple decimal points", `"1..1"`, 0},
		{"malformed overflowing exponent", `"1e-999999999x"`, 0},
		{"exponent", `1.7941824e9`, 1794182400},
		{"tiny exponent", `1e-999999999`, 1},
		{"huge exponent", `1e999999999`, 0},
		{"exponent beyond int64", `1e99999999999999999999999`, 0},
		{"negative exponent beyond int64", `1e-99999999999999999999999`, 1},
		{"maximum", `253402300799`, 253402300799},
		{"past maximum fraction", `253402300799.0000000001`, 0},
		{"leading zeros", `"0001794182400.0001"`, 1794182401},
		{"UTC date", `"2026-11-09T00:00:00Z"`, 1794182400},
		{"offset date", `"2026-11-09T02:00:00+02:00"`, 1794182400},
		{"fractional date", `"2026-11-09T00:00:00.000000001Z"`, 1794182401},
		{"sub-nanosecond date", `"2026-11-09T00:00:00.0000000001Z"`, 1794182401},
		{"zero fractional date", `"2026-11-09T00:00:00.0000000000Z"`, 1794182400},
		{"absent", ``, 0},
		{"null", `null`, 0},
		{"empty", `""`, 0},
		{"malformed date", `"not-a-date"`, 0},
		{"object", `{}`, 0},
		{"array", `[]`, 0},
		{"boolean", `true`, 0},
		{"negative", `-10`, 0},
		{"infinity", `"Infinity"`, 0},
		{"NaN", `"NaN"`, 0},
		{"overflow", `1e999`, 0},
		{"huge integer", `9223372036854775807`, 0},
		{"out of range date", `"0000-01-01T00:00:00Z"`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := `{"access_token":"access","refresh_token":"replacement"`
			if tc.raw != "" {
				wire += `,"earliest_refresh_at":` + tc.raw
			}
			wire += "}"
			var token TokenResponse
			if err := readTokenResponse(strings.NewReader(wire), "previous", &token); err != nil {
				t.Fatalf("optional metadata rejected response: %v", err)
			}
			if int64(token.EarliestRefreshAt) != tc.want {
				t.Fatalf("timestamp=%d want=%d", token.EarliestRefreshAt, tc.want)
			}
			stored, err := json.Marshal(token)
			if err != nil {
				t.Fatal(err)
			}
			var roundTrip TokenResponse
			if err := json.Unmarshal(stored, &roundTrip); err != nil || roundTrip.EarliestRefreshAt != token.EarliestRefreshAt {
				t.Fatal("normalized timestamp failed durable round trip")
			}
		})
	}
}

func TestRefreshTimestampInvalidHintClearsPreviousValue(t *testing.T) {
	var previous RefreshTimestamp = 42
	if err := json.Unmarshal([]byte(`"invalid"`), &previous); err != nil || previous != 0 {
		t.Fatal("invalid hint retained stale scheduling metadata")
	}
}

func TestAmbiguousRefreshHintPreservesReplacement(t *testing.T) {
	for _, extension := range []string{
		`"Earliest_Refresh_At":42`,
		`"earliest_refresh_at":42,"earliest_refresh_at":43`,
		`"earliest_refresh_at":42,"Earliest_Refresh_At":43`,
		`"Earliest_Refresh_At":42,"Earliest_Refresh_At":43`,
	} {
		wire := `{"access_token":"access","refresh_token":"replacement",` + extension + `}`
		var tokens TokenResponse
		if err := readTokenResponse(strings.NewReader(wire), "previous", &tokens); err != nil ||
			tokens.RefreshToken != "replacement" || tokens.EarliestRefreshAt != 0 {
			t.Fatalf("optional hint %s affected the grant: %v", extension, err)
		}
	}
}

func TestOptionalMetadataDoesNotRelaxCredentialParsing(t *testing.T) {
	for _, tc := range []struct{ name, wire string }{
		{"numeric access token", `{"access_token":123,"earliest_refresh_at":"invalid"}`},
		{"object refresh token", `{"refresh_token":{},"earliest_refresh_at":null}`},
		{"array scope", `{"scope":[],"earliest_refresh_at":"invalid"}`},
		{"string expiration", `{"expires_in":"3600","earliest_refresh_at":"invalid"}`},
		{"truncated JSON", `{"earliest_refresh_at":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var token TokenResponse
			if err := readTokenResponse(strings.NewReader(tc.wire), "previous", &token); err == nil {
				t.Fatal("invalid credential response accepted")
			}
		})
	}
}

func TestDiagnosticTokenResponseDoesNotSupplyCredentials(t *testing.T) {
	for _, body := range []string{`{"error":"temporarily_unavailable","refresh_token":"echo"}`, `{"refresh_token":"echo","error":`, `{"refresh_token":"echo","error_description":"please retry"}`} {
		var tokens TokenResponse
		err := readTokenResponse(strings.NewReader(body), "previous", &tokens)
		if err == nil || tokens.RefreshToken != "" || tokens.AccessToken != "" {
			t.Fatal("OAuth error supplied a cleanup token")
		}
	}
}

func TestConflictingTokensCannotConfirmRevocation(t *testing.T) {
	for _, body := range []string{
		`{"refresh_token":"one","refresh_token":"two"}`,
		`{"refresh_token":"one","refresh_token":"two","refresh_token":"one"}`,
		`{"Refresh_Token":"alias","refresh_token":"rotated"}`,
		`{"refresh_token":"rotated","Refresh_Token":"alias"}`,
		`{"access_token":"one","access_token":"two"}`,
	} {
		var tokens TokenResponse
		if err := readTokenResponse(strings.NewReader(body), "previous", &tokens); err == nil || tokens.RefreshToken != "" || tokens.AccessToken != "" {
			t.Fatal("conflicting tokens produced a cleanup candidate")
		}
	}
}

func TestUnusableReplacementPreservesOnlyAKnownAccessTokenForCleanup(t *testing.T) {
	for _, body := range []string{
		`{"access_token":"known","refresh_token":"first","refresh_token":"second"}`,
		`{"access_token":"known","refresh_token":"bad\ntoken"}`,
		`{"access_token":"known","refresh_token":17}`,
		`{"access_token":"known","refresh_token":{}}`,
	} {
		var tokens TokenResponse
		err := readTokenResponse(strings.NewReader(body), "previous", &tokens)
		if !errors.Is(err, ErrAmbiguousRotation) || tokens.AccessToken != "known" || tokens.RefreshToken != "" {
			t.Fatal("failed to distinguish the known access token from an unusable replacement")
		}
	}
}

func TestMisnamedRotatedTokenIsOnlyReturnedForCleanup(t *testing.T) {
	for _, body := range []string{
		`{"Refresh_Token":"rotated","token_type":"Bearer"}`,
		`{"Refresh_Token":"rotated","refresh_token":"rotated"}`,
		`{"refresh_token":"rotated","Refresh_Token":"previous"}`,
	} {
		var tokens TokenResponse
		err := readTokenResponse(strings.NewReader(body), "previous", &tokens)
		if err == nil || tokens.RefreshToken != "rotated" {
			t.Fatalf("malformed rotation did not return its cleanup token: %v", err)
		}
	}
}

func TestUnexpectedSuccessfulStatusOnlyReturnsCleanupEvidence(t *testing.T) {
	c := NewClient()
	c.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		body := io.NopCloser(strings.NewReader(`{"refresh_token":"replacement","expires_in":"broken"}`))
		return &http.Response{StatusCode: http.StatusCreated, Header: make(http.Header), Body: body}, nil
	})
	tokens, err := c.Refresh(t.Context(), "registered", "previous")
	if !errors.Is(err, ErrNetwork) || tokens.RefreshToken != "replacement" {
		t.Fatalf("unexpected success was accepted or lost the replacement: %v", err)
	}
}

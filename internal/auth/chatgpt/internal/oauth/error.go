package oauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"strings"

	josejson "github.com/go-jose/go-jose/v4/json"
)

// ErrorCategory returns a fixed category safe to show to the user. Provider
// descriptions and arbitrary codes can contain authorization data.
func ErrorCategory(code string) string {
	switch code {
	case "access_denied", "invalid_request", "invalid_client", "unauthorized_client",
		"invalid_scope", "invalid_resource", "unsupported_grant_type", "unsupported_response_type",
		"invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired",
		"refresh_token_invalidated", "refresh_token_reused", "invalid_token", "invalid_api_key",
		"server_error", "temporarily_unavailable", "rate_limit_exceeded":
		return code
	default:
		return "oauth_error"
	}
}

func oauthResponseCategory(raw json.RawMessage) string {
	var code string
	if json.Unmarshal(raw, &code) != nil {
		if json.Unmarshal(errorField(raw, "code"), &code) != nil {
			return "oauth_error"
		}
	}
	return ErrorCategory(code)
}

// A terminal error can clear credentials. Use go-jose's strict decoder to
// reject duplicate names, and ignore mixed grant/error responses.
func errorField(raw json.RawMessage, name string) json.RawMessage {
	var fields map[string]json.RawMessage
	if josejson.Unmarshal(raw, &fields) != nil {
		return nil
	}
	for key := range fields {
		if (key != name && strings.EqualFold(key, name)) || strings.EqualFold(key, "access_token") ||
			strings.EqualFold(key, "refresh_token") || strings.EqualFold(key, "id_token") {
			return nil
		}
	}
	return fields[name]
}

var requestIDPattern = regexp.MustCompile(`^req_[A-Za-z0-9_-]{8,124}$`)

// requestIDForError omits unfamiliar header values instead of echoing provider data.
func requestIDForError(id string) string {
	if !requestIDPattern.MatchString(id) {
		return ""
	}
	return id
}

var (
	ErrAuth    = errors.New("ChatGPT authentication required")
	ErrNetwork = errors.New("ChatGPT service unavailable")
)

type oauthError struct {
	code      string
	status    int
	requestID string
	catalog   bool
}

func responseError(resp *http.Response, catalog bool) error {
	failure := &oauthError{status: resp.StatusCode, code: "oauth_error", catalog: catalog}
	var body json.RawMessage
	if contentTypes := resp.Header.Values("Content-Type"); len(contentTypes) == 1 {
		mediaType, _, err := mime.ParseMediaType(contentTypes[0])
		if err == nil && mediaType == "application/json" && readJSON(resp.Body, 1<<20, &body) == nil {
			failure.code = oauthResponseCategory(errorField(body, "error"))
		}
	}
	if values := resp.Header.Values("X-Request-Id"); len(values) == 1 {
		failure.requestID = requestIDForError(values[0])
	}
	return failure
}

func (e *oauthError) Is(target error) bool {
	if e.catalog {
		switch e.status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return target == ErrAuth
		case http.StatusBadRequest:
			if e.code == "invalid_token" || e.code == "token_expired" {
				return target == ErrAuth
			}
		}
		return target == ErrNetwork
	}
	if e.status >= http.StatusInternalServerError || e.status == http.StatusTooManyRequests || e.status == http.StatusRequestTimeout {
		return target == ErrNetwork
	}
	return target == ErrAuth
}

func (e *oauthError) Error() string {
	request := "ChatGPT OAuth request"
	if e.catalog {
		request = "ChatGPT model catalog request"
	}
	message := fmt.Sprintf("%s failed (HTTP %d, %s)", request, e.status, ErrorCategory(e.code))
	if requestID := requestIDForError(e.requestID); requestID != "" {
		message += "; request ID " + requestID
	}
	if !e.catalog && errors.Is(e, ErrNetwork) {
		return message + "; retry later"
	}
	return message
}

// DefinitiveGrantRejection reports an explicit client error that rules out issuance.
// invalid_grant can mean a one-use authorization code was already consumed.
func DefinitiveGrantRejection(err error) bool {
	var failure *oauthError
	if !errors.As(err, &failure) || failure.catalog ||
		(failure.status != http.StatusBadRequest && failure.status != http.StatusUnauthorized && failure.status != http.StatusForbidden) {
		return false
	}
	switch failure.code {
	case "access_denied", "invalid_request", "invalid_client", "unauthorized_client", "invalid_scope",
		"invalid_resource", "unsupported_grant_type":
		return true
	}
	return false
}

func InvalidRefreshGrant(err error) bool {
	var failure *oauthError
	if !errors.As(err, &failure) || failure.status != http.StatusBadRequest {
		return false
	}
	switch failure.code {
	case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused":
		return true
	default:
		return false
	}
}

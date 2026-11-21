//go:build darwin || linux

package chatgpt

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestNewReportsUnavailableKeyring(t *testing.T) {
	mockKeyring(t)
	keyring.MockInitWithError(errors.New("test backend failure"))
	if _, err := New(t.Context()); err == nil || !strings.Contains(err.Error(), "system keyring") {
		t.Fatalf("New on an unavailable keyring = %v", err)
	}
}

func TestSignInPersistsStableHostBeforeAuthorization(t *testing.T) {
	c := testClient(t)
	uuid := regexp.MustCompile("^urn:uuid:[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$")
	stop := errors.New("stop before browser authorization")
	var initial string
	for range 2 {
		err := c.SignIn(context.Background(), "", func(raw string) error {
			u, err := url.Parse(raw)
			if err != nil {
				return err
			}
			hostID := u.Query().Get("ext_agent_host_id")
			accounts, err := c.loadStore(t.Context())
			if err != nil || accounts.HostID != hostID || !uuid.MatchString(hostID) || len(accounts.Accounts) != 0 {
				t.Fatalf("sign-in did not prepare a stable host: %v", err)
			}
			if initial != "" && initial != hostID {
				t.Fatal("subsequent sign-in changed the host identity")
			}
			initial = hostID
			return stop
		})
		if !errors.Is(err, stop) {
			t.Fatalf("sign-in did not reach browser authorization: %v", err)
		}
	}
}

package middleware

import "testing"

// The login cookie is named after the product; the browser keeps it under
// this name, so a rename signs everyone out once.
func TestSessionCookieNames(t *testing.T) {
	if SessionCookieSecure != "__Host-agency_session" || SessionCookieInsecure != "agency_session" {
		t.Errorf("cookie names = %q, %q, want __Host-agency_session, agency_session", SessionCookieSecure, SessionCookieInsecure)
	}
}

package inventory

import "testing"

func TestParseDefaults(t *testing.T) {
	defaults, err := ParseDefaults("_obmc_console._tcp=root:0penBmc,_redfish._tcp=admin:pa:ss")
	if err != nil {
		t.Fatalf("ParseDefaults: %v", err)
	}
	if got := defaults["_obmc_console._tcp"]; got != (Credentials{Username: "root", Password: "0penBmc"}) {
		t.Errorf("console creds = %+v", got)
	}

	wild, err := ParseDefaults("*=admin:admin")
	if err != nil {
		t.Fatalf("wildcard entry: %v", err)
	}
	if got := wild[WildcardService]; got != (Credentials{Username: "admin", Password: "admin"}) {
		t.Errorf("wildcard creds = %+v", got)
	}
	// The password may contain colons; only the first splits user from pass.
	if got := defaults["_redfish._tcp"]; got != (Credentials{Username: "admin", Password: "pa:ss"}) {
		t.Errorf("redfish creds = %+v", got)
	}

	if defaults, err := ParseDefaults(""); err != nil || len(defaults) != 0 {
		t.Errorf("empty input: defaults=%v err=%v", defaults, err)
	}

	for _, bad := range []string{"no-equals", "svc=nopassword", "=root:pw", "svc=:pw", "svc=root:"} {
		if _, err := ParseDefaults(bad); err == nil {
			t.Errorf("ParseDefaults(%q) should fail", bad)
		}
	}
}

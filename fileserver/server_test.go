package silod

import (
	"net/http/httptest"
	"testing"

	"github.com/dkam/silo/fileserver/option"
)

func TestProfilingAuthorized(t *testing.T) {
	origEnabled := option.EnableProfiling
	origPassword := option.ProfilePassword
	t.Cleanup(func() {
		option.EnableProfiling = origEnabled
		option.ProfilePassword = origPassword
	})

	cases := []struct {
		name       string
		enabled    bool
		configured string
		supplied   string
		want       bool
	}{
		{"correct password", true, "s3cret", "s3cret", true},
		{"wrong password", true, "s3cret", "guess", false},
		{"prefix of password", true, "s3cret", "s3c", false},
		{"password with correct length", true, "s3cret", "aaaaaa", false},
		{"no password supplied", true, "s3cret", "", false},
		// option.go fatals when profile_password is absent, but a present and
		// empty value would otherwise let anyone through.
		{"configured password empty", true, "", "", false},
		{"configured password empty, guess supplied", true, "", "anything", false},
		// Profiling off is a hard no regardless of the password.
		{"profiling disabled", false, "s3cret", "s3cret", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			option.EnableProfiling = c.enabled
			option.ProfilePassword = c.configured

			r := httptest.NewRequest("GET", "/debug/pprof/?password="+c.supplied, nil)
			if got := profilingAuthorized(r); got != c.want {
				t.Errorf("profilingAuthorized() = %v, want %v", got, c.want)
			}
		})
	}
}

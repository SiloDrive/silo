package middleware

import "testing"

func TestRouteName(t *testing.T) {
	tests := []struct {
		name     string
		template string
		want     string
	}{
		{
			"regex-pinned variable and optional slash",
			`/repo/{repoid:[\da-z]{8}-[\da-z]{4}-[\da-z]{4}-[\da-z]{4}-[\da-z]{12}}/permission-check{slash:\/?}`,
			"/repo/{repoid}/permission-check",
		},
		{
			"two pinned variables",
			`/repo/{repoid:[\da-z]{8}-[\da-z]{4}-[\da-z]{4}-[\da-z]{4}-[\da-z]{12}}/block/{id:[\da-z]{40}}`,
			"/repo/{repoid}/block/{id}",
		},
		{
			"plain variable is left alone",
			"/api/silo/v1/repos/{repoid}/dir/",
			"/api/silo/v1/repos/{repoid}/dir/",
		},
		{
			"no variables",
			"/api/silo/v1/auth/login",
			"/api/silo/v1/auth/login",
		},
		{
			"slash variable at the end of a bare route",
			`/repo/head-commits-multi{slash:\/?}`,
			"/repo/head-commits-multi",
		},
		{
			"anonymous wildcard",
			"/files/{.*}/{.*}",
			"/files/{.*}/{.*}",
		},
		// A template that cannot be parsed must come back whole. Truncating it
		// would produce a name that reads like a real, shorter route.
		{
			"unbalanced brace",
			"/repo/{repoid",
			"/repo/{repoid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := routeName(tt.template); got != tt.want {
				t.Errorf("routeName(%q) =\n %q\nwant\n %q", tt.template, got, tt.want)
			}
		})
	}
}

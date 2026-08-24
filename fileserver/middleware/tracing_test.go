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
			`/libraries/{libraryid:[\da-z]{8}-[\da-z]{4}-[\da-z]{4}-[\da-z]{4}-[\da-z]{12}}/permission-check{slash:\/?}`,
			"/libraries/{libraryid}/permission-check",
		},
		{
			"two pinned variables",
			`/libraries/{libraryid:[\da-z]{8}-[\da-z]{4}-[\da-z]{4}-[\da-z]{4}-[\da-z]{12}}/block/{id:[\da-z]{40}}`,
			"/libraries/{libraryid}/block/{id}",
		},
		{
			"plain variable is left alone",
			"/api/silo/v1/libraries/{libraryid}/dir/",
			"/api/silo/v1/libraries/{libraryid}/dir/",
		},
		{
			"no variables",
			"/api/silo/v1/auth/login",
			"/api/silo/v1/auth/login",
		},
		{
			"slash variable at the end of a bare route",
			`/libraries/head-commits-multi{slash:\/?}`,
			"/libraries/head-commits-multi",
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
			"/libraries/{libraryid",
			"/libraries/{libraryid",
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

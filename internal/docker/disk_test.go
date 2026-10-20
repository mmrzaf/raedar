package docker

import (
	"errors"
	"testing"
)

func TestIsDiskUsageForbidden(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"other 403", errors.New("docker: GET /v1.46/containers/abc/json: 403 Forbidden"), false},
		{"system/df 403", errors.New("docker: GET /v1.46/system/df: 403 Forbidden"), true},
		{"system/df 404", errors.New("docker: GET /v1.46/system/df: 404 Not Found"), true},
		{"system/df 405", errors.New("docker: GET /v1.46/system/df: 405 Method Not Allowed"), true},
		{"system/df with message", errors.New(`docker: GET /v1.46/system/df: 403 Forbidden: <html>denied</html>`), true},
		{"unrelated", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsDiskUsageForbidden(tc.err); got != tc.want {
				t.Fatalf("IsDiskUsageForbidden(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

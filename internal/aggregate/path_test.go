package aggregate

import "testing"

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "root", path: "/", want: "/"},
		{name: "empty", path: "", want: "/"},
		{name: "plain", path: "/api/orders", want: "/api/orders"},
		{name: "trailing slash", path: "/api/orders/", want: "/api/orders/"},
		{name: "digits", path: "/api/orders/123", want: "/api/orders/:n"},
		{name: "zero", path: "/api/orders/0", want: "/api/orders/:n"},
		{name: "query string removed", path: "/api/orders/123?page=2&sort=asc", want: "/api/orders/:n"},
		{name: "fragment removed", path: "/api/orders/123#items", want: "/api/orders/:n"},
		{name: "query without path", path: "/?a=1", want: "/"},
		{
			name: "uuid",
			path: "/users/9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a",
			want: "/users/:uuid",
		},
		{
			name: "uppercase uuid",
			path: "/users/9F8E7D6C-5B4A-4392-8170-6F5E4D3C2B1A",
			want: "/users/:uuid",
		},
		{name: "hexadecimal", path: "/objects/deadbeefdeadbeefdeadbeefdeadbeef", want: "/objects/:hex"},
		{name: "shortest hexadecimal", path: "/objects/0123456789abcdef", want: "/objects/:hex"},
		{name: "fifteen characters are not hexadecimal", path: "/objects/0123456789abcde", want: "/objects/0123456789abcde"},
		{name: "token", path: "/share/abc123def456ghi789jkl012", want: "/share/:token"},
		{name: "token with dashes", path: "/share/abc123def456-ghi789jkl012", want: "/share/:token"},
		{name: "letters only", path: "/share/abcdefghijklmnopqrstuvwx", want: "/share/abcdefghijklmnopqrstuvwx"},
		{name: "long digits stay numeric", path: "/share/123456789012345678901234", want: "/share/:n"},
		{name: "script", path: "/assets/app.4f3a2b.js", want: "/assets/*.js"},
		{name: "module", path: "/assets/app.mjs", want: "/assets/*.mjs"},
		{name: "stylesheet", path: "/assets/style.css", want: "/assets/*.css"},
		{name: "source map", path: "/assets/style.css.map", want: "/assets/*.map"},
		{name: "image", path: "/images/logo.2x.png", want: "/images/*.png"},
		{name: "icon", path: "/favicon.ico", want: "/*.ico"},
		{name: "font", path: "/fonts/inter.woff2", want: "/fonts/*.woff2"},
		{name: "uppercase extension", path: "/assets/app.JS", want: "/assets/*.js"},
		{name: "html is kept", path: "/docs/readme.html", want: "/docs/readme.html"},
		{name: "extension is not the last segment", path: "/app.js/routes", want: "/app.js/routes"},
		{name: "six segments", path: "/a/b/c/d/e/f", want: "/a/b/c/d/e/f"},
		{name: "seven segments", path: "/a/b/c/d/e/f/g", want: "/a/b/c/d/e/f/…"},
		{name: "many segments", path: "/a/b/c/d/e/f/g/h/i", want: "/a/b/c/d/e/f/…"},
		{name: "deep path with an extension", path: "/a/b/c/d/e/f/g.js", want: "/a/b/c/d/e/f/…"},
		{name: "identifiers are replaced before truncation", path: "/a/b/c/d/e/123456/789", want: "/a/b/c/d/e/:n/…"},
		{name: "empty segments are kept", path: "/a//b", want: "/a//b"},
		{name: "no leading slash", path: "api/orders/1", want: "/api/orders/:n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizePath(tt.path); got != tt.want {
				t.Errorf("NormalizePath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

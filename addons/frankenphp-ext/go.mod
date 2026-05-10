// FrankenPHP-extension addon for scopecache.
//
// Lives in its own Go module so the scopecache core stays stdlib-only
// (same pattern caddymodule/ uses). The extension wraps *Gateway in
// PHP-callable functions exposed via FrankenPHP's extension generator.
//
// Build: see build.sh in this directory. Requires the FrankenPHP
// builder image (or a local FrankenPHP source checkout) plus xcaddy.

module github.com/VeloxCoding/scopecache/addons/frankenphp-ext

go 1.23

require (
	github.com/VeloxCoding/scopecache v0.8.22
	github.com/dunglas/frankenphp v1.6.0
)

// Local development: when iterating against an unreleased scopecache
// change, uncomment the next line and adjust the path. CI / consumer
// builds resolve through the require above + go.sum.
//
// replace github.com/VeloxCoding/scopecache => ../..

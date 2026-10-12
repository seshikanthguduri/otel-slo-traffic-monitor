// Package version is stamped at build time via -ldflags.
package version

// Version is overridden with -ldflags "-X .../internal/version.Version=v1.2.3".
var Version = "dev"

// Package env holds tiny helpers for 12-factor configuration.
package env

import (
	"os"
	"strconv"
	"time"
)

// String returns the variable or a default.
func String(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Float returns the variable parsed as float64 or a default.
func Float(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return def
}

// Int returns the variable parsed as int or a default.
func Int(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

// Duration returns the variable parsed as time.Duration or a default.
func Duration(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

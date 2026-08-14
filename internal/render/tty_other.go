//go:build !linux && !darwin

package render

// ttyWidth has no portable implementation here, so callers fall back to their
// default width. Windows is served via WSL for now; see the README.
func ttyWidth() int { return 0 }

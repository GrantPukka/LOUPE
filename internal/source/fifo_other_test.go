//go:build !unix

package source

import "errors"

// makeFIFO has no portable equivalent, so the test that needs one skips.
func makeFIFO(string) error { return errors.New("named pipes are not available here") }

//go:build !unix

package main

// setUmask does nothing where there is no umask.
func setUmask() {}
